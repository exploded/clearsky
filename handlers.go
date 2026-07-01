package main

import (
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"clearsky/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

const pageSize = 60

// App holds the dependencies the HTTP handlers need.
type App struct {
	q       *store.Queries
	runner  *Runner
	loc     *time.Location
	tmpl    *template.Template
	tonight TonightPanel
}

// TonightPanel holds the external "eyeball" images embedded at the top of the page.
type TonightPanel struct {
	SourceMode      string
	ClearOutsideImg string
	YrMeteogramURL  string
	SkippyImg       string
	DeepSpaceURL    string
}

// NewApp builds the app and parses the embedded templates.
func NewApp(q *store.Queries, runner *Runner, loc *time.Location, cfg Config) (*App, error) {
	tmpl, err := template.New("").Funcs(template.FuncMap{}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{
		q: q, runner: runner, loc: loc, tmpl: tmpl,
		tonight: TonightPanel{
			SourceMode:      cfg.Source,
			ClearOutsideImg: cfg.ClearOutsideImg,
			YrMeteogramURL:  cfg.YrMeteogramURL,
			SkippyImg:       cfg.SkippyImg,
			DeepSpaceURL:    cfg.DeepSpaceURL,
		},
	}, nil
}

// Routes wires the router. Stdlib http.ServeMux with method+path patterns.
func (a *App) Routes() http.Handler {
	mux := http.NewServeMux()

	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /nights/rows", a.handleRows)
	mux.HandleFunc("POST /run", a.handleRun)

	// Later seam — stubbed in v1. The DB columns and queries already exist, so these
	// become working endpoints with a handler body and (for result) a UI form.
	mux.HandleFunc("POST /nights/{date}/result", a.handleResultStub)
	mux.HandleFunc("POST /webhooks/nina", a.handleNinaStub)

	return securityHeaders(mux)
}

// pageData is the template root for the page and the rows fragment. Tonight is set
// only for the full page (nil on the rows fragment).
type pageData struct {
	Title      string
	Nights     []NightView
	NextBefore string
	Tonight    *TonightPanel
}

// NightView is a template-ready projection of a store.Night.
type NightView struct {
	NightDate     string
	DateLabel     string
	GO            bool
	DecisionLabel string
	Score         int64
	Reason        string
	Source        string
	CloudAvg      int
	CloudMax      int
	RainMm        float64
	RainProb      int
	MoonPct       int
	Dusk          string
	Dawn          string
	Imaged        string
	ImageURL      string
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	nights, err := a.q.ListNights(r.Context(), store.ListNightsParams{Limit: pageSize, Offset: 0})
	if err != nil {
		a.serverError(w, err)
		return
	}
	data := a.buildPageData(nights)
	data.Title = "clearsky — astrophotography nights"
	data.Tonight = &a.tonight
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, "base", data); err != nil {
		slog.Error("render index", "err", err)
	}
}

func (a *App) handleRows(w http.ResponseWriter, r *http.Request) {
	before := r.URL.Query().Get("before")
	if before == "" {
		http.Error(w, "missing before cursor", http.StatusBadRequest)
		return
	}
	nights, err := a.q.ListNightsBefore(r.Context(), store.ListNightsBeforeParams{NightDate: before, Limit: pageSize})
	if err != nil {
		a.serverError(w, err)
		return
	}
	data := a.buildPageData(nights)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmpl.ExecuteTemplate(w, "rows", data); err != nil {
		slog.Error("render rows", "err", err)
	}
}

func (a *App) handleRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, err := a.runner.RunForDate(ctx, time.Now().In(a.loc)); err != nil {
		a.serverError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleResultStub / handleNinaStub — later-seam placeholders (see plan). Returning
// 501 keeps the routes registered and self-documenting without shipping the feature.
func (a *App) handleResultStub(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "results logging not implemented yet (v1 seam)", http.StatusNotImplemented)
}

func (a *App) handleNinaStub(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "NINA ingest not implemented yet (v1 seam)", http.StatusNotImplemented)
}

// buildPageData projects rows to views and computes the next pagination cursor.
func (a *App) buildPageData(nights []store.Night) pageData {
	views := make([]NightView, 0, len(nights))
	for _, n := range nights {
		views = append(views, a.toView(n))
	}
	next := ""
	if len(nights) == pageSize {
		next = nights[len(nights)-1].NightDate // oldest in this batch
	}
	return pageData{Nights: views, NextBefore: next}
}

func (a *App) toView(n store.Night) NightView {
	var cloud CloudSummary
	var rain RainSummary
	_ = json.Unmarshal([]byte(n.CloudSummary), &cloud)
	_ = json.Unmarshal([]byte(n.RainSummary), &rain)

	label := n.NightDate
	if d, err := time.ParseInLocation("2006-01-02", n.NightDate, a.loc); err == nil {
		label = d.Format("Mon 2 Jan 2006")
	}

	imaged := "—"
	if n.Imaged.Valid {
		if n.Imaged.Int64 == 1 {
			imaged = "yes"
		} else {
			imaged = "no"
		}
	}

	return NightView{
		NightDate:     n.NightDate,
		DateLabel:     label,
		GO:            n.Decision == "GO",
		DecisionLabel: decisionLabel(n.Decision),
		Score:         n.Score,
		Reason:        n.Reason,
		Source:        n.Source,
		CloudAvg:      cloud.Avg,
		CloudMax:      cloud.Max,
		RainMm:        rain.TotalMm,
		RainProb:      rain.MaxProbPct,
		MoonPct:       int(n.MoonIllumPct + 0.5),
		Dusk:          unixHM(n.DuskAt, a.loc),
		Dawn:          unixHMDay(n.DawnAt, a.loc),
		Imaged:        imaged,
		ImageURL:      n.ImageUrl,
	}
}

func (a *App) serverError(w http.ResponseWriter, err error) {
	slog.Error("server error", "err", err)
	http.Error(w, "Something went wrong.", http.StatusInternalServerError)
}

func decisionLabel(code string) string {
	if code == "GO" {
		return "GO"
	}
	return "NO-GO"
}

func unixHM(u int64, loc *time.Location) string {
	if u == 0 {
		return "—"
	}
	return time.Unix(u, 0).In(loc).Format("15:04")
}

func unixHMDay(u int64, loc *time.Location) string {
	if u == 0 {
		return "—"
	}
	return time.Unix(u, 0).In(loc).Format("Mon 15:04")
}
