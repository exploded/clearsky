package main

import (
	"bytes"
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// clearHours builds n consecutive imageable hours from start at the given cloud level.
func clearHours(start time.Time, n, cloud int) []HourlyPoint {
	out := make([]HourlyPoint, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, HourlyPoint{At: start.Add(time.Duration(i) * time.Hour), CloudTotal: cloud})
	}
	return out
}

// TestSummarizeAgreementSplit is the 2026-08-03 failure in miniature: one optimistic
// model finds a usable window, two others call the same hours overcast. The merged
// decision is correctly NO-GO, and the breakdown must record 1-of-3 rather than
// leaving the disagreement invisible.
func TestSummarizeAgreementSplit(t *testing.T) {
	loc := mustMelbourne(t)
	dusk := time.Date(2026, 8, 3, 19, 0, 0, 0, loc)
	dark := Darkness{Dusk: dusk, Dawn: dusk.Add(6 * time.Hour)}
	th := FromEnv().Thresholds

	optimist := stubSource{name: "ecmwf", hours: clearHours(dusk, 5, 6)}
	pessimist1 := stubSource{name: "gfs", hours: clearHours(dusk, 5, 95)}
	pessimist2 := stubSource{name: "icon", hours: clearHours(dusk, 5, 88)}

	fc, err := NewMultiSource(optimist, pessimist1, pessimist2).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	fc = fc.InLocation(loc)

	if got := Evaluate(fc.HoursWithin(dark.Dusk, dark.Dawn), dark, th); got.GO {
		t.Fatalf("merged decision should be NO-GO, got GO: %s", got.Reason)
	}

	ag := summarizeAgreement(fc, dark, th)
	if len(ag.Sources) != 3 {
		t.Fatalf("expected 3 source verdicts, got %d", len(ag.Sources))
	}
	if ag.GoCount != 1 {
		t.Errorf("GoCount = %d, want 1 (only the optimist)", ag.GoCount)
	}
	if ag.Unanimous() {
		t.Error("Unanimous() = true, want false — the sources split 1/3")
	}
	if ag.Spread != 89 { // 95 - 6
		t.Errorf("Spread = %d, want 89", ag.Spread)
	}
	if ag.Label() != "1/3 agree · spread 89%" {
		t.Errorf("Label() = %q", ag.Label())
	}

	// The optimist must be the one recorded as GO.
	for _, s := range ag.Sources {
		if (s.Name == "ecmwf") != s.GO {
			t.Errorf("source %q: GO = %v, want %v", s.Name, s.GO, s.Name == "ecmwf")
		}
	}
}

// A unanimous night must not be flagged as split — otherwise the amber warning on the
// log page means nothing.
func TestSummarizeAgreementUnanimous(t *testing.T) {
	loc := mustMelbourne(t)
	dusk := time.Date(2026, 8, 3, 19, 0, 0, 0, loc)
	dark := Darkness{Dusk: dusk, Dawn: dusk.Add(6 * time.Hour)}
	th := FromEnv().Thresholds

	fc, err := NewMultiSource(
		stubSource{name: "ecmwf", hours: clearHours(dusk, 5, 5)},
		stubSource{name: "gfs", hours: clearHours(dusk, 5, 9)},
		stubSource{name: "icon", hours: clearHours(dusk, 5, 12)},
	).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	ag := summarizeAgreement(fc.InLocation(loc), dark, th)
	if ag.GoCount != 3 || !ag.Unanimous() {
		t.Errorf("GoCount = %d, Unanimous = %v; want 3 and true", ag.GoCount, ag.Unanimous())
	}
	if ag.Spread != 7 { // 12 - 5
		t.Errorf("Spread = %d, want 7", ag.Spread)
	}
}

// A single provider has nobody to agree with; the breakdown stays empty so the log
// page falls back to the bare source name.
func TestSummarizeAgreementSingleSource(t *testing.T) {
	loc := mustMelbourne(t)
	dusk := time.Date(2026, 8, 3, 19, 0, 0, 0, loc)
	dark := Darkness{Dusk: dusk, Dawn: dusk.Add(6 * time.Hour)}

	fc := Forecast{Source: "ecmwf", Hours: clearHours(dusk, 5, 5)}
	if ag := summarizeAgreement(fc, dark, FromEnv().Thresholds); len(ag.Sources) != 0 {
		t.Errorf("expected no breakdown for a single source, got %d", len(ag.Sources))
	}
}

