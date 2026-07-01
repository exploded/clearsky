-- name: UpsertNight :exec
-- Insert or overwrite a night's decision + snapshot. Keyed by night_date, so a
-- re-run for the same date updates the row rather than inserting a duplicate.
-- created_at and notified_at are preserved on conflict.
INSERT INTO nights (
  night_date, decision, score, reason, source,
  cloud_summary, rain_summary, hourly_json,
  dusk_at, dawn_at, moon_rise_at, moon_set_at, moon_illum_pct,
  created_at, updated_at
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(night_date) DO UPDATE SET
  decision       = excluded.decision,
  score          = excluded.score,
  reason         = excluded.reason,
  source         = excluded.source,
  cloud_summary  = excluded.cloud_summary,
  rain_summary   = excluded.rain_summary,
  hourly_json    = excluded.hourly_json,
  dusk_at        = excluded.dusk_at,
  dawn_at        = excluded.dawn_at,
  moon_rise_at   = excluded.moon_rise_at,
  moon_set_at    = excluded.moon_set_at,
  moon_illum_pct = excluded.moon_illum_pct,
  updated_at     = excluded.updated_at;

-- name: SetNightNotified :exec
UPDATE nights SET notified_at = ?, updated_at = ? WHERE night_date = ?;

-- name: GetNight :one
SELECT * FROM nights WHERE night_date = ?;

-- name: ListNights :many
SELECT * FROM nights ORDER BY night_date DESC LIMIT ? OFFSET ?;

-- name: ListNightsBefore :many
SELECT * FROM nights WHERE night_date < ? ORDER BY night_date DESC LIMIT ?;

-- name: MarkImaged :exec
UPDATE nights
SET imaged = ?, image_result = ?, image_url = ?, updated_at = ?
WHERE night_date = ?;

-- name: SetNightNina :exec
UPDATE nights SET nina_json = ?, updated_at = ? WHERE night_date = ?;
