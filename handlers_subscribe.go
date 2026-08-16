package main

import (
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
)

// SubscribeForm is the template state for the sign-up form: what the user typed, any
// error to show next to a field, and whether the submission went through.
type SubscribeForm struct {
	Email      string
	Webhook    string
	EmailErr   string
	WebhookErr string
	FormErr    string
	Done       bool // true → render the "check your inbox" state instead of the form
}

// messagePage is the data for the one-paragraph result pages (confirm / unsubscribe).
type messagePage struct {
	Title   string
	Heading string
	Lines   []string
	Tone    string // "ok" | "bad" | "" — colours the heading
	// Optional action form rendered under the text (the unsubscribe confirmation).
	Action      string
	ActionLabel string
}

// handleSubscribe takes the sign-up form. HTMX requests get the form fragment back
// (either re-rendered with errors, or in its "check your inbox" state); a plain POST
// gets a full result page.
func (a *App) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// Honeypot: a real browser never fills the hidden "website" field.
	if r.PostFormValue("website") != "" {
		a.renderSubscribe(w, r, SubscribeForm{Done: true})
		return
	}
	form := SubscribeForm{
		Email:   strings.TrimSpace(r.PostFormValue("email")),
		Webhook: strings.TrimSpace(r.PostFormValue("discord_webhook")),
	}
	err := a.subs.Subscribe(r.Context(), form.Email, form.Webhook, clientIP(r))
	var fe *FormError
	switch {
	case err == nil:
		form.Done = true
	case errors.As(err, &fe):
		switch fe.Field {
		case "email":
			form.EmailErr = fe.Msg
		case "discord_webhook":
			form.WebhookErr = fe.Msg
		default:
			form.FormErr = fe.Msg
		}
	default:
		a.serverError(w, err)
		return
	}
	a.renderSubscribe(w, r, form)
}

func (a *App) renderSubscribe(w http.ResponseWriter, r *http.Request, form SubscribeForm) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if r.Header.Get("HX-Request") == "true" {
		if err := a.tmplNights.ExecuteTemplate(w, "subscribe_form", &form); err != nil {
			slog.Error("render subscribe form", "err", err)
		}
		return
	}
	// No-JS fallback: a whole page saying the same thing.
	if form.Done {
		a.renderMessage(w, messagePage{
			Title: "Check your inbox — clearsky", Heading: "Check your inbox", Tone: "ok",
			Lines: []string{
				"If that address isn't already subscribed, a confirmation email is on its way to " + form.Email + ".",
				"Nothing will be sent until you click the link in it. The link is good for 48 hours.",
			},
		})
		return
	}
	msg := form.FormErr
	if msg == "" {
		msg = form.EmailErr
	}
	if msg == "" {
		msg = form.WebhookErr
	}
	a.renderMessage(w, messagePage{
		Title: "Couldn't subscribe — clearsky", Heading: "Couldn't subscribe", Tone: "bad",
		Lines: []string{msg, "Go back and try again."},
	})
}

// handleConfirm activates a subscription from the emailed link.
func (a *App) handleConfirm(w http.ResponseWriter, r *http.Request) {
	res, err := a.subs.Confirm(r.Context(), r.URL.Query().Get("t"))
	switch {
	case errors.Is(err, ErrBadToken):
		a.renderMessage(w, messagePage{
			Title: "Link not valid — clearsky", Heading: "That link isn't valid", Tone: "bad",
			Lines: []string{
				"It may have expired (links last 48 hours) or been replaced by a newer sign-up.",
				"Head back to the log page and sign up again to get a fresh one.",
			},
		})
	case err != nil:
		a.serverError(w, err)
	default:
		sub := res.Sub
		lines := []string{
			"You'll get an email at " + sub.Email + " on nights that look good for imaging.",
			"Remember: the forecast is for Donvale, VIC — Melbourne skies only. NO-GO nights are silent.",
			"Every alert has an unsubscribe link at the bottom.",
		}
		switch {
		case sub.DiscordWebhook != "" && res.DiscordErr == nil:
			lines = append(lines, "A hello has just been posted to your Discord webhook; alerts will land there too.")
		case sub.DiscordWebhook != "":
			lines = append(lines, "Heads up: posting a hello to your Discord webhook failed ("+res.DiscordErr.Error()+"). "+
				"Emails will still arrive; to fix Discord, unsubscribe from any alert and sign up again with the right webhook URL.")
		}
		a.renderMessage(w, messagePage{
			Title: "Subscribed — clearsky", Heading: "You're in 🔭", Tone: "ok", Lines: lines,
		})
	}
}

// handleUnsubscribePage is what the emailed link opens: it names the address and asks
// for one click, so a link-scanning mail gateway cannot unsubscribe someone by
// pre-fetching it. Mail clients' own one-click buttons POST straight to the same URL.
func (a *App) handleUnsubscribePage(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("t")
	sub, err := a.subs.Lookup(r.Context(), token)
	switch {
	case errors.Is(err, ErrBadToken):
		a.renderNotSubscribed(w)
	case err != nil:
		a.serverError(w, err)
	default:
		a.renderMessage(w, messagePage{
			Title: "Unsubscribe — clearsky", Heading: "Unsubscribe from clearsky alerts?",
			Lines:       []string{"This will stop all alerts to " + sub.Email + " and delete the address from the list."},
			Action:      "/subscribe/unsubscribe?t=" + token,
			ActionLabel: "Yes, unsubscribe",
		})
	}
}

// handleUnsubscribe performs the removal. Serves both the page's button and RFC 8058
// one-click POSTs from mail clients (body "List-Unsubscribe=One-Click", token in query).
func (a *App) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	err := a.subs.Unsubscribe(r.Context(), r.URL.Query().Get("t"))
	switch {
	case errors.Is(err, ErrBadToken):
		a.renderNotSubscribed(w)
	case err != nil:
		a.serverError(w, err)
	default:
		a.renderMessage(w, messagePage{
			Title: "Unsubscribed — clearsky", Heading: "You're unsubscribed", Tone: "ok",
			Lines: []string{
				"That address has been removed and won't get any more alerts.",
				"Changed your mind, or want a different Discord webhook? Sign up again from the log page any time.",
			},
		})
	}
}

func (a *App) renderNotSubscribed(w http.ResponseWriter) {
	a.renderMessage(w, messagePage{
		Title: "Not subscribed — clearsky", Heading: "Nothing to unsubscribe",
		Lines: []string{
			"That link doesn't match anyone on the list — most likely it was already used and the address is gone.",
			"If alerts keep arriving, reply to one and it will be sorted by hand.",
		},
	})
}

func (a *App) renderMessage(w http.ResponseWriter, p messagePage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.tmplMessage.ExecuteTemplate(w, "base", p); err != nil {
		slog.Error("render message page", "err", err)
	}
}

// clientIP is the rate-limit key. Behind Caddy the real client is the LAST entry of
// X-Forwarded-For (Caddy appends the peer it saw); with no proxy it is RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
