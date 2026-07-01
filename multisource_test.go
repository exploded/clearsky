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
	t1 := time.Date(2026, 7, 1, 22, 0, 0, 0, loc)
	t2 := time.Date(2026, 7, 1, 23, 0, 0, 0, loc)

	// Source A: clear at both hours. Source B: cloudy + rainy at t2.
	a := stubSource{name: "a", hours: []HourlyPoint{
		{At: t1, CloudTotal: 5, CloudLow: 0, CloudMid: 0, CloudHigh: 5, PrecipMm: 0},
		{At: t2, CloudTotal: 10, CloudLow: 0, CloudMid: 5, CloudHigh: 10, PrecipMm: 0},
	}}
	b := stubSource{name: "b", hours: []HourlyPoint{
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
	if len(fc.Hours) != 2 {
		t.Fatalf("expected 2 merged hours, got %d", len(fc.Hours))
	}
	// t2 must reflect the WORST of both (source B): cloud 90, precip 1.2.
	got := fc.Hours[1]
	if got.CloudTotal != 90 || got.PrecipMm != 1.2 || got.CloudLow != 60 {
		t.Errorf("pessimistic merge wrong at t2: %+v", got)
	}

	// Agreement semantics: A alone would be GO, but merged (with B's rain) is NO-GO.
	th := defaultThresholds()
	if !Evaluate(a.hours, th).GO {
		t.Error("source A alone should be GO")
	}
	if Evaluate(fc.Hours, th).GO {
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
