package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubSource returns a fixed forecast (or error) for tests.
type stubSource struct {
	name  string
	hours []HourlyPoint
	err   error
}

func (s stubSource) Name() string { return s.name }
func (s stubSource) Fetch(_ context.Context, _, _ float64) (Forecast, error) {
	if s.err != nil {
		return Forecast{}, s.err
	}
	return Forecast{Source: s.name, Hours: s.hours}, nil
}

func TestMergeUnanimous(t *testing.T) {
	loc := mustMelbourne(t)
	t0 := time.Date(2026, 7, 1, 21, 0, 0, 0, loc)
	t1 := time.Date(2026, 7, 1, 22, 0, 0, 0, loc)
	t2 := time.Date(2026, 7, 1, 23, 0, 0, 0, loc)

	// Source A: clear at all three hours. Source B: cloudy + rainy at t2. Three hours
	// so source A alone clears the minimum usable window.
	a := stubSource{name: "a", hours: []HourlyPoint{
		{At: t0, CloudTotal: 5, CloudLow: 0, CloudMid: 0, CloudHigh: 5, PrecipMm: 0},
		{At: t1, CloudTotal: 5, CloudLow: 0, CloudMid: 0, CloudHigh: 5, PrecipMm: 0},
		{At: t2, CloudTotal: 10, CloudLow: 0, CloudMid: 5, CloudHigh: 10, PrecipMm: 0},
	}}
	b := stubSource{name: "b", hours: []HourlyPoint{
		{At: t0.UTC(), CloudTotal: 6, CloudLow: 1, CloudMid: 0, CloudHigh: 6, PrecipMm: 0},
		{At: t1.UTC(), CloudTotal: 8, CloudLow: 2, CloudMid: 0, CloudHigh: 8, PrecipMm: 0},
		{At: t2.UTC(), CloudTotal: 90, CloudLow: 60, CloudMid: 40, CloudHigh: 10, PrecipMm: 1.2, PrecipProbPct: 80},
	}}

	th := defaultThresholds()
	fc, err := NewMultiSource(th, a, b).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if fc.Source != "a+b" {
		t.Errorf("source = %q, want a+b", fc.Source)
	}
	if len(fc.Hours) != 3 {
		t.Fatalf("expected 3 merged hours, got %d", len(fc.Hours))
	}
	// When every model must agree, t2 is source B's point: cloud 90, precip 1.2.
	got := fc.Hours[2]
	if got.CloudTotal != 90 || got.PrecipMm != 1.2 || got.CloudLow != 60 {
		t.Errorf("unanimous merge wrong at t2: %+v", got)
	}
	// Among usable points it keeps the cloudier one.
	if fc.Hours[1].CloudTotal != 8 {
		t.Errorf("unanimous merge at t1 = %d%% cloud, want 8 (the worse model)", fc.Hours[1].CloudTotal)
	}

	// A alone is a 3h usable window (GO); the merge loses t2 to B's rain, leaving only
	// 2h — under the minimum, so NO-GO.
	dark := testDark()
	if !Evaluate(a.hours, dark, th).GO {
		t.Error("source A alone should be GO")
	}
	if Evaluate(fc.Hours, dark, th).GO {
		t.Error("merged (disagreement) should be NO-GO")
	}
}

// majority is the production panel: all three must answer, two must agree per hour.
func majority(th Thresholds, sources ...Source) *MultiSource {
	return NewMultiSourceMin(0, 0, th, sources...)
}

// 17 Sep 2026, from the live forecasts: ECMWF and GFS both put the whole night at
// 0-27% cloud, ICON put it at 25-74% low cloud, and the sky was clear. Under the old
// all-must-agree merge ICON's low cloud cut the night down to one usable hour.
func TestMergeMajorityOutvotesLonePessimist(t *testing.T) {
	loc := mustMelbourne(t)
	dusk := time.Date(2026, 9, 17, 19, 0, 0, 0, loc)
	dark := Darkness{Dusk: dusk, Dawn: dusk.Add(6 * time.Hour)}
	th := defaultThresholds()

	icon := clearHours(dusk, 6, 65)
	for i := range icon {
		icon[i].CloudLow = 65
	}
	sources := []Source{
		stubSource{name: "ecmwf", hours: clearHours(dusk, 6, 5)},
		stubSource{name: "gfs", hours: clearHours(dusk, 6, 0)},
		stubSource{name: "icon", hours: icon},
	}

	fc, err := majority(th, sources...).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got := Evaluate(fc.InLocation(loc).HoursWithin(dark.Dusk, dark.Dawn), dark, th)
	if !got.GO || got.Window.Hours != 6 {
		t.Fatalf("2 of 3 clear should be GO over 6h, got GO=%v %dh: %s", got.GO, got.Window.Hours, got.Reason)
	}
	// The values reported are the less optimistic of the two models that agreed.
	if got.Window.AvgCloud != 5 {
		t.Errorf("window avg = %d%%, want 5 (ECMWF, the cloudier of the majority)", got.Window.AvgCloud)
	}

	fc, err = NewMultiSource(th, sources...).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if Evaluate(fc.InLocation(loc).HoursWithin(dark.Dusk, dark.Dawn), dark, th).GO {
		t.Error("with every model required to agree, the lone pessimist must still veto")
	}
}

