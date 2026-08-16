package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"clearsky/store"
)

// Subscriptions is the public opt-in list: anyone imaging around Melbourne can leave an
// email address (and optionally a Discord webhook) and get the same GO-night nudge the
// owner gets. Everything goes out through AWS SES.
//
// The list is double opt-in and self-service by construction:
//   - sign-up creates a pending row and emails a confirmation link; nothing else is ever
//     sent until that link is clicked;
//   - every alert carries an unsubscribe link (and RFC 8058 List-Unsubscribe headers so
//     mail clients show their own button); unsubscribing deletes the row.
//
// The form is public, so it is also defended a little: a per-IP rate limit, a cooldown
// on system emails to any one address, a cap on table size, and a purge of stale pending
// rows. The form's response never reveals whether an address is already on the list.
type Subscriptions struct {
	q       *store.Queries
	mailer  Mailer
	baseURL string
	max     int
	limiter *ipLimiter
	now     func() time.Time
	// discord builds the poster for a subscriber's webhook; swapped in tests.
	discord func(webhookURL string) channel

	// OnConfirmed, if set, is called once when a subscription is first confirmed — the
	// owner's "someone joined" ping. Wired by main after the Notifier exists (the two
	// reference each other, so neither can construct the other).
	OnConfirmed func(ctx context.Context, sub store.Subscriber, confirmedTotal int64)
}

const (
	confirmTTL     = 48 * time.Hour   // confirmation links (and pending rows) live this long
	resendCooldown = 10 * time.Minute // min gap between system emails to one address
)

// ErrBadToken is returned for confirm/unsubscribe links that match nothing — expired,
// already used, or made up.
var ErrBadToken = errors.New("that link is not valid — it may have expired or already been used")

// FormError is a validation problem to show the user, attributed to a form field
// ("email", "discord_webhook") or to the whole form (Field == "").
type FormError struct {
	Field string
	Msg   string
}

func (e *FormError) Error() string { return e.Msg }

func NewSubscriptions(q *store.Queries, mailer Mailer, cfg Config) *Subscriptions {
	return &Subscriptions{
		q:       q,
		mailer:  mailer,
		baseURL: cfg.BaseURL,
		max:     cfg.MaxSubscribers,
		limiter: newIPLimiter(5, time.Hour),
		now:     time.Now,
		discord: func(u string) channel { return &discordChannel{webhookURL: u} },
	}
}

// Subscribe handles one form submission. A nil return means "check your inbox" — which
// is deliberately also the answer for an address that is already subscribed (they get
// an email saying so; the page does not).
func (s *Subscriptions) Subscribe(ctx context.Context, rawEmail, rawWebhook, ip string) error {
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		return &FormError{Field: "email", Msg: err.Error()}
	}
	webhook, err := normalizeWebhook(rawWebhook)
	if err != nil {
		return &FormError{Field: "discord_webhook", Msg: err.Error()}
	}
	if !s.limiter.allow(ip) {
		return &FormError{Msg: "Too many sign-ups from your connection — please try again in an hour."}
	}
	now := s.now()
	if err := s.q.PurgeStalePending(ctx, now.Add(-confirmTTL).Unix()); err != nil {
		slog.Warn("purge stale subscribers", "err", err)
	}

	existing, err := s.q.GetSubscriberByEmail(ctx, email)
	switch {
	case err == nil:
		if now.Unix()-existing.LastSentAt < int64(resendCooldown.Seconds()) {
			slog.Info("subscribe: within cooldown, not resending", "email", email)
			return nil
		}
		if existing.ConfirmedAt.Valid {
			// Already on the list. Nothing changes (a stranger must not be able to swap
			// someone's webhook by typing their address); tell the owner of the inbox.
			if err := s.sendAlreadySubscribed(ctx, existing); err != nil {
				return s.sendFailed(err)
			}
			return s.touch(ctx, existing.ID, now)
		}
		token := newToken()
		if err := s.q.RefreshPendingSubscriber(ctx, store.RefreshPendingSubscriberParams{
			DiscordWebhook: webhook, Token: token, LastSentAt: 0, UpdatedAt: now.Unix(), Email: email,
		}); err != nil {
			return fmt.Errorf("refresh pending subscriber: %w", err)
		}
		if err := s.sendConfirmation(ctx, email, token, webhook != ""); err != nil {
			return s.sendFailed(err)
		}
		return s.touch(ctx, existing.ID, now)

	case errors.Is(err, sql.ErrNoRows):
		n, err := s.q.CountSubscribers(ctx)
		if err != nil {
			return fmt.Errorf("count subscribers: %w", err)
		}
		if int(n) >= s.max {
			return &FormError{Msg: "The list is full for now — sorry. Try again another day."}
		}
		token := newToken()
		if err := s.q.CreateSubscriber(ctx, store.CreateSubscriberParams{
			Email: email, DiscordWebhook: webhook, Token: token,
			LastSentAt: 0, CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
		}); err != nil {
			return fmt.Errorf("create subscriber: %w", err)
		}
		if err := s.sendConfirmation(ctx, email, token, webhook != ""); err != nil {
			return s.sendFailed(err)
		}
		created, err := s.q.GetSubscriberByEmail(ctx, email)
		if err != nil {
			return fmt.Errorf("reload subscriber: %w", err)
		}
		return s.touch(ctx, created.ID, now)

	default:
		return fmt.Errorf("lookup subscriber: %w", err)
	}
}

