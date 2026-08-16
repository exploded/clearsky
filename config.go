package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds runtime configuration, sourced entirely from environment variables
// (prefix CLEARSKY_) so no secrets live in the repo. See .env.example.
type Config struct {
	Addr     string // HTTP listen address, e.g. ":8080"
	DB       string // sqlite file path
	BaseURL  string // public base URL for links in templates/notifications
	LogLevel string // slog level: debug | info | warn | error

	TZ             string  // scheduler + darkness timezone, e.g. "Australia/Melbourne"
	RunHour        int     // daily job fire hour, local time
	RunMinute      int     // daily job fire minute, local time
	Lat            float64 // observing site latitude
	Lon            float64 // observing site longitude
	CatchupOnStart bool    // on boot, run today's job if no row exists yet

	Retry RetryPolicy

	// Weather source selection: "agreement" (Open-Meteo AND yr.no must both be clear),
	// "open-meteo", or "met-no".
	Source         string
	MetnoUserAgent string // required descriptive UA for the MET Norway API

	// Visual "Tonight" panel image URLs (embedded on the log page for eyeballing).
	ClearOutsideImg string
	YrMeteogramURL  string
	SkippyImg       string
	DeepSpaceURL    string

	Thresholds Thresholds

	// Owner notifications. An empty webhook disables Discord; EmailTo is sent via SES
	// and so needs the SES block below.
	DiscordWebhookURL string
	EmailTo           string

	// All email goes out through AWS SES: the owner's own alerts (EmailTo) and the
	// public subscribers (opt-in GO alerts for other Melbourne imagers). All four SES
	// values must be set for email to work and for the subscribe form to appear; the
	// list feature is otherwise dormant. MaxSubscribers caps the table so the public
	// form cannot run up the bill.
	SESRegion          string
	SESAccessKeyID     string
	SESSecretAccessKey string
	SESFrom            string // verified SES identity, e.g. "clearsky <alerts@mchugh.au>"
	MaxSubscribers     int
	MailDryRun         bool // dev only: log subscriber emails instead of sending via SES
}

// SubscribersEnabled reports whether the public subscribe flow is switched on: SES is
// fully configured, or the dev dry-run switch is set.
func (c Config) SubscribersEnabled() bool {
	if c.MailDryRun {
		return true
	}
	return c.SESRegion != "" && c.SESAccessKeyID != "" && c.SESSecretAccessKey != "" && c.SESFrom != ""
}

// mailer is the one email transport: SES, or the log in dry-run.
func (c Config) mailer() Mailer {
	if c.MailDryRun {
		return logMailer{}
	}
	return newSESMailer(c)
}

// RetryPolicy governs what happens when a night's run fails. The forecast APIs are
// free and occasionally unavailable, and the nightly job hits them at one fixed instant
// every day — so a provider that is reliably sick at that instant costs every night, not
// a random few. Between 4 and 7 Aug 2026 Open-Meteo returned 503 to all three models at
// exactly 08:00 UTC (= the 18:00 Melbourne fire time) and four consecutive nights were
// lost, because the scheduler made one attempt and slept until tomorrow.
//
// Retries back off (First, doubling, capped at Max) and stop at UntilHour local — late
// enough to outlast a long outage, early enough that the answer still describes a night
// you could act on.
type RetryPolicy struct {
	First     time.Duration // wait before the first retry
	Max       time.Duration // ceiling on the doubling backoff
	UntilHour int           // stop retrying at this local hour (24h clock)
}

// Thresholds are the tunable decision rules (all env-driven). Moon is never here —
// it is informational only and never affects the GO/NO-GO decision.
//
// The cloud caps are applied PER HOUR, gating which hours count as imageable; the
// average cap then applies to the resulting usable window. They are deliberately
// looser than a whole-night peak cap would be — a single marginal hour inside an
// otherwise clear run should not end the night.
type Thresholds struct {
	RainMmVetoHour  float64 // per-hour precip veto (mm)
	RainProbVetoPct int     // per-hour precip probability veto (%)
	RainMmTotalVeto float64 // usable-window total precip veto (mm)
	RainMarginHours int     // hours after the window to scan for a "pack up by" warning
	MinUsableHours  int     // shortest usable window worth a GO
	CloudAvgMaxPct  int     // mean total cloud over the usable window (%)
	CloudMaxPct     int     // per-hour total cloud cap (%)
	CloudLowMaxPct  int     // per-hour low cloud cap (%)
	CloudMidMaxPct  int     // per-hour mid cloud cap (%)
	VisibilityMinM  int     // soft haze note threshold (m); no veto
}

