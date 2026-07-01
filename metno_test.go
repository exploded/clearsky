package main

import (
	"context"
	"testing"
	"time"
)

// TestMetNoLive hits the real MET Norway API. Skipped under `go test -short`.
func TestMetNoLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live met.no fetch in -short mode")
	}
	m := NewMetNo("clearsky-astro-test/1.0 (+https://deepspaceplace.com)")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	fc, err := m.Fetch(ctx, testLat, testLon)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if fc.Source != "met-no" {
		t.Errorf("source = %q", fc.Source)
	}
	if len(fc.Hours) < 24 {
		t.Fatalf("expected many hourly points, got %d", len(fc.Hours))
	}
	for _, h := range fc.Hours {
		if h.CloudTotal < 0 || h.CloudTotal > 100 {
			t.Errorf("cloud %d out of range at %v", h.CloudTotal, h.At)
		}
		if h.PrecipMm < 0 {
			t.Errorf("negative precip at %v", h.At)
		}
	}
}