// The failure the independent panel exists to catch (2026-08-03) must still be caught:
// a lone optimist is outvoted exactly as a lone pessimist is.
func TestMergeMajorityOutvotesLoneOptimist(t *testing.T) {
	loc := mustMelbourne(t)
	dusk := time.Date(2026, 8, 3, 19, 0, 0, 0, loc)
	dark := Darkness{Dusk: dusk, Dawn: dusk.Add(6 * time.Hour)}
	th := defaultThresholds()

	fc, err := majority(th,
		stubSource{name: "ecmwf", hours: clearHours(dusk, 6, 6)},
		stubSource{name: "gfs", hours: clearHours(dusk, 6, 95)},
		stubSource{name: "icon", hours: clearHours(dusk, 6, 88)},
	).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := Evaluate(fc.InLocation(loc).HoursWithin(dark.Dusk, dark.Dawn), dark, th); got.GO {
		t.Errorf("1 of 3 clear must be NO-GO, got GO: %s", got.Reason)
	}
}

// Two models that each find a clear window, at DIFFERENT times, are not two models
// agreeing. The vote is per hour, so neither window survives.
func TestMergeVoteNeedsAgreementOnTheSameHours(t *testing.T) {
	loc := mustMelbourne(t)
	dusk := time.Date(2026, 8, 3, 19, 0, 0, 0, loc)
	dark := Darkness{Dusk: dusk, Dawn: dusk.Add(6 * time.Hour)}
	th := defaultThresholds()

	early := append(clearHours(dusk, 3, 5), clearHours(dusk.Add(3*time.Hour), 3, 90)...)
	late := append(clearHours(dusk, 3, 90), clearHours(dusk.Add(3*time.Hour), 3, 5)...)
	fc, err := majority(th,
		stubSource{name: "ecmwf", hours: early},
		stubSource{name: "gfs", hours: late},
		stubSource{name: "icon", hours: clearHours(dusk, 6, 90)},
	).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := Evaluate(fc.InLocation(loc).HoursWithin(dark.Dusk, dark.Dawn), dark, th); got.GO {
		t.Errorf("non-overlapping windows must not add up to a GO, got: %s", got.Reason)
	}
}

// The merged hour is one model's whole point, never a field-wise blend. A median per
// field would pair A's clear total with B's clear low layer and pass an hour that only
// one model (C) actually called usable.
func TestMergeVoteDoesNotMixFields(t *testing.T) {
	loc := mustMelbourne(t)
	at := time.Date(2026, 8, 3, 22, 0, 0, 0, loc)
	th := defaultThresholds()

	merged := mergeVote([]Forecast{
		{Source: "a", Hours: []HourlyPoint{{At: at, CloudTotal: 10, CloudLow: 35}}},
		{Source: "b", Hours: []HourlyPoint{{At: at, CloudTotal: 50, CloudLow: 10}}},
		{Source: "c", Hours: []HourlyPoint{{At: at, CloudTotal: 10, CloudLow: 10}}},
	}, 2, th)
	if len(merged.Hours) != 1 {
		t.Fatalf("merged hours = %d, want 1", len(merged.Hours))
	}
	if hourUsable(merged.Hours[0], th) {
		t.Errorf("only 1 of 3 models called the hour usable, merged point %+v passed", merged.Hours[0])
	}
}

