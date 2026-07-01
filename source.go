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

// HoursWithin returns the forecast hours whose timestamp falls in [start, end].
func (f Forecast) HoursWithin(start, end time.Time) []HourlyPoint {
	out := make([]HourlyPoint, 0, len(f.Hours))
	for _, h := range f.Hours {
		if (h.At.Equal(start) || h.At.After(start)) && (h.At.Equal(end) || h.At.Before(end)) {
			out = append(out, h)
		}
	}
	return out
}