// InLocation must restamp members too. A member left in UTC slices the wrong hours out
// of the darkness window and would report "no usable hours" for a clear night.
func TestInLocationRestampsMembers(t *testing.T) {
	loc := mustMelbourne(t)
	dusk := time.Date(2026, 8, 3, 19, 0, 0, 0, loc)
	fc := Forecast{
		Source:  "merged",
		Hours:   clearHours(dusk, 3, 5),
		Members: []Forecast{{Source: "a", Hours: clearHours(dusk.UTC(), 3, 5)}},
	}
	got := fc.InLocation(loc)
	if len(got.Members) != 1 {
		t.Fatalf("members dropped: %d", len(got.Members))
	}
	for _, h := range got.Members[0].Hours {
		if h.At.Location().String() != loc.String() {
			t.Errorf("member hour %v not restamped to site tz", h.At)
		}
	}
}

// A model Open-Meteo serves as all-nulls (ACCESS-G does exactly this) must be an
// error, never a flawlessly clear night. This is the difference between a failed
// fetch and a false GO.
func TestOpenMeteoRejectsNullCloud(t *testing.T) {
	body := map[string]any{"hourly": map[string]any{
		"time":        []string{"2026-08-03T19:00", "2026-08-03T20:00"},
		"cloud_cover": []any{nil, nil},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	om := NewOpenMeteoModel("Australia/Melbourne", "bom_access_global", "access-g")
	om.baseURL = srv.URL
	if _, err := om.Fetch(context.Background(), testLat, testLon); err == nil {
		t.Fatal("expected an error for an all-null cloud response, got nil")
	}
}

// The Sources cell has two shapes: a breakdown for agreement runs, and a plain source
// name for single-provider runs and for rows written before sources_json existed.
// Every night logged to date is the second kind, so the fallback has to hold.
func TestNightRowSourcesCell(t *testing.T) {
	tmpl, err := template.New("").ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	render := func(v NightView) string {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "night_row", v); err != nil {
			t.Fatalf("render night_row: %v", err)
		}
		return buf.String()
	}

	// html/template renders the "+" in a joined source name as &#43;, so match the
	// halves rather than the literal string.
	legacy := render(NightView{Source: "open-meteo+met-no", WindowLabel: "—"})
	if !strings.Contains(legacy, "open-meteo") || !strings.Contains(legacy, "met-no") {
		t.Errorf("legacy row should fall back to the bare source name, got: %s", legacy)
	}
	if strings.Contains(legacy, "<details") {
		t.Error("legacy row should not render an empty breakdown")
	}

	split := render(NightView{
		Source:       "ecmwf+gfs+icon",
		WindowLabel:  "04:00→05:00",
		SourcesLabel: "1/3 agree · spread 24%",
		Split:        true,
		Sources: []SourceVerdict{
			{Name: "ecmwf", GO: true, WindowLabel: "03:00→05:48", Hours: 3, WindowAvg: 14},
			{Name: "gfs", GO: false, WindowLabel: "—"},
		},
	})
	for _, want := range []string{"agree split", "1/3 agree", "ecmwf", "gfs", "no usable hours"} {
		if !strings.Contains(split, want) {
			t.Errorf("split row missing %q", want)
		}
	}
}

// buildSource must map every documented CLEARSKY_SOURCE value, and anything unknown
// must land on the three-model agreement rather than silently running one model.
func TestBuildSourceModes(t *testing.T) {
	cases := map[string]string{
		"agreement":  "ecmwf+gfs+icon",
		"":           "ecmwf+gfs+icon",
		"nonsense":   "ecmwf+gfs+icon",
		"ecmwf":      "ecmwf",
		"gfs":        "gfs",
		"icon":       "icon",
		"open-meteo": "open-meteo",
		"met-no":     "met-no",
	}
	for mode, want := range cases {
		cfg := FromEnv()
		cfg.Source = mode
		if got := buildSource(cfg).Name(); got != want {
			t.Errorf("CLEARSKY_SOURCE=%q → %q, want %q", mode, got, want)
		}
	}
}