// sendFailed turns a transport error into something the form can show. The row is
// left with last_sent_at untouched, so the user can simply try again.
func (s *Subscriptions) sendFailed(err error) error {
	slog.Error("subscriber email failed", "err", err)
	return &FormError{Msg: "Couldn't send the confirmation email just now — please try again in a few minutes."}
}

func (s *Subscriptions) touch(ctx context.Context, id int64, now time.Time) error {
	if err := s.q.TouchSubscriberSent(ctx, store.TouchSubscriberSentParams{
		LastSentAt: now.Unix(), UpdatedAt: now.Unix(), ID: id,
	}); err != nil {
		return fmt.Errorf("stamp last_sent_at: %w", err)
	}
	return nil
}

// ConfirmResult is what Confirm hands back: the live row, plus the outcome of the
// Discord hello (nil when no webhook was given, or it worked).
type ConfirmResult struct {
	Sub        store.Subscriber
	DiscordErr error
}

// Confirm activates the subscription behind a confirmation token. Clicking a link twice
// is fine — an already-confirmed row is returned as-is. If a Discord webhook was given,
// a hello is posted to it so the subscriber can see the plumbing works — and if that
// fails they are told, because a mistyped webhook is otherwise silent forever.
func (s *Subscriptions) Confirm(ctx context.Context, token string) (ConfirmResult, error) {
	sub, err := s.q.GetSubscriberByToken(ctx, token)
	if errors.Is(err, sql.ErrNoRows) {
		return ConfirmResult{}, ErrBadToken
	}
	if err != nil {
		return ConfirmResult{}, fmt.Errorf("lookup token: %w", err)
	}
	if sub.ConfirmedAt.Valid {
		return ConfirmResult{Sub: sub}, nil
	}
	now := s.now()
	if now.Unix()-sub.UpdatedAt > int64(confirmTTL.Seconds()) {
		return ConfirmResult{}, ErrBadToken
	}
	res, err := s.q.ConfirmSubscriber(ctx, store.ConfirmSubscriberParams{
		ConfirmedAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
		UpdatedAt:   now.Unix(),
		Token:       token,
	})
	if err != nil {
		return ConfirmResult{}, fmt.Errorf("confirm subscriber: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ConfirmResult{}, ErrBadToken
	}
	sub.ConfirmedAt = sql.NullInt64{Int64: now.Unix(), Valid: true}
	slog.Info("subscriber confirmed", "email", sub.Email, "discord", sub.DiscordWebhook != "")

	out := ConfirmResult{Sub: sub}
	if s.OnConfirmed != nil {
		total, err := s.q.CountConfirmedSubscribers(ctx)
		if err != nil {
			slog.Warn("count confirmed subscribers", "err", err)
		}
		s.OnConfirmed(ctx, sub, total)
	}
	if sub.DiscordWebhook != "" {
		body := fmt.Sprintf("Forecast is for Donvale, VIC — Melbourne skies only. NO-GO nights stay quiet.\nLog: %s\n", s.baseURL)
		if err := s.discord(sub.DiscordWebhook).send(ctx, "✅ clearsky will post Melbourne GO alerts here", body); err != nil {
			slog.Warn("subscriber discord hello failed", "email", sub.Email, "err", err)
			out.DiscordErr = err
		}
	}
	return out, nil
}

// Lookup returns the subscriber behind a token, for the unsubscribe confirmation page.
func (s *Subscriptions) Lookup(ctx context.Context, token string) (store.Subscriber, error) {
	sub, err := s.q.GetSubscriberByToken(ctx, token)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Subscriber{}, ErrBadToken
	}
	return sub, err
}

