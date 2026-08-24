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

func TestMergePessimistic(t *testing.T) {
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

	fc, err := NewMultiSource(a, b).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if fc.Source != "a+b" {
		t.Errorf("source = %q, want a+b", fc.Source)
	}
	if len(fc.Hours) != 3 {
		t.Fatalf("expected 3 merged hours, got %d", len(fc.Hours))
	}
	// t2 must reflect the WORST of both (source B): cloud 90, precip 1.2.
	got := fc.Hours[2]
	if got.CloudTotal != 90 || got.PrecipMm != 1.2 || got.CloudLow != 60 {
		t.Errorf("pessimistic merge wrong at t2: %+v", got)
	}

	// Agreement semantics: A alone is a 3h usable window (GO); the merge loses t2 to
	// B's rain, leaving only 2h — under the minimum, so NO-GO.
	th := defaultThresholds()
	dark := testDark()
	if !Evaluate(a.hours, dark, th).GO {
		t.Error("source A alone should be GO")
	}
	if Evaluate(fc.Hours, dark, th).GO {
		t.Error("merged (disagreement) should be NO-GO")
	}
}

// A partial outage must fail the fetch. It used to succeed on whatever survived, which
// made a partial failure strictly worse than a total one: a total failure went into the
// retry ladder and recovered minutes later, while a partial failure went straight to a
// decision with the agreement rule inoperative. Both are now errors.
func TestMultiSourceQuorum(t *testing.T) {
	loc := mustMelbourne(t)
	t1 := time.Date(2026, 7, 1, 22, 0, 0, 0, loc)
	hours := []HourlyPoint{{At: t1, CloudTotal: 5}}
	ecmwf := stubSource{name: "ecmwf", err: errors.New("503")}
	gfs := stubSource{name: "gfs", err: errors.New("503")}
	icon := stubSource{name: "icon", hours: hours}

	if _, err := NewMultiSource(ecmwf, gfs, icon).Fetch(context.Background(), 0, 0); err == nil {
		t.Fatal("1 of 3 models must not satisfy the default quorum")
	}
	// Two of three is still short of the quorum — the merge is only a cross-check if
	// everything configured to disagree got the chance to.
	if _, err := NewMultiSource(ecmwf, stubSource{name: "gfs", hours: hours}, icon).Fetch(context.Background(), 0, 0); err == nil {
		t.Error("2 of 3 models must not satisfy the default quorum")
	}
	// All sources down -> error, naming them.
	_, err := NewMultiSource(ecmwf, gfs).Fetch(context.Background(), 0, 0)
	if err == nil || !strings.Contains(err.Error(), "all sources failed") {
		t.Errorf("expected an all-failed error, got %v", err)
	}
}

// The relaxed copy is the deadline fallback: it accepts a lone survivor, but the answer
// has to carry the names of the models that went missing, or the log page and the alert
// cannot tell anyone that the night was decided on one opinion.
func TestMultiSourceRelaxedReportsMissing(t *testing.T) {
	loc := mustMelbourne(t)
	t1 := time.Date(2026, 7, 1, 22, 0, 0, 0, loc)
	ecmwf := stubSource{name: "ecmwf", err: errors.New("503")}
	gfs := stubSource{name: "gfs", err: errors.New("503")}
	icon := stubSource{name: "icon", hours: []HourlyPoint{{At: t1, CloudTotal: 5}}}

	relaxed := NewMultiSource(ecmwf, gfs, icon).relaxed()
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
	if _, err := NewMultiSource(ecmwf, gfs).relaxed().Fetch(context.Background(), 0, 0); err == nil {
		t.Error("expected error when every source fails, even relaxed")
	}
}

// An explicitly lowered quorum (CLEARSKY_MIN_SOURCES) still records what it lost.
func TestMultiSourceMinConfigured(t *testing.T) {
	loc := mustMelbourne(t)
	t1 := time.Date(2026, 7, 1, 22, 0, 0, 0, loc)
	hours := []HourlyPoint{{At: t1, CloudTotal: 5}}
	fc, err := NewMultiSourceMin(2,
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
	if got := NewMultiSourceMin(0, stubSource{name: "a"}, stubSource{name: "b"}).min; got != 2 {
		t.Errorf("min 0 → %d, want 2 (all sources)", got)
	}
	if got := NewMultiSourceMin(9, stubSource{name: "a"}, stubSource{name: "b"}).min; got != 2 {
		t.Errorf("min 9 → %d, want 2 (all sources)", got)
	}
}
