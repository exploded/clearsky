package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
)

// MultiSource fetches several providers and merges them pessimistically: for each
// hour it takes the WORST (max) cloud and precipitation across sources. Feeding that
// worst-case forecast to the normal decision engine yields "GO only if EVERY source
// agrees it's clear" — because the merged value passes a threshold only when all
// sources are below it. No changes to the decision engine are needed.
//
// If a source fails, the run degrades gracefully to whichever sources succeeded
// (logged), so one provider outage never blocks a decision.
type MultiSource struct {
	sources []Source
}

func NewMultiSource(sources ...Source) *MultiSource {
	return &MultiSource{sources: sources}
}

func (m *MultiSource) Name() string { return "agreement" }

func (m *MultiSource) Fetch(ctx context.Context, lat, lon float64) (Forecast, error) {
	type res struct {
		fc  Forecast
		err error
	}
	results := make([]res, len(m.sources))
	done := make(chan int, len(m.sources))
	for i, s := range m.sources {
		go func(i int, s Source) {
			fc, err := s.Fetch(ctx, lat, lon)
			results[i] = res{fc, err}
			done <- i
		}(i, s)
	}
	for range m.sources {
		<-done
	}

	var ok []Forecast
	var failed []string
	for i, r := range results {
		if r.err != nil {
			slog.Warn("source failed in agreement fetch", "source", m.sources[i].Name(), "err", r.err)
			failed = append(failed, m.sources[i].Name())
			continue
		}
		ok = append(ok, r.fc)
	}

	switch len(ok) {
	case 0:
		return Forecast{}, fmt.Errorf("all sources failed: %v", failed)
	case 1:
		// Only one source available — return it as-is (no agreement possible).
		return ok[0], nil
	default:
		return mergePessimistic(ok), nil
	}
}

// mergePessimistic combines forecasts by taking the element-wise maximum cloud and
// precipitation per hour (aligned by absolute time). The result's Source lists the
// contributing providers, e.g. "open-meteo+met-no".
func mergePessimistic(forecasts []Forecast) Forecast {
	byHour := map[int64]HourlyPoint{}
	names := make([]string, 0, len(forecasts))
	for _, fc := range forecasts {
		names = append(names, fc.Source)
		for _, h := range fc.Hours {
			key := h.At.Unix()
			cur, exists := byHour[key]
			if !exists {
				byHour[key] = h
				continue
			}
			byHour[key] = HourlyPoint{
				At:            cur.At,
				CloudTotal:    max(cur.CloudTotal, h.CloudTotal),
				CloudLow:      max(cur.CloudLow, h.CloudLow),
				CloudMid:      max(cur.CloudMid, h.CloudMid),
				CloudHigh:     max(cur.CloudHigh, h.CloudHigh),
				PrecipMm:      maxF(cur.PrecipMm, h.PrecipMm),
				PrecipProbPct: max(cur.PrecipProbPct, h.PrecipProbPct),
				VisibilityM:   minVis(cur.VisibilityM, h.VisibilityM),
			}
		}
	}

	hours := make([]HourlyPoint, 0, len(byHour))
	for _, h := range byHour {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].At.Before(hours[j].At) })

	return Forecast{Source: joinNames(names), Hours: hours}
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

// minVis takes the lower (worse) visibility, treating 0 as "unknown" (ignored).
func minVis(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func joinNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += "+"
		}
		out += n
	}
	return out
}
