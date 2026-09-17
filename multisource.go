package main

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
)

// MultiSource fetches several providers and merges them by a per-hour VOTE: an hour is
// usable in the merged forecast only when at least `agree` models, each judged on its
// own against the normal per-hour gate, call it usable. The merged forecast is then fed
// to the ordinary decision engine, so "GO" means "enough models agree on the same run
// of clear hours".
//
// The default is a majority (2 of 3), not unanimity. The merge used to take the worst
// value of every field across all models, which made any one model a veto. At Donvale
// that model is nearly always ICON, which forecasts low stratocumulus on nights ECMWF
// and GFS call clear: across 17 Aug-17 Sep 2026, seven nights had two models at GO and
// still recorded NO-GO, five of them on ICON alone — including 17 Sep, which ICON put
// at 25-74% low cloud and which was clear. Majority still stops the failure the
// independent panel was built for (one optimistic model shipping a GO on its own, as
// on 2026-08-03), because the lone optimist is outvoted exactly as the lone pessimist
// now is. CLEARSKY_MIN_AGREE=3 restores the veto.
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
	min     int        // models that must answer for the fetch to succeed
	agree   int        // models that must call an hour usable for the merge to keep it usable
	th      Thresholds // the per-hour gate each model is judged against in the vote
}

// NewMultiSource requires every source to answer and every source to agree — the
// strictest panel. Production uses NewMultiSourceMin.
func NewMultiSource(th Thresholds, sources ...Source) *MultiSource {
	return NewMultiSourceMin(len(sources), len(sources), th, sources...)
}

// NewMultiSourceMin sets the answer quorum (CLEARSKY_MIN_SOURCES) and the per-hour vote
// (CLEARSKY_MIN_AGREE). An out-of-range min means every source must answer; an
// out-of-range agree means a simple majority of the configured panel.
func NewMultiSourceMin(min, agree int, th Thresholds, sources ...Source) *MultiSource {
	n := len(sources)
	if min < 1 || min > n {
		min = n
	}
	if agree < 1 || agree > n {
		agree = n/2 + 1
	}
	return &MultiSource{sources: sources, min: min, agree: agree, th: th}
}

// relaxed returns a copy that accepts a single surviving source, for the scheduler's
// final attempt before it would otherwise record nothing at all. Implements relaxable.
func (m *MultiSource) relaxed() Source {
	return &MultiSource{sources: m.sources, min: 1, agree: m.agree, th: m.th}
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
		// A panel thinned by a lowered quorum or the deadline fallback cannot out-vote
		// itself: with two survivors and a 2-vote rule, both must agree.
		fc = mergeVote(ok, min(m.agree, len(ok)), m.th)
	} else {
		// A lone survivor still records itself as a member, so the log page can say
		// "icon only" rather than showing a bare source name that looks routine.
		fc.Members = ok
	}
	fc.Missing = failed
	return fc, nil
}

// mergeVote combines forecasts hour by hour (aligned by absolute time). For each hour it
// ranks the models' points — usable before unusable, then by total cloud — and keeps
// the agree-th best WHOLE point. So the merged hour is usable exactly when at least
// `agree` models find it usable on their own, and its values belong to one real model
// rather than being stitched together field by field. (A field-wise median would let
// one model's total cloud pass alongside another's low cloud, clearing an hour no
// model actually called clear.) With agree == len(forecasts) this keeps the worst
// point: the old all-must-agree merge.
//
// An hour fewer than `agree` models forecast at all is dropped: it cannot win the vote,
// and the gap it leaves correctly breaks any usable run across it.
func mergeVote(forecasts []Forecast, agree int, th Thresholds) Forecast {
	byHour := map[int64][]HourlyPoint{}
	names := make([]string, 0, len(forecasts))
	for _, fc := range forecasts {
		names = append(names, fc.Source)
		for _, h := range fc.Hours {
			key := h.At.Unix()
			byHour[key] = append(byHour[key], h)
		}
	}

	hours := make([]HourlyPoint, 0, len(byHour))
	for _, pts := range byHour {
		if len(pts) < agree {
			continue
		}
		sort.SliceStable(pts, func(i, j int) bool {
			ui, uj := hourUsable(pts[i], th), hourUsable(pts[j], th)
			if ui != uj {
				return ui
			}
			return pts[i].CloudTotal < pts[j].CloudTotal
		})
		hours = append(hours, pts[agree-1])
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].At.Before(hours[j].At) })

	// Keep the un-merged inputs so the runner can report each model's own verdict.
	// The merge is deliberately lossy — it cannot show which model was outvoted.
	return Forecast{Source: joinNames(names), Hours: hours, Members: forecasts}
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
