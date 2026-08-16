-- Public opt-in subscribers for GO-night alerts. The owner's own channels stay in env
-- config (CLEARSKY_DISCORD_WEBHOOK_URL / SMTP); this table is for anyone else around
-- Melbourne who wants the same nudge.
--
-- Double opt-in: a row is created on sign-up with confirmed_at NULL and a confirmation
-- link is emailed; nothing else is ever sent until that link is clicked. `token` is a
-- single random secret that authenticates both the confirm link and the unsubscribe
-- link carried in every alert. Unsubscribing deletes the row outright — there is no
-- reason to keep an address nobody wants us to hold.
--
-- last_sent_at throttles the system emails (confirmation / already-subscribed notice)
-- so the public form cannot be used to hose an address with mail.

CREATE TABLE subscribers (
  id              INTEGER PRIMARY KEY,
  email           TEXT NOT NULL UNIQUE,      -- stored lower-cased
  discord_webhook TEXT NOT NULL DEFAULT '',  -- optional; discord.com/api/webhooks/... only
  token           TEXT NOT NULL UNIQUE,      -- random, base64url; confirm + unsubscribe
  confirmed_at    INTEGER,                   -- NULL until the confirmation link is clicked
  last_sent_at    INTEGER NOT NULL DEFAULT 0,-- last confirmation/notice email (unix)
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE INDEX idx_subscribers_confirmed ON subscribers(confirmed_at);
