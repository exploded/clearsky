package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The worked example from the AWS SigV4 documentation ("Examples of the complete
// Signature Version 4 signing process"): GET iam ListUsers, 2015-08-30T12:36:00Z, with
// the well-known AKIDEXAMPLE credentials. Any drift in canonicalisation shows up here.
func TestSignV4MatchesAWSExample(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	at := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

	signV4(req, nil, "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "us-east-1", "iam", at)

	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-date, " +
		"Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := req.Header.Get("Authorization"); got != want {
		t.Errorf("Authorization\n got %s\nwant %s", got, want)
	}
	if got := req.Header.Get("X-Amz-Date"); got != "20150830T123600Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
}

func TestAwsEscape(t *testing.T) {
	if got := awsEscape("a b/c~d-e_f.g"); got != "a%20b%2Fc~d-e_f.g" {
		t.Errorf("awsEscape = %q", got)
	}
}

func TestBuildMIME(t *testing.T) {
	now := time.Date(2026, 8, 16, 18, 0, 0, 0, time.UTC)
	raw := string(buildMIME("clearsky <alerts@example.com>", Email{
		To:      "someone@example.org",
		Subject: "GO for astrophotography tonight — Donvale (16 Aug)",
		Body:    "🔭 GO tonight\nline two\n",
		Headers: map[string]string{"List-Unsubscribe": "<https://x/u?t=abc>"},
	}, now))
	for _, want := range []string{
		"From: clearsky <alerts@example.com>\r\n",
		"To: someone@example.org\r\n",
		"Subject: =?utf-8?q?", // em dash forces RFC 2047 encoding
		"Content-Transfer-Encoding: quoted-printable\r\n",
		"List-Unsubscribe: <https://x/u?t=abc>\r\n",
		"\r\n\r\n=F0=9F=94=AD GO tonight\r\nline two\r\n",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("MIME missing %q\n%s", want, raw)
		}
	}
	// A CR/LF-free header block: nothing user-controlled may inject a line.
	if strings.Contains(strings.SplitN(raw, "\r\n\r\n", 2)[0], "\n\n") {
		t.Errorf("bare LF inside header block")
	}
}

func TestNormalizeEmail(t *testing.T) {
	good := map[string]string{
		"Someone@Example.org ": "someone@example.org",
		"a.b+c@sub.example.au": "a.b+c@sub.example.au",
	}
	for in, want := range good {
		got, err := normalizeEmail(in)
		if err != nil || got != want {
			t.Errorf("normalizeEmail(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{"", "nope", "a@b", "Bob <bob@example.com>", "a@b.com,c@d.com",
		"a@b.com\r\nBcc: x@y.com", "\"quoted\"@example.com"}
	for _, in := range bad {
		if _, err := normalizeEmail(in); err == nil {
			t.Errorf("normalizeEmail(%q) accepted", in)
		}
	}
}
