package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"clearsky/store"
)

// Scheduler fires the nightly job at a fixed local time (default 18:00 Melbourne).
// It is an in-process timer — no OS cron, no dependency. Manual "run now" is handled
// directly by the HTTP handler calling Runner.RunForDate, so no trigger channel is
// needed here.
//
// A failed run is retried (see RetryPolicy) rather than skipped to the next day. The
// original design fired once and logged the error, which meant an API that was down at
// the fire time silently produced no decision at all — indistinguishable, from the log
// page, from a night nobody asked about.
// nightRunner is the slice of *Runner the scheduler depends on. Narrowing it to an
// interface lets the retry ladder be tested against a scripted sequence of failures
// without a database or a live forecast API behind it.
type nightRunner interface {
	RunForDate(ctx context.Context, date time.Time) (Result, error)
	SourceName() string
}

type Scheduler struct {
	runner    nightRunner
	q         *store.Queries
	notifier  *Notifier
	loc       *time.Location
	hour, min int
	retry     RetryPolicy

	// now and sleep are injected so the retry ladder is testable without waiting out
	// real backoff. Production wiring is time.Now and a ctx-aware timer.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) bool
}

func NewScheduler(runner nightRunner, q *store.Queries, notifier *Notifier, loc *time.Location, hour, min int, retry RetryPolicy) *Scheduler {
	return &Scheduler{
		runner:   runner,
		q:        q,
		notifier: notifier,
		loc:      loc,
		hour:     hour,
		min:      min,
		retry:    retry,
		now:      func() time.Time { return time.Now().In(loc) },
		sleep:    sleepCtx,
	}
}

// Run loops until ctx is cancelled, firing the job at each next scheduled time.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next := nextFireAt(s.now(), s.hour, s.min, s.loc)
		slog.Info("next scheduled run", "at", next.Format("Mon 2 Jan 15:04 MST"))
		timer := time.NewTimer(next.Sub(s.now()))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.runWithRetry(ctx, s.now())
		}
	}
}

// CatchupIfMissing runs today's job once at startup if no row exists yet — so a
// restart after the scheduled time still produces tonight's decision.
//
// Only TODAY is ever caught up. Backfilling a missed night is impossible and would be
// worse than the gap: the forecast APIs only serve the present forward, so re-running an
// older date returns no hours inside that night's darkness window, and the run would
// cheerfully persist a fabricated "no usable hours" NO-GO for a night nobody observed.
// A missing row is an honest record that the check never completed.
func (s *Scheduler) CatchupIfMissing(ctx context.Context) {
	today := s.now()
	key := today.Format("2006-01-02")
	if _, err := s.q.GetNight(ctx, key); err == nil {
		return // already have a decision for today
	} else if !errors.Is(err, sql.ErrNoRows) {
		slog.Error("catch-up lookup failed", "date", key, "err", err)
		return
	}
	slog.Info("catch-up: no decision for today yet, running now", "date", key)
	s.runWithRetry(ctx, today)
}

// runWithRetry evaluates the night, retrying with exponential backoff until it succeeds,
// the retry deadline passes, or ctx is cancelled. On final failure it notifies, because
// the failure mode that actually bit was silence: four nights in a row produced nothing
// but a WARN in the journal, and the gap was only noticed by looking at the log page.
func (s *Scheduler) runWithRetry(ctx context.Context, date time.Time) {
	dateKey := date.Format("2006-01-02")
	deadline := s.retryDeadline(date)
	delay := s.retry.First

	for attempt := 1; ; attempt++ {
		_, err := s.runner.RunForDate(ctx, date)
		if err == nil {
			if attempt > 1 {
				slog.Info("run succeeded after retry", "date", dateKey, "attempts", attempt)
			}
			return
		}
		if ctx.Err() != nil {
			return // shutting down; not a forecast failure worth reporting
		}

		// Only retry if the next attempt still lands before the deadline — a retry at
		// 23:30 answers a question about a night that is already half over.
		if next := s.now().Add(delay); next.After(deadline) {
			slog.Error("giving up on tonight's run", "date", dateKey,
				"attempts", attempt, "deadline", deadline.Format("15:04"), "err", err)
			s.notifier.NotifyFailure(ctx, FailureMessage{
				Date: date, Attempts: attempt, Err: err, Source: s.runner.SourceName(),
			})
			return
		}

		slog.Warn("run failed, retrying", "date", dateKey, "attempt", attempt,
			"retry_in", delay.String(), "err", err)
		if !s.sleep(ctx, delay) {
			return // cancelled mid-backoff
		}
		if delay *= 2; delay > s.retry.Max {
			delay = s.retry.Max
		}
	}
}

// retryDeadline is UntilHour local on the run's own date. A misconfigured hour at or
// before the fire time would disable retries entirely, so it falls back to five hours
// past the fire time.
func (s *Scheduler) retryDeadline(date time.Time) time.Time {
	fire := time.Date(date.Year(), date.Month(), date.Day(), s.hour, s.min, 0, 0, s.loc)
	deadline := time.Date(date.Year(), date.Month(), date.Day(), s.retry.UntilHour, 0, 0, 0, s.loc)
	if !deadline.After(fire) {
		return fire.Add(5 * time.Hour)
	}
	return deadline
}

// sleepCtx waits for d, reporting false if ctx was cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// nextFireAt returns the next occurrence of hour:min in loc, strictly after now.
func nextFireAt(now time.Time, hour, min int, loc *time.Location) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, loc)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}
