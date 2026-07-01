package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// errNoChannels is returned by the -test-notify path when nothing is configured.
var errNoChannels = errors.New("no notification channels configured (set CLEARSKY_DISCORD_WEBHOOK_URL and/or CLEARSKY_SMTP_* )")

// Message is everything needed to render a notification for one night.
type Message struct {
	Date   time.Time // the evening's date (local)
	Source string
	Result Result
	Dark   Darkness
	Moon   MoonInfo
}

// Subject is the email subject line.
func (m Message) Subject() string {
	verdict := "NO-GO"
	if m.Result.GO {
		verdict = "GO"
	}
	return fmt.Sprintf("%s for astrophotography tonight — Donvale (%s)", verdict, m.Date.Format("2 Jan"))
}

// Body is the plaintext / lightly-marked-up message shared by all channels.
func (m Message) Body() string {
	var b strings.Builder
	head := "🔭 GO tonight"
	if !m.Result.GO {
		head = "☁️ NO-GO tonight"
	}
	fmt.Fprintf(&b, "%s — Donvale (%s)   Score %d/100.  [%s]\n",
		head, m.Date.Format("Mon 2 Jan"), m.Result.Score, m.Source)
	fmt.Fprintf(&b, "%s\n\n", m.Result.Reason)

	dur := m.Dark.Dawn.Sub(m.Dark.Dusk)
	fmt.Fprintf(&b, "Darkness:  dusk %s → dawn %s (%s %s dark)\n",
		m.Dark.Dusk.Format("15:04"), m.Dark.Dawn.Format("Mon 15:04"),
		humanDur(dur), m.Dark.Kind)
	fmt.Fprintf(&b, "Cloud:     avg %d%%, peak %d%% at %s (low %d%%, mid %d%%, high %d%%)\n",
		m.Result.Cloud.Avg, m.Result.Cloud.Max, m.Result.Cloud.PeakAt,
		m.Result.Cloud.MaxLow, m.Result.Cloud.MaxMid, m.Result.Cloud.MaxHigh)
	fmt.Fprintf(&b, "Rain:      %.1f mm, max prob %d%%\n",
		m.Result.Rain.TotalMm, m.Result.Rain.MaxProbPct)
	fmt.Fprintf(&b, "Moon:      %.0f%% (%s)%s      (info only)\n",
		m.Moon.IllumPct, m.Moon.PhaseName, moonTimes(m.Moon))
	return b.String()
}

func moonTimes(m MoonInfo) string {
	var parts []string
	if m.Rise != nil {
		parts = append(parts, "rises "+m.Rise.Format("15:04"))
	}
	if m.Set != nil {
		parts = append(parts, "sets "+m.Set.Format("15:04"))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", " + strings.Join(parts, ", ")
}

func humanDur(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh%02dm", h, m)
}

// demoMessage builds a representative GO message for verifying notification channels
// (used by the -test-notify flag). It does not touch the database.
func demoMessage(loc *time.Location) Message {
	now := time.Now().In(loc)
	dusk := time.Date(now.Year(), now.Month(), now.Day(), 18, 45, 0, 0, loc)
	dawn := dusk.Add(11*time.Hour + 17*time.Minute)
	moonSet := time.Date(now.Year(), now.Month(), now.Day(), 21, 3, 0, 0, loc)
	return Message{
		Date:   now,
		Source: "test",
		Result: Result{
			GO: true, Score: 82, Reason: "TEST MESSAGE — clear: avg 8% cloud, peak 22%, no rain",
			Cloud: CloudSummary{Avg: 8, Max: 22, PeakAt: "00:00", MaxHigh: 22},
			Rain:  RainSummary{TotalMm: 0, MaxProbPct: 5},
		},
		Dark: Darkness{Dusk: dusk, Dawn: dawn, Kind: "astronomical"},
		Moon: MoonInfo{IllumPct: 12, PhaseName: "Waxing Crescent", Set: &moonSet},
	}
}

// channel is one notification transport.
type channel interface {
	name() string
	send(ctx context.Context, subject, body string) error
}

// Notifier fans a message out to every configured channel. It logs per-channel
// failures and never returns an error — a channel outage must not fail the job.
type Notifier struct {
	channels []channel
}

// NewNotifier builds the channels enabled by config. Empty webhook / SMTP creds mean
// that channel is simply omitted.
func NewNotifier(cfg Config) *Notifier {
	n := &Notifier{}
	if cfg.DiscordWebhookURL != "" {
		n.channels = append(n.channels, &discordChannel{webhookURL: cfg.DiscordWebhookURL})
	}
	if cfg.SMTPUser != "" && cfg.SMTPPass != "" && cfg.EmailTo != "" {
		n.channels = append(n.channels, &emailChannel{
			host: cfg.SMTPHost, port: cfg.SMTPPort,
			user: cfg.SMTPUser, pass: cfg.SMTPPass, to: cfg.EmailTo,
		})
	}
	return n
}

// Enabled reports whether any channel is configured.
func (n *Notifier) Enabled() bool { return len(n.channels) > 0 }

// Notify sends the message to all channels. Returns nil always; failures are logged.
func (n *Notifier) Notify(ctx context.Context, m Message) {
	if len(n.channels) == 0 {
		slog.Debug("no notification channels configured; skipping")
		return
	}
	subject, body := m.Subject(), m.Body()
	for _, c := range n.channels {
		if err := c.send(ctx, subject, body); err != nil {
			slog.Error("notification failed", "channel", c.name(), "err", err)
			continue
		}
		slog.Info("notification sent", "channel", c.name(), "date", m.Date.Format("2006-01-02"))
	}
}
