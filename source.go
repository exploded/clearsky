package main

import (
	"context"
	"io"
	"strings"
	"time"
)

// Source is any provider that can forecast a night's clouds + rain for a location.
// v1 ships one implementation (OpenMeteo); adding a provider later is a new file that
// implements this interface. A future "pick best / merge" strategy can itself be a
// Source that wraps others — the chosen provider's Name() is stored per night.
type Source interface {
	Name() string
	Fetch(ctx context.Context, lat, lon float64) (Forecast, error)
}

// bodySnippet reads the first few hundred bytes of an error response for inclusion in
// the returned error. Providers explain themselves in the body — Open-Meteo answers
// {"error":true,"reason":"…"} — and formatting only the status code throws that away.
//
// This is not cosmetic. On 2026-08-04..07 every nightly run died on "open-meteo status
// 503" with no further detail, and the reason had to be reconstructed from the pattern
// of failure timestamps days later. Log what the server actually said.
func bodySnippet(r io.Reader) string {
	b, err := io.ReadAll(io.LimitReader(r, 512))
	if err != nil || len(b) == 0 {
		return "(no body)"
	}
	s := strings.Join(strings.Fields(string(b)), " ") // collapse newlines/indentation
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// Forecast is a provider-neutral hourly forecast. Hours covers the whole returned
// horizon; the caller selects those within the darkness window.
type Forecast struct {
	Source string
	Hours  []HourlyPoint

	// Members is set only by MultiSource: the untouched per-provider forecasts that
	// Hours was merged from. It lets the runner ask each model for its own verdict on
	// the night, so the log page can show whether the sources actually agreed or the
	// merge was hiding a 90%-cloud outlier. Always empty for a single provider.
	Members []Forecast
}

// HourlyPoint is one hour of forecast. Cloud values are percentages (0..100).
//
// At is an absolute instant, but its *location* matters: every HH:MM the app reports
// (window label, pack-up time, peak-cloud time) is formatted straight off it. Providers
// disagree — Open-Meteo is asked for site-local stamps, MET Norway returns UTC — so the
// runner normalises a fetched Forecast with InLocation before anything reads At.
type HourlyPoint struct {
	At            time.Time `json:"at"`
	CloudTotal    int       `json:"cloud"`
	CloudLow      int       `json:"cloudLow"`
	CloudMid      int       `json:"cloudMid"`
	CloudHigh     int       `json:"cloudHigh"`
	PrecipMm      float64   `json:"precipMm"`
	PrecipProbPct int       `json:"precipProbPct"`
	VisibilityM   int       `json:"visM"`
}

// InLocation restamps every hour into loc. The instants are unchanged — only the wall
// clock they render as — so it is safe to call on any provider's output and required
// before formatting: a UTC-stamped 02:00 Melbourne hour otherwise prints as "16:00".
func (f Forecast) InLocation(loc *time.Location) Forecast {
	if loc == nil {
		return f
	}
	hours := make([]HourlyPoint, len(f.Hours))
	for i, h := range f.Hours {
		h.At = h.At.In(loc)
		hours[i] = h
	}
	// Members are restamped too — each is evaluated independently downstream, and a
	// UTC-stamped member would slice the wrong hours out of the darkness window.
	var members []Forecast
	if len(f.Members) > 0 {
		members = make([]Forecast, len(f.Members))
		for i, m := range f.Members {
			members[i] = m.InLocation(loc)
		}
	}
	return Forecast{Source: f.Source, Hours: hours, Members: members}
}

// HoursWithin returns the forecast hours that OVERLAP [start, end]. An hourly point
// stamped T describes the hour [T, T+1h), so the bucket containing start is included
// even though it begins slightly before it — a strict comparison would silently throw
// away the first hour of nearly every night (dusk at 19:02 discarding all of 19:00).
func (f Forecast) HoursWithin(start, end time.Time) []HourlyPoint {
	// Floor to the local hour. Built explicitly rather than with Truncate, which works
	// on absolute time and so misfires in half-hour zones (Adelaide, India).
	from := time.Date(start.Year(), start.Month(), start.Day(), start.Hour(), 0, 0, 0, start.Location())
	out := make([]HourlyPoint, 0, len(f.Hours))
	for _, h := range f.Hours {
		if !h.At.Before(from) && h.At.Before(end) {
			out = append(out, h)
		}
	}
	return out
}
