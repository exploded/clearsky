package main

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"clearsky/store"
)

// fakeMailer records every email and can be told to fail.
type fakeMailer struct {
	mu   sync.Mutex
	sent []Email
	fail bool
}

func (f *fakeMailer) name() string { return "fake" }
func (f *fakeMailer) Send(_ context.Context, e Email) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("smtp down")
	}
	f.sent = append(f.sent, e)
	return nil
}

func (f *fakeMailer) last(t *testing.T) Email {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		t.Fatal("no email sent")
	}
	return f.sent[len(f.sent)-1]
}

func newTestSubs(t *testing.T) (*Subscriptions, *fakeMailer, *fakeChannel) {
	t.Helper()
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	mailer := &fakeMailer{}
	disc := &fakeChannel{}
	s := NewSubscriptions(store.New(db), mailer, Config{BaseURL: "https://clearsky.test", MaxSubscribers: 3})
	s.discord = func(string) channel { return disc }
	now := time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.limiter.now = s.now
	return s, mailer, disc
}

// tokenFrom pulls the t= parameter out of the first URL in an email body that contains path.
func tokenFrom(t *testing.T, body, path string) string {
	t.Helper()
	for _, f := range strings.Fields(body) {
		if strings.Contains(f, path) {
			u, err := url.Parse(f)
			if err != nil {
				t.Fatal(err)
			}
			return u.Query().Get("t")
		}
	}
	t.Fatalf("no %s link in body:\n%s", path, body)
	return ""
}

func TestSubscribeConfirmBroadcastUnsubscribe(t *testing.T) {
	s, mailer, disc := newTestSubs(t)
	ctx := context.Background()

	// Sign up with a webhook → pending row + confirmation email, nothing to Discord yet.
	if err := s.Subscribe(ctx, "Astro@Example.com", "https://discord.com/api/webhooks/1/abc", "1.2.3.4"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	conf := mailer.last(t)
	if conf.To != "astro@example.com" || !strings.Contains(conf.Subject, "Confirm") {
		t.Errorf("confirmation email: to=%q subject=%q", conf.To, conf.Subject)
	}
	if !strings.Contains(conf.Body, "MELBOURNE") || !strings.Contains(conf.Body, "Discord webhook") {
		t.Errorf("confirmation body missing Melbourne caveat / discord note:\n%s", conf.Body)
	}
	if disc.sent != 0 {
		t.Errorf("discord contacted before confirmation")
	}

	// Broadcast before confirming reaches nobody.
	s.Broadcast(ctx, sampleMessage(t, true))
	if len(mailer.sent) != 1 {
		t.Fatalf("pending subscriber was emailed an alert: %d sends", len(mailer.sent))
	}

	// Confirm → hello on Discord, owner hook fires once, and the row is live.
	var hookCalls int
	var hookTotal int64
	s.OnConfirmed = func(_ context.Context, sub store.Subscriber, total int64) {
		hookCalls++
		hookTotal = total
		if sub.Email != "astro@example.com" {
			t.Errorf("hook got %q", sub.Email)
		}
	}
	token := tokenFrom(t, conf.Body, "/subscribe/confirm")
	res, err := s.Confirm(ctx, token)
	if err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if hookCalls != 1 || hookTotal != 1 {
		t.Errorf("OnConfirmed calls=%d total=%d, want 1/1", hookCalls, hookTotal)
	}
	if sub := res.Sub; !sub.ConfirmedAt.Valid || sub.Email != "astro@example.com" || res.DiscordErr != nil {
		t.Errorf("confirmed row: %+v (discord err %v)", sub, res.DiscordErr)
	}
	if disc.sent != 1 || !strings.Contains(disc.lastSub, "clearsky will post") {
		t.Errorf("expected discord hello, got sent=%d sub=%q", disc.sent, disc.lastSub)
	}
	// Second click is harmless and does not re-ping the owner.
	if _, err := s.Confirm(ctx, token); err != nil {
		t.Errorf("re-confirm: %v", err)
	}
	if hookCalls != 1 {
		t.Errorf("OnConfirmed fired again on re-confirm: %d", hookCalls)
	}

	// Broadcast → email with unsubscribe footer + headers, and a Discord post.
	s.Broadcast(ctx, sampleMessage(t, true))
	alert := mailer.last(t)
	if alert.To != "astro@example.com" || !strings.Contains(alert.Subject, "GO for astrophotography") {
		t.Errorf("alert: to=%q subject=%q", alert.To, alert.Subject)
	}
	unsub := tokenFrom(t, alert.Body, "/subscribe/unsubscribe")
	if unsub != token {
		t.Errorf("unsubscribe token %q != subscriber token %q", unsub, token)
	}
	if !strings.Contains(alert.Headers["List-Unsubscribe"], "/subscribe/unsubscribe?t=") ||
		alert.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("missing List-Unsubscribe headers: %v", alert.Headers)
	}
	if !strings.Contains(alert.Body, "MELBOURNE") {
		t.Errorf("alert footer missing Melbourne caveat")
	}
	if disc.sent != 2 || !strings.Contains(disc.lastBody, "Melbourne only") {
		t.Errorf("expected discord alert, got sent=%d body=%q", disc.sent, disc.lastBody)
	}

	// Unsubscribe deletes the row; the link is dead afterwards, and broadcasts stop.
	if err := s.Unsubscribe(ctx, unsub); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
	if err := s.Unsubscribe(ctx, unsub); !errors.Is(err, ErrBadToken) {
		t.Errorf("second unsubscribe: got %v, want ErrBadToken", err)
	}
	if _, err := s.Lookup(ctx, unsub); !errors.Is(err, ErrBadToken) {
		t.Errorf("lookup after delete: %v", err)
	}
	before := len(mailer.sent)
	s.Broadcast(ctx, sampleMessage(t, true))
	if len(mailer.sent) != before {
		t.Errorf("unsubscribed address still emailed")
	}
}

