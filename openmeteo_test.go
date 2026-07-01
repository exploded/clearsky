package main

import (
	"context"
	"testing"
	"time"
)

// TestOpenMeteoLive hits the real Open-Meteo API. Skipped under `go test -short`.
func TestOpenMeteoLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live Open-Meteo fetch in -short mode")
	}
	loc := mustMelbourne(t)
	om := NewOpenMeteo("Australia/Melbourne")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fc, err := om.Fetch(ctx, testLat, testLon)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if fc.Source != "open-meteo" {
		t.Errorf("source = %q", fc.Source)
	}
	// 2 forecast days => ~48 hourly points.
	if len(fc.Hours) < 40 {
		t.Fatalf("expected ~48 hourly points, got %d", len(fc.Hours))
	}
	for _, h := range fc.Hours {
		if h.CloudTotal < 0 || h.CloudTotal > 100 {
			t.Errorf("cloud %d out of range at %v", h.CloudTotal, h.At)
		}
		if h.At.Location().String() != loc.String() {
			t.Errorf("hour %v not in site tz", h.At)
		}
	}

	// The darkness-window selector should return a sensible slice for tonight.
	now := time.Now().In(loc)
	d := darknessWindow(now, testLat, testLon, loc)
	within := fc.HoursWithin(d.Dusk, d.Dawn)
	t.Logf("tonight: dusk=%s dawn=%s hours-in-window=%d",
		d.Dusk.Format("15:04"), d.Dawn.Format("Mon 15:04"), len(within))
}
