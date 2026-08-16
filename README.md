# clearsky 🔭

A personal astrophotography **go / no-go** app for Donvale, VIC. Each evening it checks
tonight's forecast + moon, decides whether conditions suit imaging, logs the decision,
and (on GO nights) pings Discord + email. It also serves a web log of past nights, where
other Melbourne imagers can opt in to the same GO alerts (see [Subscribers](#subscribers)).

## How it decides

Looks only at the **astronomical dark window** (dusk → dawn) for tonight:

- **Rain veto (hard NO-GO):** any measurable rain, any hour over the rain-probability
  cap, or too much total precip.
- **Cloud gate ("mostly clear"):** low average cloud, bounded peak, with tighter caps on
  low/mid cloud (worse for imaging) than thin high cloud.
- **Moon:** rise/set + illumination are shown for information only — they **never**
  change the decision.

All thresholds are environment variables (see `.env.example`) — tune without recompiling.

## Weather sources

`CLEARSKY_SOURCE` selects the data behind the decision:

- **`agreement`** (default) — fetches **Open-Meteo** and **yr.no** (MET Norway API, free,
  keyless) and merges them pessimistically, so it's only a GO when *both* agree the sky is
  clear and dry. The stored `source` reads e.g. `open-meteo+met-no`.
- **`open-meteo`** / **`met-no`** — use a single provider.

Adding another provider is one new file implementing the `Source` interface.

The log page also embeds a **visual "Tonight" panel** — the Clear Outside forecast image,
Skippy Sky cloud map, and yr.no meteogram — for an eyeball sanity-check (these images are
*not* parsed into the decision; Clear Outside and your deepspaceplace.com/weather page are
HTML/image only). Note the Skippy image is served over HTTP, so it may be blocked as mixed
content if you deploy clearsky over HTTPS.

## Run

```sh
cp .env.example .env      # then fill in notification creds (optional)
go run .                  # serves http://localhost:8080, runs the 6 PM check daily
```

- Open `/` for the log. The **“Run tonight's check now”** button runs it on demand.
- Test your notification setup any time: `go run . -test-notify` sends a sample message
  to every configured owner channel; `go run . -test-email you@example.com` sends one
  subscriber-style alert through SES.

## Subscribers

The log page has a **Get GO alerts** form: an email address plus an optional Discord
webhook. It is loudly labelled **Melbourne only** — the forecast is for one site in
Donvale, and a GO here says nothing about anyone else's sky.

- **Double opt-in.** Sign-up creates a pending row and emails a confirmation link (good
  for 48 h). Nothing else is ever sent until the link is clicked. Confirming with a
  webhook posts a hello to it, so a mistyped URL is caught on the spot.
- **Unsubscribe in every email** — a footer link plus `List-Unsubscribe` /
  `List-Unsubscribe-Post` headers so Gmail & co. show their own one-click button.
  Unsubscribing deletes the row.
- **Sent through AWS SES** (v2 API over HTTPS, SigV4 signed in-process — no SDK).
  Configure `CLEARSKY_SES_*` in `.env`; the form only appears when SES is set up. New
  SES accounts start in the sandbox (verified recipients only) — request production
  access before opening the form to strangers.
- **Abuse guards on the public form:** per-IP rate limit, a cooldown on system emails to
  any one address, honeypot field, a cap (`CLEARSKY_MAX_SUBSCRIBERS`), stale-pending
  purge, and webhooks restricted to `discord.com/api/webhooks/…`. The form's response
  never reveals whether an address is already on the list.
- Subscribers get **GO alerts only** — not NO-GO nights, and not the owner's
  "check failed" ops alerts. The owner channels get a short "new subscriber" ping
  each time someone confirms.
- Local dev: `CLEARSKY_MAIL_DRY_RUN=true` enables the flow without SES and prints the
  emails (with their confirm/unsubscribe links) to the log.

## Stack

Go + stdlib `net/http` (no web framework) + `modernc.org/sqlite` (CGO-free) + sqlc +
HTMX. Only two third-party modules: `modernc.org/sqlite` and `github.com/kixorz/suncalc`.
Weather comes from the free [Open-Meteo](https://open-meteo.com) API (no key).

## Layout

Flat `package main` at the root; the one separate package is generated `store/` (sqlc).

| File | Responsibility |
|------|----------------|
| `openmeteo.go` / `source.go` | Weather provider (behind a `Source` interface for adding more) |
| `astro.go` | Darkness window + moon, via suncalc |
| `decision.go` | GO/NO-GO rules (rain veto + cloud gate) |
| `runner.go` | Fetch → decide → persist → notify (once per GO night) |
| `scheduler.go` | 18:00 timer + catch-up-on-boot |
| `notify*.go` | Owner fanout: Discord webhook + email (via SES) |
| `mail.go` / `ses.go` | `Mailer` interface, MIME builder, AWS SES v2 + SigV4 signer |
| `subscribers.go` | Public opt-in list: sign-up, confirm, unsubscribe, broadcast |
| `handlers.go` / `handlers_subscribe.go` + `templates/` | HTMX log page + subscribe routes |

## Regenerate DB code after editing SQL

```sh
sqlc generate     # reads sqlc.yaml, migrations/, queries/ -> store/
```

## Deploy

Hosted at [clearsky.mchugh.au](https://clearsky.mchugh.au) — systemd on Linode
Debian behind Caddy (TLS auto-provisioned), deployed via GitHub Actions.

```sh
git push origin master
```

The `Test and Deploy` workflow (`.github/workflows/deploy.yml`) runs the tests,
cross-compiles a static Linux binary (`CGO_ENABLED=0`), SCPs it to the server,
and runs `scripts/deploy-clearsky` as root to install and restart the service.
Because templates/static/migrations/tzdata are all `go:embed`-ed, only the
binary is shipped.

First-time server provisioning is a one-off `sudo bash scripts/server-setup.sh`
(creates `/var/www/clearsky`, the systemd unit listening on `:8994`, a `.env`
template, the sudoers entry, and prints the Caddy site block to add). It reuses
the shared `deploy` user and `DEPLOY_HOST`/`DEPLOY_USER`/`DEPLOY_SSH_KEY` Actions
secrets from the other projects.

## Later (seams already in place)

- **Results logging** — did I image / outcome / link to deepspaceplace.com/images
  (`nights.imaged/image_result/image_url` columns + `MarkImaged` query exist;
  `POST /nights/{date}/result` is a 501 stub).
- **NINA ingest** — `POST /webhooks/nina` (501 stub) → `nights.nina_json`.
- **More weather sources** — add a `Source` impl; the chosen provider is recorded per
  night in `nights.source`.
