package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A non-200 must carry the provider's stated reason into the error. The four nights
// lost in Aug 2026 all failed with a bare "open-meteo status 503"; whatever the body
// said went unread, and the cause had to be inferred from timestamps afterwards.
func TestOpenMeteoErrorIncludesResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":true,"reason":"API temporarily unavailable\n  during model update"}`))
	}))
	defer srv.Close()

	om := NewOpenMeteoModel("Australia/Melbourne", "ecmwf_ifs025", "ecmwf")
	om.baseURL = srv.URL
	_, err := om.Fetch(context.Background(), testLat, testLon)
	if err == nil {
		t.Fatal("expected an error on 503")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error should name the status: %v", err)
	}
	if !strings.Contains(err.Error(), "API temporarily unavailable") {
		t.Errorf("error should carry the provider's reason: %v", err)
	}
	// Newlines would break the one-line-per-event journal format.
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error must stay single-line for logging: %q", err)
	}
}

func TestBodySnippetTruncatesAndHandlesEmpty(t *testing.T) {
	if got := bodySnippet(strings.NewReader("")); got != "(no body)" {
		t.Errorf("empty body: got %q", got)
	}
	long := bodySnippet(strings.NewReader(strings.Repeat("x", 900)))
	if len([]rune(long)) > 201 {
		t.Errorf("snippet not truncated: %d runes", len([]rune(long)))
	}
}
