package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"clearsky/store"
)

// errNoChannels is returned by the -test-notify path when nothing is configured.
var errNoChannels = errors.New("no owner notification channels configured (set CLEARSKY_DISCORD_WEBHOOK_URL and/or CLEARSKY_EMAIL_TO + CLEARSKY_SES_*)")

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

	w := m.Result.Window
	if w.Hours > 0 {
		fmt.Fprintf(&b, "Window:    %s — %dh usable, avg %d%% cloud (peak %d%%)\n",
			w.Label, w.Hours, w.AvgCloud, w.MaxCloud)
	} else {
		fmt.Fprintf(&b, "Window:    none — no hour met the cloud/rain gate\n")
	}
	dur := m.Dark.Dawn.Sub(m.Dark.Dusk)
	fmt.Fprintf(&b, "Darkness:  dusk %s → dawn %s (%s %s dark)\n",
		m.Dark.Dusk.Format("15:04"), m.Dark.Dawn.Format("Mon 15:04"),
		humanDur(dur), m.Dark.Kind)
	fmt.Fprintf(&b, "Cloud:     whole night avg %d%%, peak %d%% at %s (low %d%%, mid %d%%, high %d%%)\n",
		m.Result.Cloud.Avg, m.Result.Cloud.Max, m.Result.Cloud.PeakAt,
		m.Result.Cloud.MaxLow, m.Result.Cloud.MaxMid, m.Result.Cloud.MaxHigh)
	fmt.Fprintf(&b, "Rain:      %.1f mm, max prob %d%%%s\n",
		m.Result.Rain.TotalMm, m.Result.Rain.MaxProbPct, packUpNote(m.Result.Rain))
	fmt.Fprintf(&b, "Moon:      %.0f%% (%s)%s\n",
		m.Moon.IllumPct, m.Moon.PhaseName, moonTimes(m.Moon))
	fmt.Fprintf(&b, "           %s   (info only — never affects the decision)\n", m.Moon.Note())
	return b.String()
}

// FailureMessage reports that a night's check could not be completed at all — every
// attempt failed and the retry deadline passed, so no decision was recorded.
//
// This exists because the app's original failure mode was silence. A GO night notified,
// a NO-GO night sat quietly in the log, and a night whose forecast fetch died looked
// exactly like the latter. Four consecutive nights were lost that way in Aug 2026. A
// broken check is now as loud as a good night.
type FailureMessage struct {
	Date     time.Time
	Attempts int
	Err      error
	Source   string
	BaseURL  string
}

func (f FailureMessage) Subject() string {
	return fmt.Sprintf("clearsky FAILED to check tonight — Donvale (%s)", f.Date.Format("2 Jan"))
}

func (f FailureMessage) Body() string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ No decision for %s — the check failed and has stopped retrying.\n\n",
		f.Date.Format("Mon 2 Jan"))
	fmt.Fprintf(&b, "Attempts:  %d (backing off, until the retry deadline)\n", f.Attempts)
	fmt.Fprintf(&b, "Source:    %s\n", f.Source)
	fmt.Fprintf(&b, "Last error: %v\n\n", f.Err)
	b.WriteString("There is NO row for tonight — this is not a NO-GO, it is no answer at all.\n")
	if f.BaseURL != "" {
		fmt.Fprintf(&b, "Check the sky yourself, and retry from %s if the provider has recovered.\n", f.BaseURL)
	}
	return b.String()
}

func packUpNote(r RainSummary) string {
	if r.PackUpAt == "" {
		return ""
	}
	return " — arrives " + r.PackUpAt + ", pack up by then"
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
			GO: true, Score: 82, Reason: "TEST MESSAGE — clear 19:45→01:45, 6h usable, avg 8% cloud",
			Cloud:  CloudSummary{Avg: 8, Max: 22, PeakAt: "00:00", MaxHigh: 22},
			Rain:   RainSummary{TotalMm: 0, MaxProbPct: 5},
			Window: WindowSummary{Label: "19:45→01:45", Hours: 6, AvgCloud: 8, MaxCloud: 22},
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

// Notifier fans a message out to the owner's channels and, for GO nights, to every
// confirmed public subscriber. It logs per-channel failures and never returns an error
// — a channel outage must not fail the job.
type Notifier struct {
	channels []channel
	subs     *Subscriptions // nil when SES is not configured
	baseURL  string         // included in failure messages so a manual re-run is one click away
}

// NewNotifier builds the owner channels enabled by config plus, when SES is configured,
// the subscriber fanout. An empty webhook means no Discord; EmailTo without SES is
// logged and dropped rather than silently doing nothing.
func NewNotifier(cfg Config, subs *Subscriptions) *Notifier {
	n := &Notifier{baseURL: cfg.BaseURL, subs: subs}
	if cfg.DiscordWebhookURL != "" {
		n.channels = append(n.channels, &discordChannel{webhookURL: cfg.DiscordWebhookURL})
	}
	if cfg.EmailTo != "" {
		if cfg.SubscribersEnabled() {
			n.channels = append(n.channels, &emailChannel{to: cfg.EmailTo, mailer: cfg.mailer()})
		} else {
			slog.Warn("CLEARSKY_EMAIL_TO is set but SES is not configured; owner email disabled")
		}
	}
	return n
}

// Enabled reports whether anything can be notified: an owner channel, or a
// subscriber list (which may currently be empty — that is still worth a send attempt,
// and it keeps notified_at honest).
func (n *Notifier) Enabled() bool { return len(n.channels) > 0 || n.subs != nil }

// Notify sends a GO alert to the owner's channels and every confirmed subscriber.
func (n *Notifier) Notify(ctx context.Context, m Message) {
	n.fanout(ctx, m.Subject(), m.Body(), m.Date)
	if n.subs != nil {
		n.subs.Broadcast(ctx, m)
	}
}

// NotifyFailure reports a night that could not be evaluated at all. Owner channels
// only — it is an ops alert ("go retry it"), not something subscribers can act on.
func (n *Notifier) NotifyFailure(ctx context.Context, f FailureMessage) {
	f.BaseURL = n.baseURL
	n.fanout(ctx, f.Subject(), f.Body(), f.Date)
}

// NotifySubscriberConfirmed tells the owner that someone has joined the list. Owner
// channels only. It runs in the background so the subscriber's confirmation page is
// not held up by the owner's Discord round-trip.
func (n *Notifier) NotifySubscriberConfirmed(ctx context.Context, sub store.Subscriber, confirmedTotal int64) {
	subject := fmt.Sprintf("New clearsky subscriber: %s", sub.Email)
	var b strings.Builder
	fmt.Fprintf(&b, "📬 %s just confirmed their subscription.\n", sub.Email)
	if sub.DiscordWebhook != "" {
		b.WriteString("Discord:   yes (webhook given)\n")
	} else {
		b.WriteString("Discord:   no\n")
	}
	fmt.Fprintf(&b, "Confirmed subscribers now: %d\n", confirmedTotal)
	go n.fanout(ctx, subject, b.String(), time.Now())
}

func (n *Notifier) fanout(ctx context.Context, subject, body string, date time.Time) {
	if len(n.channels) == 0 {
		slog.Debug("no owner notification channels configured; skipping")
		return
	}
	// The caller's context may already be cancelled (shutdown) or carry the failed run's
	// deadline; give the send its own budget so the alert still gets out.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for _, c := range n.channels {
		if err := c.send(ctx, subject, body); err != nil {
			slog.Error("notification failed", "channel", c.name(), "err", err)
			continue
		}
		slog.Info("notification sent", "channel", c.name(), "date", date.Format("2006-01-02"))
	}
}
