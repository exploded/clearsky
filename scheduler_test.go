package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNextFireAt(t *testing.T) {
	loc := mustMelbourne(t)

	// Before the fire time on the same day -> today at 18:00.
	now := time.Date(2026, 7, 1, 14, 0, 0, 0, loc)
	got := nextFireAt(now, 18, 0, loc)
	want := time.Date(2026, 7, 1, 18, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("before fire: got %v, want %v", got, want)
	}

	// After the fire time -> tomorrow at 18:00.
	now = time.Date(2026, 7, 1, 19, 30, 0, 0, loc)
	got = nextFireAt(now, 18, 0, loc)
	want = time.Date(2026, 7, 2, 18, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("after fire: got %v, want %v", got, want)
	}

	// Exactly at the fire time -> next day (strictly after now).
	now = time.Date(2026, 7, 1, 18, 0, 0, 0, loc)
	got = nextFireAt(now, 18, 0, loc)
	if !got.After(now) {
		t.Errorf("at fire: got %v not after now %v", got, now)
	}
}

// scriptedRunner fails the first failures attempts, then succeeds.
type scriptedRunner struct {
	failures int
	calls    int
	err      error
}

func (r *scriptedRunner) SourceName() string { return "test-source" }

func (r *scriptedRunner) RunForDate(context.Context, time.Time) (Result, error) {
	r.calls++
	if r.calls <= r.failures {
		return Result{}, r.err
	}
	return Result{GO: true}, nil
}

// testScheduler wires a scheduler to a fake clock: sleeping advances the clock instead
// of blocking, so an hour of backoff costs nothing to exercise.
func testScheduler(t *testing.T, runner nightRunner, start time.Time) (*Scheduler, *[]time.Duration, *fakeChannel) {
	t.Helper()
	loc := start.Location()
	ch := &fakeChannel{}
	s := NewScheduler(runner, nil, &Notifier{channels: []channel{ch}}, loc, 18, 0,
		RetryPolicy{First: 2 * time.Minute, Max: 30 * time.Minute, UntilHour: 23})

	clock := start
	var slept []time.Duration
	s.now = func() time.Time { return clock }
	s.sleep = func(ctx context.Context, d time.Duration) bool {
		if ctx.Err() != nil {
			return false
		}
		slept = append(slept, d)
		clock = clock.Add(d)
		return true
	}
	return s, &slept, ch
}

// A transient provider outage must not cost the night. This is the Aug 2026 regression:
// Open-Meteo 503'd all three models at the 18:00 fire time on four consecutive nights and
// the one-shot scheduler recorded nothing at all.
func TestRunWithRetrySucceedsAfterTransientFailure(t *testing.T) {
	loc := mustMelbourne(t)
	start := time.Date(2026, 8, 7, 18, 0, 0, 0, loc)
	runner := &scriptedRunner{failures: 2, err: errors.New("open-meteo status 503")}
	s, slept, ch := testScheduler(t, runner, start)

	s.runWithRetry(context.Background(), start)

	if runner.calls != 3 {
		t.Errorf("expected 3 attempts (2 failures then success), got %d", runner.calls)
	}
	want := []time.Duration{2 * time.Minute, 4 * time.Minute}
	if len(*slept) != len(want) {
		t.Fatalf("expected %d backoffs, got %v", len(want), *slept)
	}
	for i, d := range want {
		if (*slept)[i] != d {
			t.Errorf("backoff %d: got %v, want %v (should double)", i, (*slept)[i], d)
		}
	}
	if ch.sent != 0 {
		t.Errorf("a run that eventually succeeded must not send a failure alert (sent %d)", ch.sent)
	}
}

// A sustained outage must give up at the deadline and say so out loud.
func TestRunWithRetryGivesUpAtDeadlineAndNotifies(t *testing.T) {
	loc := mustMelbourne(t)
	start := time.Date(2026, 8, 7, 18, 0, 0, 0, loc)
	runner := &scriptedRunner{failures: 1000, err: errors.New("all sources failed: [ecmwf gfs icon]")}
	s, slept, ch := testScheduler(t, runner, start)

	s.runWithRetry(context.Background(), start)

	if runner.calls < 2 {
		t.Errorf("expected multiple attempts before giving up, got %d", runner.calls)
	}
	// 18:00 -> 23:00 with 2m doubling to a 30m cap is well under 20 attempts; the point
	// is that it terminates rather than retrying into the small hours.
	if runner.calls > 20 {
		t.Errorf("retried %d times — deadline is not bounding the ladder", runner.calls)
	}
	total := time.Duration(0)
	for _, d := range *slept {
		total += d
		if d > 30*time.Minute {
			t.Errorf("backoff %v exceeded the configured cap", d)
		}
	}
	if total > 5*time.Hour {
		t.Errorf("retried across %v, past the 23:00 deadline", total)
	}
	if ch.sent != 1 {
		t.Fatalf("expected exactly one failure alert, got %d", ch.sent)
	}
	if !strings.Contains(ch.lastSub, "FAILED") {
		t.Errorf("failure subject should be unmistakable, got %q", ch.lastSub)
	}
	if !strings.Contains(ch.lastBody, "ecmwf gfs icon") {
		t.Errorf("failure body should carry the underlying error, got %q", ch.lastBody)
	}
}

// Shutdown mid-backoff must not fire a spurious "check failed" alert.
func TestRunWithRetryStopsOnCancel(t *testing.T) {
	loc := mustMelbourne(t)
	start := time.Date(2026, 8, 7, 18, 0, 0, 0, loc)
	runner := &scriptedRunner{failures: 1000, err: errors.New("boom")}
	s, _, ch := testScheduler(t, runner, start)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.runWithRetry(ctx, start)

	if runner.calls != 1 {
		t.Errorf("cancelled context should stop after the in-flight attempt, got %d calls", runner.calls)
	}
	if ch.sent != 0 {
		t.Errorf("shutdown is not a forecast failure; expected no alert, got %d", ch.sent)
	}
}

// A retry window configured at or before the fire time would silently disable retries.
func TestRetryDeadlineFallsBackWhenMisconfigured(t *testing.T) {
	loc := mustMelbourne(t)
	date := time.Date(2026, 8, 7, 18, 0, 0, 0, loc)
	s := NewScheduler(&scriptedRunner{}, nil, &Notifier{}, loc, 18, 0,
		RetryPolicy{First: time.Minute, Max: time.Minute, UntilHour: 6}) // 06:00 < 18:00

	got := s.retryDeadline(date)
	want := time.Date(2026, 8, 7, 23, 0, 0, 0, loc) // fire + 5h
	if !got.Equal(want) {
		t.Errorf("misconfigured UntilHour: got %v, want fallback %v", got, want)
	}
}

func TestGetenvDuration(t *testing.T) {
	t.Setenv("CLEARSKY_TEST_DUR", "90s")
	if got := getenvDuration("CLEARSKY_TEST_DUR", time.Minute); got != 90*time.Second {
		t.Errorf("duration string: got %v", got)
	}
	t.Setenv("CLEARSKY_TEST_DUR", "5") // bare number means minutes
	if got := getenvDuration("CLEARSKY_TEST_DUR", time.Minute); got != 5*time.Minute {
		t.Errorf("bare number: got %v", got)
	}
	t.Setenv("CLEARSKY_TEST_DUR", "nonsense")
	if got := getenvDuration("CLEARSKY_TEST_DUR", 7*time.Minute); got != 7*time.Minute {
		t.Errorf("garbage should fall back to the default: got %v", got)
	}
}
