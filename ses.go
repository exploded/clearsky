package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// sesMailer sends through the Amazon SES v2 API (SendEmail with raw MIME content) over
// plain HTTPS with a hand-rolled SigV4 signature. That is deliberate: the AWS SDK would
// add a dozen modules to a project that has two direct dependencies, and the whole of
// SigV4 is a hundred lines of hashing. Only static IAM user credentials are supported —
// the policy needs ses:SendEmail and ses:SendRawEmail on the sending identity.
//
// Sandbox note: a fresh SES account can only send TO verified addresses. Request
// production access in the SES console before opening the subscribe form to anyone.
type sesMailer struct {
	region    string
	accessKey string
	secretKey string
	from      string // verified identity, "Name <addr>" form is fine
	client    *http.Client
	now       func() time.Time
}

func newSESMailer(cfg Config) *sesMailer {
	return &sesMailer{
		region:    cfg.SESRegion,
		accessKey: cfg.SESAccessKeyID,
		secretKey: cfg.SESSecretAccessKey,
		from:      cfg.SESFrom,
		client:    &http.Client{Timeout: 20 * time.Second},
		now:       time.Now,
	}
}

func (s *sesMailer) name() string { return "ses" }

func (s *sesMailer) Send(ctx context.Context, e Email) error {
	raw := buildMIME(s.from, e, s.now())
	body, err := json.Marshal(map[string]any{
		"FromEmailAddress": s.from,
		"Destination":      map[string]any{"ToAddresses": []string{e.To}},
		"Content":          map[string]any{"Raw": map[string]any{"Data": base64.StdEncoding.EncodeToString(raw)}},
	})
	if err != nil {
		return err
	}
	endpoint := fmt.Sprintf("https://email.%s.amazonaws.com/v2/email/outbound-emails", s.region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	signV4(req, body, s.accessKey, s.secretKey, s.region, "ses", s.now().UTC())

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("ses: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ses status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// signV4 adds X-Amz-Date and Authorization headers per AWS Signature Version 4.
// Signed headers are host, content-type (if set) and every x-amz-* header present.
// The path is used as-is (SES paths carry nothing that needs encoding) and the query
// string is re-encoded per the SigV4 rules (RFC 3986 unreserved set, %20 for space).
func signV4(req *http.Request, payload []byte, accessKey, secretKey, region, service string, t time.Time) {
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "x-amz-") {
			headers[lk] = strings.Join(v, ",")
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + strings.Join(strings.Fields(headers[k]), " ") + "\n")
	}
	signedHeaders := strings.Join(names, ";")

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	payloadHash := sha256Hex(payload)
	canonical := strings.Join([]string{
		req.Method, path, canonicalQuery(req.URL.Query()),
		canonHeaders.String(), signedHeaders, payloadHash,
	}, "\n")

	scope := date + "/" + region + "/" + service + "/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex([]byte(canonical))

	k := hmacSHA256([]byte("AWS4"+secretKey), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		accessKey, scope, signedHeaders, sig))
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

// awsEscape percent-encodes everything outside RFC 3986's unreserved set, with
// upper-case hex — url.QueryEscape's '+' for space is not accepted here.
func awsEscape(s string) string {
	const hexd = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexd[c>>4])
		b.WriteByte(hexd[c&15])
	}
	return b.String()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
