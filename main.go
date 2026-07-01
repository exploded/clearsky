// Command clearsky is a personal astrophotography "go/no-go" app for Donvale, AU.
// A single long-running binary that each evening checks tonight's weather + moon,
// decides whether conditions suit astrophotography, logs the decision to SQLite,
// notifies on GO nights (Discord + email), and serves an HTMX log of past nights.
package main

import (
	"context"
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
	testNotify := flag.Bool("test-notify", false, "send a sample notification to all configured channels and exit")
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

	// -test-notify: verify webhook / SMTP setup without a database or scheduler.
	if *testNotify {
		notifier := NewNotifier(cfg)
		if !notifier.Enabled() {
			fatal("test-notify", errNoChannels)
		}
		notifier.Notify(context.Background(), demoMessage(loc))
		slog.Info("test notification dispatched")
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
	notifier := NewNotifier(cfg)
	runner := NewRunner(q, source, notifier, cfg, loc)
	scheduler := NewScheduler(runner, q, loc, cfg.RunHour, cfg.RunMinute)

	slog.Info("weather source", "mode", cfg.Source, "name", source.Name())
	slog.Info("notifications", "enabled", notifier.Enabled(), "channels", len(notifier.channels))

	// Catch up on today's decision if we missed the scheduled time, then run the
	// daily scheduler for the process lifetime.
	if cfg.CatchupOnStart {
		go scheduler.CatchupIfMissing(ctx)
	}
	go scheduler.Run(ctx)

	app, err := NewApp(q, runner, loc, cfg)
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

// buildSource constructs the weather source from config. "agreement" runs Open-Meteo
// and yr.no together and requires both to be clear; the single-provider modes run just
// one. Unknown values fall back to agreement.
func buildSource(cfg Config) Source {
	openMeteo := NewOpenMeteo(cfg.TZ)
	metNo := NewMetNo(cfg.MetnoUserAgent)
	switch cfg.Source {
	case "open-meteo":
		return openMeteo
	case "met-no":
		return metNo
	default: // "agreement"
		return NewMultiSource(openMeteo, metNo)
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
