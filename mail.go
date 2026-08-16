package main

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"os"
	"sort"
	"strings"
	"time"
)

// Email is one outbound plaintext message. Headers carries extras such as
// List-Unsubscribe; the standard From/To/Subject/MIME headers are always written.
type Email struct {
	To      string
	Subject string
	Body    string
	Headers map[string]string
}

// Mailer is a transport for a single email. Production is AWS SES (sesMailer); dev
// can swap in logMailer. Both the owner's alerts and the subscriber fanout use it.
type Mailer interface {
	Send(ctx context.Context, e Email) error
	name() string
}

// logMailer is the CLEARSKY_MAIL_DRY_RUN transport: the full message goes to the log
// and nothing leaves the box. For exercising the sign-up flow on a dev machine.
type logMailer struct{}

func (logMailer) name() string { return "log" }

func (logMailer) Send(_ context.Context, e Email) error {
	slog.Info("DRY RUN email (not sent)", "to", e.To, "subject", e.Subject, "headers", e.Headers)
	fmt.Fprintf(os.Stderr, "----- dry-run email to %s -----\n%s\n----- end -----\n", e.To, e.Body)
	return nil
}

// buildMIME renders a complete RFC 5322 message: CRLF line endings, an RFC 2047
// encoded subject (the subjects here contain em dashes), and a quoted-printable body so
// emoji and non-ASCII survive every hop without relying on 8BITMIME.
func buildMIME(from string, e Email, now time.Time) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", e.To)
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", e.Subject))
	fmt.Fprintf(&b, "Date: %s\r\n", now.Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n")
	// Deterministic header order keeps the output stable for tests and diffs.
	keys := make([]string, 0, len(e.Headers))
	for k := range e.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\r\n", k, e.Headers[k])
	}
	b.WriteString("\r\n")
	w := quotedprintable.NewWriter(&b)
	_, _ = w.Write([]byte(strings.ReplaceAll(e.Body, "\r\n", "\n")))
	_ = w.Close()
	return []byte(b.String())
}

// normalizeEmail validates a bare address ("user@host.tld", no display name) and
// returns it lower-cased. It is strict on purpose: the value goes straight into a
// To: header, so anything that could smuggle a second header or recipient is refused.
func normalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("enter an email address")
	}
	if len(s) > 254 || strings.ContainsAny(s, " \t\r\n<>,;\"()") {
		return "", fmt.Errorf("that doesn't look like a valid email address")
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Name != "" || !strings.EqualFold(addr.Address, s) {
		return "", fmt.Errorf("that doesn't look like a valid email address")
	}
	at := strings.LastIndex(s, "@")
	if at <= 0 || !strings.Contains(s[at+1:], ".") {
		return "", fmt.Errorf("that doesn't look like a valid email address")
	}
	return strings.ToLower(s), nil
}
