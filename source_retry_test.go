package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// flakyServer drops the connection without a response for the first `drops` requests,
// then answers with ok. Dropping the connection is what a client sees as a transport
// error, the same class as the TLS handshake timeouts of 13 and 15 Nov 2026.
func flakyServer(t *testing.T, drops int32, ok http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= drops {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
			return
		}
		ok(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func getter(ctx context.Context, url string) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	}
}

func TestDoWithRetryRecoversFromOneTransportFailure(t *testing.T) {
	srv, calls := flakyServer(t, 1, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	ctx := context.Background()
	resp, err := doWithRetry(ctx, srv.Client(), time.Millisecond, getter(ctx, srv.URL))
	if err != nil {
		t.Fatalf("expected recovery on the second try, got %v", err)
	}
	resp.Body.Close()
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
}

// An HTTP error status is the server answering. It goes back to the caller untouched,
// for the scheduler's longer backoff, rather than being hammered again 10 s later.
func TestDoWithRetryDoesNotRetryHTTPStatus(t *testing.T) {
	srv, calls := flakyServer(t, 0, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	ctx := context.Background()
	resp, err := doWithRetry(ctx, srv.Client(), time.Millisecond, getter(ctx, srv.URL))
	if err != nil {
		t.Fatalf("a 503 is a response, not an error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1", n)
	}
}

func TestDoWithRetryGivesUpAfterTwoTriesWithoutURL(t *testing.T) {
	srv, calls := flakyServer(t, 99, nil)
	ctx := context.Background()
	_, err := doWithRetry(ctx, srv.Client(), time.Millisecond, getter(ctx, srv.URL+"/v1/forecast?latitude=-37.79"))
	if err == nil {
		t.Fatal("expected an error after two dropped connections")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want exactly 2", n)
	}
	if !strings.Contains(err.Error(), "(2 tries)") {
		t.Errorf("error should say it retried: %v", err)
	}
	if strings.Contains(err.Error(), "latitude") {
		t.Errorf("error should not repeat the URL: %v", err)
	}
}

// Shutdown, or a manual run's 30 s budget running out, must not sit through the wait.
func TestDoWithRetryStopsWaitingWhenCancelled(t *testing.T) {
	srv, calls := flakyServer(t, 99, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err := doWithRetry(ctx, srv.Client(), time.Hour, getter(ctx, srv.URL))
	if err == nil {
		t.Fatal("expected an error once cancelled")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("waited %s; should have stopped at cancellation", time.Since(start))
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("calls = %d, want 1 (no retry after cancellation)", n)
	}
}

// End to end through the real provider: one dropped connection costs a short wait, not
// a failed fetch.
func TestOpenMeteoRetriesTransportFailure(t *testing.T) {
	srv, calls := flakyServer(t, 1, func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"hourly":{"time":["2026-11-15T21:00"],"cloud_cover":[10]}}`))
	})
	om := NewOpenMeteoModel("Australia/Melbourne", "ecmwf_ifs025", "ecmwf")
	om.baseURL = srv.URL
	om.retryWait = time.Millisecond

	fc, err := om.Fetch(context.Background(), testLat, testLon)
	if err != nil {
		t.Fatalf("fetch should survive one dropped connection: %v", err)
	}
	if len(fc.Hours) != 1 || fc.Hours[0].CloudTotal != 10 {
		t.Errorf("unexpected forecast: %+v", fc.Hours)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls = %d, want 2", n)
	}
}
