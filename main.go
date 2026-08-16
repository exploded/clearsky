// Command clearsky is a personal astrophotography "go/no-go" app for Donvale, AU.
// A single long-running binary that each evening checks tonight's weather + moon,
// decides whether conditions suit astrophotography, logs the decision to SQLite,
// notifies on GO nights (Discord + email), and serves an HTMX log of past nights.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "time/tzdata" // embed the tz database so Australia/Melbourne loads on Windows

	"clearsky/store"
)

func main() {
	testNotify := flag.Bool("test-notify", false, "send a sample notification to all configured owner channels and exit")
	testEmail := flag.String("test-email", "", "send a sample subscriber GO alert via SES to this address and exit")
	flag.Parse()

	if err := LoadDotEnv(); err != nil {
		// Not fatal — a missing .env is fine; real env vars still apply.
		log.Printf("reading .env: %v", err)
	}
	cfg := FromEnv()
	setupLogging(cfg.LogLevel)

	loc, err := time.LoadLocation(cfg.TZ)
	if err != nil {
		fatal("load timezone", err)
	}

	// -test-email: verify the SES setup by sending one subscriber-style GO alert (with a
	// dummy unsubscribe link) to a single address. Never touches real subscribers.
	if *testEmail != "" {
		if !cfg.SubscribersEnabled() {
			fatal("test-email", errors.New("SES not configured (set CLEARSKY_SES_ACCESS_KEY_ID / _SECRET_ACCESS_KEY / _FROM)"))
		}
		to, err := normalizeEmail(*testEmail)
		if err != nil {
			fatal("test-email", err)
		}
		subs := NewSubscriptions(nil, cfg.mailer(), cfg)
		m := demoMessage(loc)
		e := subs.alertEmail(store.Subscriber{Email: to, Token: "TEST-TOKEN"}, m.Subject(), m.Body())
		if err := subs.mailer.Send(context.Background(), e); err != nil {
			fatal("test-email", err)
		}
		slog.Info("test subscriber alert sent", "via", subs.mailer.name(), "to", to, "from", cfg.SESFrom, "region", cfg.SESRegion)
		return
	}

	// -test-notify: verify webhook / SES setup without a database or scheduler.
	if *testNotify {
		notifier := NewNotifier(cfg, nil)
		if !notifier.Enabled() {
			fatal("test-notify", errNoChannels)
		}
		notifier.Notify(context.Background(), demoMessage(loc))
		// Also send the failure alert. It only fires during a provider outage, which is
		// exactly when you cannot afford to discover the channel was misconfigured.
		notifier.NotifyFailure(context.Background(), FailureMessage{
			Date: time.Now().In(loc), Attempts: 5, Source: "test",
			Err: errors.New("TEST MESSAGE — fetch forecast: all sources failed: [ecmwf gfs icon]"),
		})
		slog.Info("test notifications dispatched (one GO sample, one failure sample)")
		return
	}

	database, err := openDB(cfg.DB)
	if err != nil {
		fatal("open db", err)
	}
	defer database.Close()
	if err := migrate(database); err != nil {
		fatal("migrate", err)
	}
	q := store.New(database)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	source := buildSource(cfg)
	var subs *Subscriptions
	if cfg.SubscribersEnabled() {
		subs = NewSubscriptions(q, cfg.mailer(), cfg)
	}
	notifier := NewNotifier(cfg, subs)
	runner := NewRunner(q, source, notifier, cfg, loc)
	scheduler := NewScheduler(runner, q, notifier, loc, cfg.RunHour, cfg.RunMinute, cfg.Retry)

	slog.Info("weather source", "mode", cfg.Source, "name", source.Name())
	slog.Info("retry policy", "first", cfg.Retry.First.String(), "max", cfg.Retry.Max.String(),
		"until_hour", cfg.Retry.UntilHour)
	slog.Info("notifications", "enabled", notifier.Enabled(), "owner_channels", len(notifier.channels),
		"subscribers", subs != nil, "ses_region", cfg.SESRegion)

	// Catch up on today's decision if we missed the scheduled time, then run the
	// daily scheduler for the process lifetime.
	if cfg.CatchupOnStart {
		go scheduler.CatchupIfMissing(ctx)
	}
	go scheduler.Run(ctx)

	app, err := NewApp(q, runner, subs, loc, cfg)
	if err != nil {
		fatal("templates", err)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           app.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("clearsky listening", "addr", cfg.Addr, "base_url", cfg.BaseURL, "site", loc.String())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("listen", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// agreementModels are the sources behind the default "agreement" mode: three global
// NWP models from three different meteorological centres, each fetched by name.
//
// They are chosen for INDEPENDENCE, not accuracy. The previous pairing (Open-Meteo's
// best_match blend + yr.no) was two views of one model — on 2026-08-03 they agreed
// within ~4% on every hour of the night and jointly passed a window that GFS put at
// 92-100% cloud. Requiring three genuinely separate models to agree is what makes the
// pessimistic merge mean something.
//
// BOM's own ACCESS-G is deliberately absent: Open-Meteo carries it but returns null
// for every field at this site, and BOM's public API has no cloud data and is not
// licensed for reuse.
var agreementModels = []struct{ model, name string }{
	{"ecmwf_ifs025", "ecmwf"}, // ECMWF (Reading)
	{"gfs_seamless", "gfs"},   // NOAA (US)
	{"icon_seamless", "icon"}, // DWD (Germany)
}

// buildSource constructs the weather source from config. "agreement" requires every
// model in agreementModels to be clear; the single-provider modes run just one.
// Unknown values fall back to agreement.
func buildSource(cfg Config) Source {
	switch cfg.Source {
	case "open-meteo":
		return NewOpenMeteo(cfg.TZ)
	case "met-no":
		return NewMetNo(cfg.MetnoUserAgent)
	case "ecmwf", "gfs", "icon":
		for _, m := range agreementModels {
			if m.name == cfg.Source {
				return NewOpenMeteoModel(cfg.TZ, m.model, m.name)
			}
		}
		fallthrough
	default: // "agreement"
		sources := make([]Source, 0, len(agreementModels))
		for _, m := range agreementModels {
			sources = append(sources, NewOpenMeteoModel(cfg.TZ, m.model, m.name))
		}
		return NewMultiSource(sources...)
	}
}

// securityHeaders adds a few conservative response headers to every request.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// setupLogging installs a process-wide slog text logger at the configured level and
// routes the stdlib log package through it, so every line shares one structured stream.
func setupLogging(level string) {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
	slog.SetDefault(logger)
	log.SetFlags(0)
	log.SetOutput(slogWriter{})
}

type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	slog.Info(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// fatal logs an error and exits non-zero (slog has no Fatal of its own).
func fatal(msg string, err error) {
	slog.Error(msg, "err", err)
	os.Exit(1)
}
