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
Only two direct deps. Weather: Open-Meteo (ECMWF + GFS + ICON by name) and yr.no
(MET Norway) as an alternate, all free/keyless.

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
agreement.go               per-source verdicts (who agreed, how far apart)
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
  (`CLEARSKY_METNO_USER_AGENT`); requests without one get rejected. Only consulted
  when `CLEARSKY_SOURCE=met-no` — it is no longer part of the default agreement set.
- **Agreement sources must be INDEPENDENT models, and that is not free.** Default mode
  `agreement` is ECMWF + GFS + ICON, each fetched by name via Open-Meteo's `models=`
  (see `agreementModels` in main.go). The original pairing — Open-Meteo `best_match` +
  yr.no — was the same model twice: on 2026-08-03 they agreed within ~4% on every hour
  and jointly passed a window GFS/ICON put at 85-100% cloud, shipping a false GO. The
  pessimistic merge only filters anything if the sources can actually disagree. Before
  adding a provider, check it against the others on a marginal night — a new API is not
  a new opinion.
- **BOM is not available as a source.** Their public API forbids reuse in its own
  copyright field, carries no cloud data (icon + rain chance only), and ACCESS-G via
  Open-Meteo returns null for every field at this site. Don't re-litigate this.
- Open-Meteo returns JSON `null` for fields a model lacks, and `encoding/json` turns
  that into `0` — i.e. "perfectly clear". `openMeteoResponse` uses pointer slices and
  `Fetch` errors out when a response has no cloud data at all, so a dud model fails
  loudly instead of manufacturing a GO.
- Per-source verdicts are persisted to `nights.sources_json` (`Agreement` in
  agreement.go) and shown as an expandable cell on the log page; a split decision is
  flagged amber. The decision itself is still made on the merge — the breakdown is
  the record of whether the models actually agreed.
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
- **A failed run is retried, never skipped.** The nightly job hits free APIs at one
  fixed instant, so a provider that is sick at that instant costs *every* night rather
  than a random few. On 2026-08-04..07 Open-Meteo returned 503 to all three models at
  exactly 08:00 UTC — the 18:00 Melbourne fire time — and four consecutive nights
  recorded nothing, because the scheduler made one attempt and slept till tomorrow.
  `runWithRetry` now backs off (`CLEARSKY_RETRY_FIRST` doubling to `_MAX`) until
  `CLEARSKY_RETRY_UNTIL_HOUR`, then sends a **failure notification**. Silence used to be
  indistinguishable from a quiet NO-GO night; it no longer is.
- **Missed nights are never backfilled, and must not be.** Catch-up only ever runs
  *today*. The forecast APIs serve the present forward, so re-running an older date
  returns no hours inside that night's darkness window and would persist a fabricated
  "no usable hours" NO-GO for a night nobody observed. A missing row is the honest record.
- **Log what the provider actually said.** Non-200s carry a `bodySnippet` of the response
  body (Open-Meteo answers `{"error":true,"reason":…}`). The bare `open-meteo status 503`
  the app used to log cost days of guesswork.
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
