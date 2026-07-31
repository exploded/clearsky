# clearsky — astrophotography go/no-go

Personal app for Donvale, VIC: each evening (18:00) it checks tonight's forecast + moon
over the astronomical dark window, decides GO/NO-GO for imaging, logs the decision to
SQLite, notifies Discord + email on GO nights, and serves an HTMX web log of past nights.

After any UI change, verify it in the browser (use the chrome-devtools MCP tools if
available) before reporting done. Do not claim a fix works without exercising the flow.

## Skills
Use `go-htmx-skill` for any template/HTMX/UI work and `sqlc-sqlite` for any query work.

## Stack
Go 1.26, stdlib `net/http` (no framework), html/template + HTMX (self-hosted in
`static/`), modernc.org/sqlite (pure Go) via sqlc, `kixorz/suncalc` for darkness/moon.
Only two direct deps. Weather: Open-Meteo + yr.no (MET Norway), both free/keyless.

## Build / test / run (Windows)
```
go run .                  # serves http://localhost:8080, schedules the daily 18:00 check
go run . -test-notify     # sends a sample message to every configured channel, then exits
go test ./...
```
Copy `.env.example` → `.env` first (everything optional; Donvale defaults baked in).
The log page has a "Run tonight's check now" button for on-demand runs.

## sqlc
`sqlc generate` — reads `migrations/` + `queries/` → generated `store/` package.

## Layout
Flat `package main` at the root; the one separate package is generated `store/`.
```
main.go / config.go        entry, env config (CLEARSKY_* vars)
openmeteo.go / metno.go / multisource.go / source.go   weather providers behind Source interface
astro.go                   dark window + moon (suncalc)
decision.go                GO/NO-GO rules (usable-window search + cloud gate)
runner.go / scheduler.go   fetch→decide→persist→notify; 18:00 timer + catch-up-on-boot
notify*.go                 Discord webhook + Gmail SMTP fanout
handlers.go / proxy.go     HTMX log page; image proxy for the Tonight panel
templates/ static/ migrations/ queries/ store/
```

## Deployment
- Repo: `exploded/clearsky` (GitHub), default branch `master`.
- Push to `master` → GitHub Actions "Test and Deploy" (tests, then static Linux build
  `CGO_ENABLED=0`). Bundle is binary + deploy script only — templates/static/migrations/
  tzdata are all `go:embed`-ed.
- Prod: systemd unit `clearsky` on port **8994** (`CLEARSKY_ADDR=:8994` in
  `/var/www/clearsky/.env`; dev default is `:8080`) behind Caddy at
  `https://clearsky.mchugh.au`.

## Gotchas (verified in code / git history)
- **Everything is embedded** — template/static/migration changes require a rebuild.
- **yr.no (MET Norway) requires a descriptive User-Agent with contact info**
  (`CLEARSKY_METNO_USER_AGENT`); requests without one get rejected.
- Default source mode is `agreement`: Open-Meteo AND yr.no must BOTH be clear for a GO.
  Adding a provider = one new file implementing `Source`.
- **The decision is made on the "usable window", not the whole night.** `Evaluate`
  finds the longest contiguous run of hours that each pass the per-hour cloud/rain
  gate, then requires that run to be ≥ `MIN_USABLE_HOURS` (3) and average ≤
  `CLOUD_AVG_MAX_PCT`. Do NOT reintroduce whole-night peak gates — they made every
  night hostage to its worst hour (a clear evening + overcast 3am scored 37/NO-GO).
  The night-wide `CloudSummary`/`RainSummary` are still persisted, for display only.
- Cloud caps (`CLOUD_MAX/LOW/MID_MAX_PCT`) are **per-hour** gates, so they are looser
  than the old night-peak values; the window average carries the strictness.
- Rain is handled per hour (a raining hour is simply not usable) plus a "pack up by"
  note when rain lands within `RAIN_MARGIN_HOURS` after the window. Rain at 04:00 no
  longer vetoes a 20:00 session.
- `HoursWithin` floors the start to the local hour so an hourly bucket that *overlaps*
  dusk is kept — a strict compare silently dropped the first hour of nearly every
  night (dusk 19:02 discarding all of 19:00).
- **Moon data never affects the decision** — it is display-only (`MoonInfo.Note()`
  captions what the moon means for target choice). All thresholds are tunable via
  `CLEARSKY_*` env vars (no recompile).
- Tonight-panel images are **proxied through our own origin** (`proxy.go`, commit
  adae3ab) because Skippy Sky serves over plain HTTP — direct embedding is mixed
  content under HTTPS. They are eyeball-only, never parsed into the decision.
- The deploy script must `systemctl enable` the service, not just start it — an early
  deploy missed this and the app didn't survive reboot (fixed in a227f9e).
- `POST /nights/{date}/result` and `POST /webhooks/nina` are deliberate 501 stubs
  (schema columns + `MarkImaged` query already exist for later).
- Timestamps/scheduling use `CLEARSKY_TZ` (default Australia/Melbourne); tzdata is
  embedded via `_ "time/tzdata"` so it works on Windows and the bare Linux box.
- Never commit `.env` or `*.db*` (gitignored).
