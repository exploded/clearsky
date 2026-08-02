package main

import (
	"context"
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

// Forecast is a provider-neutral hourly forecast. Hours covers the whole returned
// horizon; the caller selects those within the darkness window.
type Forecast struct {
	Source string
	Hours  []HourlyPoint
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
	return Forecast{Source: f.Source, Hours: hours}
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
