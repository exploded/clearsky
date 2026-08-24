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
// A fetch that loses sources produces an error, not a thinner answer. This used to be
// the other way round — any single survivor was returned as a success — which made a
// PARTIAL outage worse than a total one: a total failure errored and went into the
// retry ladder (which works: Open-Meteo 503s at the fire time every night and recovers
// within minutes), while a partial failure sailed straight through and decided the
// night on whatever model happened to answer. On 20 Aug 2026 that was ICON alone,
// scoring 73/GO with the agreement rule silently switched off, and the alert went to
// the subscriber list. Requiring the quorum turns that into a retry.
//
// min is normally every configured source. The last-resort relaxed copy (see relaxed)
// accepts one, for the deadline case where a thin answer beats no answer at all.
type MultiSource struct {
	sources []Source
	min     int
}

// NewMultiSource requires every source to answer. Use NewMultiSourceMin to lower the
// bar deliberately (CLEARSKY_MIN_SOURCES).
func NewMultiSource(sources ...Source) *MultiSource {
	return &MultiSource{sources: sources, min: len(sources)}
}

func NewMultiSourceMin(min int, sources ...Source) *MultiSource {
	if min < 1 || min > len(sources) {
		min = len(sources)
	}
	return &MultiSource{sources: sources, min: min}
}

// relaxed returns a copy that accepts a single surviving source, for the scheduler's
// final attempt before it would otherwise record nothing at all. Implements relaxable.
func (m *MultiSource) relaxed() Source {
	return &MultiSource{sources: m.sources, min: 1}
}

// Name lists the members rather than just saying "agreement", so the startup log and
// any single-source fallback name the models actually being consulted.
func (m *MultiSource) Name() string {
	names := make([]string, 0, len(m.sources))
	for _, s := range m.sources {
		names = append(names, s.Name())
	}
	return joinNames(names)
}

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

	if len(ok) == 0 {
		return Forecast{}, fmt.Errorf("all sources failed: %v", failed)
	}
	if len(ok) < m.min {
		// Not an outage the caller can paper over: the decision this would produce is
		// weaker than the one it is configured to make. Report it as a failure so the
		// retry ladder gets a turn.
		return Forecast{}, fmt.Errorf("only %d of %d models answered (%v failed); need %d",
			len(ok), len(m.sources), failed, m.min)
	}

	fc := ok[0]
	if len(ok) > 1 {
		fc = mergePessimistic(ok)
	} else {
		// A lone survivor still records itself as a member, so the log page can say
		// "icon only" rather than showing a bare source name that looks routine.
		fc.Members = ok
	}
	fc.Missing = failed
	return fc, nil
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

	// Keep the un-merged inputs so the runner can report each model's own verdict.
	// The merge is deliberately lossy — it answers "is every source clear?" and cannot
	// distinguish unanimous agreement from one pessimist overruling three optimists.
	return Forecast{Source: joinNames(names), Hours: hours, Members: forecasts}
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