// Unsubscribe deletes the row behind a token. Works for pending rows too.
func (s *Subscriptions) Unsubscribe(ctx context.Context, token string) error {
	res, err := s.q.DeleteSubscriberByToken(ctx, token)
	if err != nil {
		return fmt.Errorf("delete subscriber: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrBadToken
	}
	slog.Info("subscriber unsubscribed")
	return nil
}

// Broadcast sends a GO alert to every confirmed subscriber: email always, Discord when
// they gave a webhook. Failures are logged per subscriber and never stop the others.
// A few sends run concurrently — enough to keep a modest list quick, few enough to stay
// under SES's default send rate.
func (s *Subscriptions) Broadcast(ctx context.Context, m Message) {
	subs, err := s.q.ListConfirmedSubscribers(ctx)
	if err != nil {
		slog.Error("list subscribers", "err", err)
		return
	}
	if len(subs) == 0 {
		return
	}
	// Own budget: the caller's ctx may carry a short request deadline or be cancelled by
	// shutdown, and a GO alert is worth finishing.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()

	subject, body := m.Subject(), m.Body()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	var sent, failed int
	var mu sync.Mutex
	for _, sub := range subs {
		wg.Add(1)
		sem <- struct{}{}
		go func(sub store.Subscriber) {
			defer wg.Done()
			defer func() { <-sem }()
			ok := true
			if err := s.mailer.Send(ctx, s.alertEmail(sub, subject, body)); err != nil {
				slog.Error("subscriber email failed", "email", sub.Email, "err", err)
				ok = false
			}
			if sub.DiscordWebhook != "" {
				footer := fmt.Sprintf("\nclearsky · Donvale, VIC (Melbourne only) · %s\n", s.baseURL)
				if err := s.discord(sub.DiscordWebhook).send(ctx, subject, body+footer); err != nil {
					slog.Error("subscriber discord failed", "email", sub.Email, "err", err)
					ok = false
				}
			}
			mu.Lock()
			if ok {
				sent++
			} else {
				failed++
			}
			mu.Unlock()
		}(sub)
	}
	wg.Wait()
	slog.Info("subscriber alerts sent", "date", m.Date.Format("2006-01-02"),
		"subscribers", len(subs), "ok", sent, "failed", failed)
}

// --- message builders ---------------------------------------------------------------

func (s *Subscriptions) confirmURL(token string) string {
	return s.baseURL + "/subscribe/confirm?t=" + url.QueryEscape(token)
}

func (s *Subscriptions) unsubscribeURL(token string) string {
	return s.baseURL + "/subscribe/unsubscribe?t=" + url.QueryEscape(token)
}

const melbourneOnly = "The forecast is for Donvale, on Melbourne's eastern edge — MELBOURNE SKIES ONLY. " +
	"If you image from anywhere else, the call won't apply to your sky."

func (s *Subscriptions) sendConfirmation(ctx context.Context, email, token string, withDiscord bool) error {
	var b strings.Builder
	b.WriteString("Hi,\n\n")
	b.WriteString("Someone — hopefully you — asked for clearsky GO alerts at this address.\n\n")
	b.WriteString("clearsky checks each evening whether the night looks good for astrophotography and\n")
	b.WriteString("sends an alert on the nights that pass. NO-GO nights are silent.\n\n")
	b.WriteString(melbourneOnly + "\n\n")
	b.WriteString("Confirm your subscription:\n  " + s.confirmURL(token) + "\n\n")
	if withDiscord {
		b.WriteString("Once confirmed, alerts will also be posted to the Discord webhook you gave.\n\n")
	}
	b.WriteString("The link is good for 48 hours. If you didn't ask for this, just ignore it — nothing\n")
	b.WriteString("will be sent to this address unless you confirm.\n\n")
	b.WriteString("— clearsky · " + s.baseURL + "\n")
	return s.mailer.Send(ctx, Email{
		To:      email,
		Subject: "Confirm your clearsky GO alerts (Melbourne)",
		Body:    b.String(),
	})
}

func (s *Subscriptions) sendAlreadySubscribed(ctx context.Context, sub store.Subscriber) error {
	var b strings.Builder
	b.WriteString("Hi,\n\n")
	b.WriteString("Someone just entered this address on the clearsky sign-up form, but it is already\n")
	b.WriteString("subscribed to GO alerts, so nothing has changed.\n\n")
	b.WriteString("To change your Discord webhook, unsubscribe and sign up again:\n  " + s.unsubscribeURL(sub.Token) + "\n\n")
	b.WriteString("If that wasn't you, you can ignore this.\n\n")
	b.WriteString("— clearsky · " + s.baseURL + "\n")
	return s.mailer.Send(ctx, s.withUnsubscribe(sub, Email{
		To:      sub.Email,
		Subject: "You're already subscribed to clearsky GO alerts",
		Body:    b.String(),
	}))
}

func (s *Subscriptions) alertEmail(sub store.Subscriber, subject, body string) Email {
	var b strings.Builder
	b.WriteString(body)
	b.WriteString("\n—\n")
	b.WriteString("You're getting this because you subscribed to clearsky GO alerts.\n")
	b.WriteString(melbourneOnly + "\n")
	b.WriteString("Log: " + s.baseURL + "\n")
	b.WriteString("Unsubscribe: " + s.unsubscribeURL(sub.Token) + "\n")
	return s.withUnsubscribe(sub, Email{To: sub.Email, Subject: subject, Body: b.String()})
}

// withUnsubscribe adds the RFC 2369 / RFC 8058 headers so mail clients can offer their
// own one-click unsubscribe (which POSTs to the same URL the footer link opens).
func (s *Subscriptions) withUnsubscribe(sub store.Subscriber, e Email) Email {
	if e.Headers == nil {
		e.Headers = map[string]string{}
	}
	e.Headers["List-Unsubscribe"] = "<" + s.unsubscribeURL(sub.Token) + ">"
	e.Headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	return e
}

// --- helpers ------------------------------------------------------------------------

// discordWebhookHosts are the only hosts a subscriber-supplied webhook may point at. The
// app POSTs to this URL on their behalf, so anything else would be an open relay.
var discordWebhookHosts = map[string]bool{
	"discord.com": true, "discordapp.com": true,
	"ptb.discord.com": true, "canary.discord.com": true,
}

// normalizeWebhook accepts an empty string (no Discord) or a Discord incoming-webhook URL.
func normalizeWebhook(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if len(s) > 512 {
		return "", fmt.Errorf("that webhook URL is too long")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || !discordWebhookHosts[strings.ToLower(u.Hostname())] ||
		!strings.HasPrefix(u.Path, "/api/webhooks/") || u.User != nil {
		return "", fmt.Errorf("enter a Discord webhook URL (https://discord.com/api/webhooks/…), or leave it blank")
	}
	return u.String(), nil
}

// newToken returns 256 bits of randomness, URL-safe, for confirm/unsubscribe links.
func newToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ipLimiter is a fixed-window per-key rate limit kept in memory — enough to make the
// public form useless for hosing an inbox, no more.
type ipLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
	now    func() time.Time
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{hits: map[string][]time.Time{}, limit: limit, window: window, now: time.Now}
}

func (l *ipLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	cut := now.Add(-l.window)
	if len(l.hits) > 4096 { // keep the map from growing without bound under a flood
		for k, ts := range l.hits {
			if len(ts) == 0 || ts[len(ts)-1].Before(cut) {
				delete(l.hits, k)
			}
		}
	}
	ts := l.hits[key]
	kept := ts[:0]
	for _, t := range ts {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}
