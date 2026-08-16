package main

import "context"

// emailChannel is the owner's email notification: one fixed recipient over the SES
// mailer (or the dry-run log mailer in dev).
type emailChannel struct {
	mailer Mailer
	to     string
}

func (e *emailChannel) name() string { return "email/" + e.mailer.name() }

func (e *emailChannel) send(ctx context.Context, subject, body string) error {
	return e.mailer.Send(ctx, Email{To: e.to, Subject: subject, Body: body})
}