func TestSubscribeValidationAndAbuseGuards(t *testing.T) {
	s, mailer, _ := newTestSubs(t)
	ctx := context.Background()

	var fe *FormError
	if err := s.Subscribe(ctx, "not-an-email", "", "9.9.9.9"); !errors.As(err, &fe) || fe.Field != "email" {
		t.Errorf("bad email: %v", err)
	}
	if err := s.Subscribe(ctx, "a@example.com", "https://evil.example/api/webhooks/1", "9.9.9.9"); !errors.As(err, &fe) || fe.Field != "discord_webhook" {
		t.Errorf("non-discord webhook accepted: %v", err)
	}
	if err := s.Subscribe(ctx, "a@example.com", "http://discord.com/api/webhooks/1/x", "9.9.9.9"); !errors.As(err, &fe) || fe.Field != "discord_webhook" {
		t.Errorf("plain-http webhook accepted: %v", err)
	}
	if len(mailer.sent) != 0 {
		t.Fatalf("validation failures must not send: %d", len(mailer.sent))
	}

	// Cooldown: the same address twice in a row gets one confirmation email.
	if err := s.Subscribe(ctx, "b@example.com", "", "9.9.9.9"); err != nil {
		t.Fatal(err)
	}
	if err := s.Subscribe(ctx, "b@example.com", "", "9.9.9.9"); err != nil {
		t.Fatal(err)
	}
	if len(mailer.sent) != 1 {
		t.Errorf("cooldown: got %d confirmation emails, want 1", len(mailer.sent))
	}

	// After the cooldown a re-submission rotates the token; the old link dies.
	oldToken := tokenFrom(t, mailer.last(t).Body, "/subscribe/confirm")
	base := s.now()
	s.now = func() time.Time { return base.Add(resendCooldown + time.Second) }
	if err := s.Subscribe(ctx, "b@example.com", "https://discord.com/api/webhooks/2/y", "9.9.9.9"); err != nil {
		t.Fatal(err)
	}
	newToken := tokenFrom(t, mailer.last(t).Body, "/subscribe/confirm")
	if newToken == oldToken {
		t.Errorf("token not rotated on re-submission")
	}
	if _, err := s.Confirm(ctx, oldToken); !errors.Is(err, ErrBadToken) {
		t.Errorf("old token still confirms: %v", err)
	}
	res, err := s.Confirm(ctx, newToken)
	if err != nil || res.Sub.DiscordWebhook != "https://discord.com/api/webhooks/2/y" {
		t.Errorf("confirm with new token: %+v %v", res, err)
	}

	// A confirmed address re-submitted by a stranger: webhook is NOT changed, the inbox
	// owner gets a notice with an unsubscribe link, and the form still says "check inbox".
	s.now = func() time.Time { return base.Add(2*resendCooldown + time.Second) }
	if err := s.Subscribe(ctx, "b@example.com", "https://discord.com/api/webhooks/666/hijack", "6.6.6.6"); err != nil {
		t.Fatalf("resubmit confirmed: %v", err)
	}
	notice := mailer.last(t)
	if !strings.Contains(notice.Subject, "already subscribed") || !strings.Contains(notice.Body, "/subscribe/unsubscribe?t=") {
		t.Errorf("already-subscribed notice: %q\n%s", notice.Subject, notice.Body)
	}
	got, _ := s.Lookup(ctx, newToken)
	if got.DiscordWebhook != "https://discord.com/api/webhooks/2/y" {
		t.Errorf("stranger changed webhook to %q", got.DiscordWebhook)
	}

	// Per-IP rate limit (5/hour): the sixth distinct address from one IP is refused.
	s.now = func() time.Time { return base.Add(3 * resendCooldown) }
	s.max = 100
	for i := 0; i < 5; i++ {
		_ = s.Subscribe(ctx, "x"+string(rune('a'+i))+"@example.com", "", "7.7.7.7")
	}
	if err := s.Subscribe(ctx, "xz@example.com", "", "7.7.7.7"); !errors.As(err, &fe) || !strings.Contains(fe.Msg, "Too many") {
		t.Errorf("rate limit: %v", err)
	}
	if err := s.Subscribe(ctx, "xz@example.com", "", "8.8.8.8"); err != nil {
		t.Errorf("other IP blocked: %v", err)
	}
}

