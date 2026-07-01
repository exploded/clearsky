package main

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"
)

// emailChannel sends a plaintext email via SMTP with PLAIN auth — designed for Gmail
// using an app password (host smtp.gmail.com, port 587).
type emailChannel struct {
	host string
	port int
	user string
	pass string
	to   string
}

func (e *emailChannel) name() string { return "email" }

func (e *emailChannel) send(_ context.Context, subject, body string) error {
	addr := fmt.Sprintf("%s:%d", e.host, e.port)
	auth := smtp.PlainAuth("", e.user, e.pass, e.host)

	var msg strings.Builder
	fmt.Fprintf(&msg, "From: %s\r\n", e.user)
	fmt.Fprintf(&msg, "To: %s\r\n", e.to)
	fmt.Fprintf(&msg, "Subject: %s\r\n", subject)
	msg.WriteString("MIME-Version: 1.0\r\n")
	msg.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	msg.WriteString("\r\n")
	// Normalise to CRLF line endings for SMTP.
	msg.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))

	return smtp.SendMail(addr, auth, e.user, []string{e.to}, []byte(msg.String()))
}
