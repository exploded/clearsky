package main

import (
	"context"
	"errors"
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

func TestMultiSourceFallback(t *testing.T) {
	loc := mustMelbourne(t)
	t1 := time.Date(2026, 7, 1, 22, 0, 0, 0, loc)
	ok := stubSource{name: "ok", hours: []HourlyPoint{{At: t1, CloudTotal: 5}}}
	bad := stubSource{name: "bad", err: errors.New("boom")}

	// One source down -> degrade to the survivor, no error.
	fc, err := NewMultiSource(bad, ok).Fetch(context.Background(), 0, 0)
	if err != nil {
		t.Fatalf("expected graceful fallback, got %v", err)
	}
	if fc.Source != "ok" {
		t.Errorf("source = %q, want ok", fc.Source)
	}

	// All sources down -> error.
	if _, err := NewMultiSource(bad, stubSource{name: "bad2", err: errors.New("x")}).Fetch(context.Background(), 0, 0); err == nil {
		t.Error("expected error when all sources fail")
	}
}