func FromEnv() Config {
	lat := getenvFloat("CLEARSKY_LAT", -37.79)
	lon := getenvFloat("CLEARSKY_LON", 145.18)
	return Config{
		Addr:           getenv("CLEARSKY_ADDR", ":8080"),
		DB:             getenv("CLEARSKY_DB", "clearsky.db"),
		BaseURL:        strings.TrimRight(getenv("CLEARSKY_BASE_URL", "http://localhost:8080"), "/"),
		LogLevel:       getenv("CLEARSKY_LOG_LEVEL", "info"),
		TZ:             getenv("CLEARSKY_TZ", "Australia/Melbourne"),
		RunHour:        getenvInt("CLEARSKY_RUN_HOUR", 18),
		RunMinute:      getenvInt("CLEARSKY_RUN_MINUTE", 0),
		Lat:            lat,
		Lon:            lon,
		CatchupOnStart: getenvBool("CLEARSKY_CATCHUP_ON_START", true),
		Retry: RetryPolicy{
			First:     getenvDuration("CLEARSKY_RETRY_FIRST", 2*time.Minute),
			Max:       getenvDuration("CLEARSKY_RETRY_MAX", 30*time.Minute),
			UntilHour: getenvInt("CLEARSKY_RETRY_UNTIL_HOUR", 23),
		},
		Source:         getenv("CLEARSKY_SOURCE", "agreement"),
		MetnoUserAgent: getenv("CLEARSKY_METNO_USER_AGENT", "clearsky-astro/1.0 (+https://deepspaceplace.com)"),
		// ClearOutside serves a public forecast PNG keyed by lat/lon (2 decimals).
		ClearOutsideImg: getenv("CLEARSKY_CLEAROUTSIDE_IMG",
			fmt.Sprintf("https://clearoutside.com/forecast_image_large/%.2f/%.2f/forecast.png", lat, lon)),
		YrMeteogramURL: getenv("CLEARSKY_YR_METEOGRAM", "https://www.yr.no/en/content/2-2158177/meteogram.svg"),
		SkippyImg:      getenv("CLEARSKY_SKIPPY_IMG", "https://www.skippysky.com.au/Melbourne/cloud_total/melb_006_cct.png"),
		DeepSpaceURL:   getenv("CLEARSKY_DEEPSPACE_URL", "https://deepspaceplace.com/weather"),
		Thresholds: Thresholds{
			RainMmVetoHour:  getenvFloat("CLEARSKY_RAIN_MM_VETO_HOUR", 0.0),
			RainProbVetoPct: getenvInt("CLEARSKY_RAIN_PROB_VETO_PCT", 35),
			RainMmTotalVeto: getenvFloat("CLEARSKY_RAIN_MM_TOTAL_VETO", 0.2),
			RainMarginHours: getenvInt("CLEARSKY_RAIN_MARGIN_HOURS", 2),
			MinUsableHours:  getenvInt("CLEARSKY_MIN_USABLE_HOURS", 3),
			CloudAvgMaxPct:  getenvInt("CLEARSKY_CLOUD_AVG_MAX_PCT", 25),
			CloudMaxPct:     getenvInt("CLEARSKY_CLOUD_MAX_PCT", 40),
			CloudLowMaxPct:  getenvInt("CLEARSKY_CLOUD_LOW_MAX_PCT", 30),
			CloudMidMaxPct:  getenvInt("CLEARSKY_CLOUD_MID_MAX_PCT", 30),
			VisibilityMinM:  getenvInt("CLEARSKY_VIS_MIN_M", 20000),
		},
		DiscordWebhookURL: getenv("CLEARSKY_DISCORD_WEBHOOK_URL", ""),
		EmailTo:           getenv("CLEARSKY_EMAIL_TO", ""),

		SESRegion:          getenv("CLEARSKY_SES_REGION", "ap-southeast-2"),
		SESAccessKeyID:     getenv("CLEARSKY_SES_ACCESS_KEY_ID", ""),
		SESSecretAccessKey: getenv("CLEARSKY_SES_SECRET_ACCESS_KEY", ""),
		SESFrom:            getenv("CLEARSKY_SES_FROM", ""),
		MaxSubscribers:     getenvInt("CLEARSKY_MAX_SUBSCRIBERS", 200),
		MailDryRun:         getenvBool("CLEARSKY_MAIL_DRY_RUN", false),
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// getenvDuration parses a Go duration string ("90s", "2m30s"). A bare number is read as
// minutes, since every duration this app configures is on that scale.
func getenvDuration(k string, def time.Duration) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Minute
	}
	return def
}

func getenvFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func getenvBool(k string, def bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
