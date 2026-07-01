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
type Scheduler struct {
	runner    *Runner
	q         *store.Queries
	loc       *time.Location
	hour, min int
}

func NewScheduler(runner *Runner, q *store.Queries, loc *time.Location, hour, min int) *Scheduler {
	return &Scheduler{runner: runner, q: q, loc: loc, hour: hour, min: min}
}

// Run loops until ctx is cancelled, firing the job at each next scheduled time.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next := nextFireAt(time.Now().In(s.loc), s.hour, s.min, s.loc)
		slog.Info("next scheduled run", "at", next.Format("Mon 2 Jan 15:04 MST"))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			today := time.Now().In(s.loc)
			if _, err := s.runner.RunForDate(ctx, today); err != nil {
				slog.Error("scheduled run failed", "date", today.Format("2006-01-02"), "err", err)
			}
		}
	}
}

// CatchupIfMissing runs today's job once at startup if no row exists yet — so a
// restart after the scheduled time still produces tonight's decision.
func (s *Scheduler) CatchupIfMissing(ctx context.Context) {
	today := time.Now().In(s.loc)
	key := today.Format("2006-01-02")
	if _, err := s.q.GetNight(ctx, key); err == nil {
		return // already have a decision for today
	} else if !errors.Is(err, sql.ErrNoRows) {
		slog.Error("catch-up lookup failed", "date", key, "err", err)
		return
	}
	slog.Info("catch-up: no decision for today yet, running now", "date", key)
	if _, err := s.runner.RunForDate(ctx, today); err != nil {
		slog.Error("catch-up run failed", "date", key, "err", err)
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
