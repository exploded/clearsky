package main

import (
	"testing"
	"time"
)

// Donvale, VIC.
const testLat, testLon = -37.79, 145.18

func mustMelbourne(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Australia/Melbourne")
	if err != nil {
		t.Fatalf("load tz: %v", err)
	}
	return loc
}

func TestDarknessWindow(t *testing.T) {
	loc := mustMelbourne(t)
	// A winter evening in Donvale: long night, astronomical darkness definitely occurs.
	date := time.Date(2026, 7, 1, 0, 0, 0, 0, loc)
	d := darknessWindow(date, testLat, testLon, loc)

	if d.Kind != "astronomical" {
		t.Errorf("expected astronomical darkness, got kind=%q", d.Kind)
	}
	if !d.Dawn.After(d.Dusk) {
		t.Fatalf("dawn %v not after dusk %v", d.Dawn, d.Dusk)
	}
	// Dusk should be that evening (after 17:00 local), dawn the next morning (before noon).
	if h := d.Dusk.In(loc).Hour(); h < 17 || h > 21 {
		t.Errorf("dusk hour %d out of expected evening range", h)
	}
	if d.Dawn.In(loc).Day() != date.Day()+1 {
		t.Errorf("dawn should be next morning, got %v", d.Dawn.In(loc))
	}
	// Winter night length in Melbourne is well over 10 hours of dark.
	if dur := d.Dawn.Sub(d.Dusk); dur < 9*time.Hour || dur > 14*time.Hour {
		t.Errorf("implausible dark duration %v", dur)
	}
}

func TestMoonInfo(t *testing.T) {
	loc := mustMelbourne(t)
	date := time.Date(2026, 7, 1, 0, 0, 0, 0, loc)
	d := darknessWindow(date, testLat, testLon, loc)
	m := moonInfo(d, testLat, testLon, loc)

	if m.IllumPct < 0 || m.IllumPct > 100 {
		t.Errorf("illumination %.1f out of range", m.IllumPct)
	}
	if m.PhaseName == "" {
		t.Error("expected a phase name")
	}
	// Any rise/set reported must genuinely be inside the window.
	if m.Rise != nil && (m.Rise.Before(d.Dusk) || m.Rise.After(d.Dawn)) {
		t.Errorf("moonrise %v outside window [%v,%v]", m.Rise, d.Dusk, d.Dawn)
	}
	if m.Set != nil && (m.Set.Before(d.Dusk) || m.Set.After(d.Dawn)) {
		t.Errorf("moonset %v outside window [%v,%v]", m.Set, d.Dusk, d.Dawn)
	}
}

func TestPhaseName(t *testing.T) {
	cases := []struct {
		p    float64
		want string
	}{
		{0.0, "New Moon"},
		{0.25, "First Quarter"},
		{0.5, "Full Moon"},
		{0.75, "Last Quarter"},
		{0.125, "Waxing Crescent"},
		{0.375, "Waxing Gibbous"},
		{0.625, "Waning Gibbous"},
		{0.875, "Waning Crescent"},
	}
	for _, c := range cases {
		if got := phaseName(c.p); got != c.want {
			t.Errorf("phaseName(%.3f) = %q, want %q", c.p, got, c.want)
		}
	}
}