func TestSubscribeCapAndSendFailure(t *testing.T) {
	s, mailer, _ := newTestSubs(t) // max 3
	ctx := context.Background()
	for _, e := range []string{"1@example.com", "2@example.com", "3@example.com"} {
		if err := s.Subscribe(ctx, e, "", "1.1.1."+e[:1]); err != nil {
			t.Fatal(err)
		}
	}
	var fe *FormError
	if err := s.Subscribe(ctx, "4@example.com", "", "1.1.1.4"); !errors.As(err, &fe) || !strings.Contains(fe.Msg, "full") {
		t.Errorf("cap: %v", err)
	}

	// Transport failure surfaces as a form error and does NOT start the cooldown, so the
	// user can retry straight away.
	s.max = 10
	mailer.fail = true
	if err := s.Subscribe(ctx, "5@example.com", "", "1.1.1.5"); !errors.As(err, &fe) || !strings.Contains(fe.Msg, "Couldn't send") {
		t.Errorf("send failure: %v", err)
	}
	mailer.fail = false
	if err := s.Subscribe(ctx, "5@example.com", "", "1.1.1.5"); err != nil {
		t.Errorf("retry after failure: %v", err)
	}
	if mailer.last(t).To != "5@example.com" {
		t.Errorf("retry did not resend")
	}
}

func TestConfirmLinkExpires(t *testing.T) {
	s, mailer, _ := newTestSubs(t)
	ctx := context.Background()
	if err := s.Subscribe(ctx, "late@example.com", "", "2.2.2.2"); err != nil {
		t.Fatal(err)
	}
	token := tokenFrom(t, mailer.last(t).Body, "/subscribe/confirm")
	base := s.now()
	s.now = func() time.Time { return base.Add(confirmTTL + time.Minute) }
	if _, err := s.Confirm(ctx, token); !errors.Is(err, ErrBadToken) {
		t.Errorf("expired link accepted: %v", err)
	}
}

func TestNormalizeWebhook(t *testing.T) {
	ok := []string{
		"", "  ", "https://discord.com/api/webhooks/123/abc-DEF_ghi",
		"https://ptb.discord.com/api/webhooks/1/x", "https://DISCORDAPP.com/api/webhooks/1/x",
	}
	for _, in := range ok {
		if _, err := normalizeWebhook(in); err != nil {
			t.Errorf("normalizeWebhook(%q): %v", in, err)
		}
	}
	bad := []string{
		"https://discord.com/", "https://discord.com.evil.io/api/webhooks/1/x",
		"https://user@discord.com/api/webhooks/1/x", "ftp://discord.com/api/webhooks/1/x",
		"https://example.com/api/webhooks/1/x", "javascript:alert(1)",
	}
	for _, in := range bad {
		if _, err := normalizeWebhook(in); err == nil {
			t.Errorf("normalizeWebhook(%q) accepted", in)
		}
	}
}

func TestIPLimiter(t *testing.T) {
	l := newIPLimiter(2, time.Hour)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	if !l.allow("a") || !l.allow("a") || l.allow("a") {
		t.Error("expected 2 allowed then refused")
	}
	if !l.allow("b") {
		t.Error("other key should be independent")
	}
	now = now.Add(61 * time.Minute)
	if !l.allow("a") {
		t.Error("window should have rolled over")
	}
}
