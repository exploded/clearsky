-- Astrophotography nightly decisions for Donvale, AU.
-- One row per evening, keyed by local date. INTEGER unix-seconds timestamps,
-- TEXT enum + CHECK, JSON summaries as TEXT. The `source` column records which
-- weather provider supplied the cloud/rain data (it can vary as providers are added).
-- The nullable columns at the bottom are the results-logging / NINA seam: present
-- but unused in v1, so those features are later handler-only additions (no migration).

CREATE TABLE nights (
  night_date     TEXT PRIMARY KEY,             -- 'YYYY-MM-DD' local (the evening)
  decision       TEXT NOT NULL CHECK (decision IN ('GO','NO_GO')),
  score          INTEGER NOT NULL DEFAULT 0,   -- 0..100 cloud-clearness, info/ranking only
  reason         TEXT NOT NULL DEFAULT '',     -- human-readable "why"
  source         TEXT NOT NULL DEFAULT '',     -- which weather provider produced the data

  cloud_summary  TEXT NOT NULL DEFAULT '{}',   -- {"max","avg","maxLow","maxMid","maxHigh"}
  rain_summary   TEXT NOT NULL DEFAULT '{}',   -- {"totalMm","maxProbPct","anyHour"}
  hourly_json    TEXT NOT NULL DEFAULT '[]',   -- darkness-window hourly points (detail view)

  dusk_at        INTEGER NOT NULL,             -- astronomical dusk (unix)
  dawn_at        INTEGER NOT NULL,             -- astronomical dawn next morning (unix)
  moon_rise_at   INTEGER,                      -- nullable (may not rise in window) — info only
  moon_set_at    INTEGER,                      -- nullable — info only
  moon_illum_pct REAL NOT NULL DEFAULT 0,      -- 0..100 — info only, never affects decision

  notified_at    INTEGER,                      -- set when a GO notification was sent
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,

  -- ==== later seam (nullable / defaulted; unused in v1) ====
  imaged         INTEGER,                      -- nullable bool 0/1
  image_result   TEXT NOT NULL DEFAULT '',     -- outcome notes
  image_url      TEXT NOT NULL DEFAULT '',     -- link to deepspaceplace.com/images
  nina_json      TEXT NOT NULL DEFAULT ''      -- raw NINA payload when it lands
);

CREATE INDEX idx_nights_date_desc ON nights(night_date DESC);
