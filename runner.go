package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"clearsky/store"
)

// Runner orchestrates one night's evaluation: fetch forecast → compute darkness/moon
// → decide → persist → notify (on GO, once). It is safe to call repeatedly for the
// same date: the upsert overwrites the snapshot and notifications are de-duplicated
// via notified_at.
type Runner struct {
	q        *store.Queries
	src      Source
	notifier *Notifier
	cfg      Config
	loc      *time.Location
	mu       sync.Mutex // serialise runs (scheduled vs. manual /run)
}

func NewRunner(q *store.Queries, src Source, notifier *Notifier, cfg Config, loc *time.Location) *Runner {
	return &Runner{q: q, src: src, notifier: notifier, cfg: cfg, loc: loc}
}

// RunForDate evaluates the night whose evening falls on date (interpreted in the site
// timezone) and returns the decision.
func (r *Runner) RunForDate(ctx context.Context, date time.Time) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	date = date.In(r.loc)
	dateKey := date.Format("2006-01-02")

	dark := darknessWindow(date, r.cfg.Lat, r.cfg.Lon, r.loc)
	moon := moonInfo(dark, r.cfg.Lat, r.cfg.Lon, r.loc)

	fc, err := r.src.Fetch(ctx, r.cfg.Lat, r.cfg.Lon)
	if err != nil {
		return Result{}, fmt.Errorf("fetch forecast: %w", err)
	}
	hours := fc.HoursWithin(dark.Dusk, dark.Dawn)
	res := Evaluate(hours, r.cfg.Thresholds)

	// Was this night already notified? (Preserved across the upsert.)
	alreadyNotified := false
	if prior, err := r.q.GetNight(ctx, dateKey); err == nil {
		alreadyNotified = prior.NotifiedAt.Valid
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Result{}, fmt.Errorf("read prior night: %w", err)
	}

	cloudJSON, _ := json.Marshal(res.Cloud)
	rainJSON, _ := json.Marshal(res.Rain)
	hourlyJSON, _ := json.Marshal(hours)
	now := time.Now().Unix()

	if err := r.q.UpsertNight(ctx, store.UpsertNightParams{
		NightDate:    dateKey,
		Decision:     decisionCode(res.GO),
		Score:        int64(res.Score),
		Reason:       res.Reason,
		Source:       fc.Source,
		CloudSummary: string(cloudJSON),
		RainSummary:  string(rainJSON),
		HourlyJson:   string(hourlyJSON),
		DuskAt:       dark.Dusk.Unix(),
		DawnAt:       dark.Dawn.Unix(),
		MoonRiseAt:   nullUnix(moon.Rise),
		MoonSetAt:    nullUnix(moon.Set),
		MoonIllumPct: moon.IllumPct,
		CreatedAt:    now,
		UpdatedAt:    now,
	}); err != nil {
		return Result{}, fmt.Errorf("upsert night: %w", err)
	}

	slog.Info("night evaluated", "date", dateKey, "decision", decisionCode(res.GO),
		"score", res.Score, "reason", res.Reason, "source", fc.Source)

	// Notify on GO nights only, and only once per date (unless a NO-GO later flips to
	// GO — then notified_at is still null, so it will notify).
	if res.GO && !alreadyNotified && r.notifier.Enabled() {
		r.notifier.Notify(ctx, Message{
			Date: date, Source: fc.Source, Result: res, Dark: dark, Moon: moon,
		})
		if err := r.q.SetNightNotified(ctx, store.SetNightNotifiedParams{
			NotifiedAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
			UpdatedAt:  time.Now().Unix(),
			NightDate:  dateKey,
		}); err != nil {
			slog.Error("stamp notified_at", "date", dateKey, "err", err)
		}
	}

	return res, nil
}

func decisionCode(go_ bool) string {
	if go_ {
		return "GO"
	}
	return "NO_GO"
}

func nullUnix(t *time.Time) sql.NullInt64 {
	if t == nil || t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}