// A partial outage must fail the fetch. It used to succeed on whatever survived, which
// made a partial failure strictly worse than a total one: a total failure went into the
// retry ladder and recovered minutes later, while a partial failure went straight to a
// decision with the agreement rule inoperative. Both are now errors.
func TestMultiSourceQuorum(t *testing.T) {
	th := defaultThresholds()
	loc := mustMelbourne(t)
	t1 := time.Date(2026, 7, 1, 22, 0, 0, 0, loc)
	hours := []HourlyPoint{{At: t1, CloudTotal: 5}}
	ecmwf := stubSource{name: "ecmwf", err: errors.New("503")}
	gfs := stubSource{name: "gfs", err: errors.New("503")}
	icon := stubSource{name: "icon", hours: hours}

	if _, err := NewMultiSource(th, ecmwf, gfs, icon).Fetch(context.Background(), 0, 0); err == nil {
		t.Fatal("1 of 3 models must not satisfy the default quorum")
	}
	// Two of three is still short of the quorum — the merge is only a cross-check if
	// everything configured to disagree got the chance to.
	if _, err := NewMultiSource(th, ecmwf, stubSource{name: "gfs", hours: hours}, icon).Fetch(context.Background(), 0, 0); err == nil {
		t.Error("2 of 3 models must not satisfy the default quorum")
	}
	// All sources down -> error, naming them.
	_, err := NewMultiSource(th, ecmwf, gfs).Fetch(context.Background(), 0, 0)
	if err == nil || !strings.Contains(err.Error(), "all sources failed") {
		t.Errorf("expected an all-failed error, got %v", err)
	}
}

// The relaxed copy is the deadline fallback: it accepts a lone survivor, but the answer
// has to carry the names of the models that went missing, or the log page and the alert
// cannot tell anyone that the night was decided on one opinion.
func TestMultiSourceRelaxedReportsMissing(t *testing.T) {
	th := defaultThresholds()
	loc := mustMelbourne(t)
	t1 := time.Date(2026, 7, 1, 22, 0, 0, 0, loc)
	ecmwf := stubSource{name: "ecmwf", err: errors.New("503")}
	gfs := stubSource{name: "gfs", err: errors.New("503")}
	icon := stubSource{name: "icon", hours: []HourlyPoint{{At: t1, CloudTotal: 5}}}

	relaxed := NewMultiSource(th, ecmwf, gfs, icon).relaxed()
	fc, err := relaxed.Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("relaxed fetch should accept a lone survivor: %v", err)
	}
	if fc.Source != "icon" {
		t.Errorf("source = %q, want icon", fc.Source)
	}
	if len(fc.Missing) != 2 || fc.Missing[0] != "ecmwf" || fc.Missing[1] != "gfs" {
		t.Errorf("Missing = %v, want [ecmwf gfs]", fc.Missing)
	}
	if len(fc.Members) != 1 {
		t.Errorf("a lone survivor must still be recorded as a member, got %d", len(fc.Members))
	}
	// Even relaxed, nothing at all is still nothing.
	if _, err := NewMultiSource(th, ecmwf, gfs).relaxed().Fetch(context.Background(), 0, 0); err == nil {
		t.Error("expected error when every source fails, even relaxed")
	}
}

// An explicitly lowered quorum (CLEARSKY_MIN_SOURCES) still records what it lost.
func TestMultiSourceMinConfigured(t *testing.T) {
	loc := mustMelbourne(t)
	t1 := time.Date(2026, 7, 1, 22, 0, 0, 0, loc)
	hours := []HourlyPoint{{At: t1, CloudTotal: 5}}
	fc, err := NewMultiSourceMin(2, 0, defaultThresholds(),
		stubSource{name: "ecmwf", err: errors.New("503")},
		stubSource{name: "gfs", hours: hours},
		stubSource{name: "icon", hours: hours},
	).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("2 of 3 should satisfy a configured minimum of 2: %v", err)
	}
	if len(fc.Members) != 2 || len(fc.Missing) != 1 {
		t.Errorf("members = %d, missing = %v; want 2 and [ecmwf]", len(fc.Members), fc.Missing)
	}
	// Out-of-range minimums fall back to requiring everything, never to requiring none.
	two := []Source{stubSource{name: "a"}, stubSource{name: "b"}}
	if got := NewMultiSourceMin(0, 0, defaultThresholds(), two...).min; got != 2 {
		t.Errorf("min 0 → %d, want 2 (all sources)", got)
	}
	if got := NewMultiSourceMin(9, 0, defaultThresholds(), two...).min; got != 2 {
		t.Errorf("min 9 → %d, want 2 (all sources)", got)
	}
	// Out-of-range agreement falls back to a majority of the panel.
	three := append(two, stubSource{name: "c"})
	for _, tc := range []struct {
		agree   int
		sources []Source
		want    int
	}{{0, three, 2}, {9, three, 2}, {3, three, 3}, {0, two, 2}, {0, two[:1], 1}} {
		if got := NewMultiSourceMin(0, tc.agree, defaultThresholds(), tc.sources...).agree; got != tc.want {
			t.Errorf("agree %d of %d → %d, want %d", tc.agree, len(tc.sources), got, tc.want)
		}
	}
}
