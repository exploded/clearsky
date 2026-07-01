package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeChannel records what it was asked to send and can simulate failure.
type fakeChannel struct {
	sent     int
	lastSub  string
	lastBody string
	fail     bool
}

func (f *fakeChannel) name() string { return "fake" }
func (f *fakeChannel) send(_ context.Context, subject, body string) error {
	f.sent++
	f.lastSub, f.lastBody = subject, body
	if f.fail {
		return context.DeadlineExceeded
	}
	return nil
}

func sampleMessage(t *testing.T, going bool) Message {
	loc := mustMelbourne(t)
	date := time.Date(2026, 7, 1, 0, 0, 0, 0, loc)
	dark := Darkness{
		Dusk: time.Date(2026, 7, 1, 18, 45, 0, 0, loc),
		Dawn: time.Date(2026, 7, 2, 6, 2, 0, 0, loc),
		Kind: "astronomical",
	}
	rise := time.Date(2026, 7, 1, 21, 3, 0, 0, loc)
	return Message{
		Date:   date,
		Source: "open-meteo",
		Result: Result{GO: going, Score: 82, Reason: "clear: avg 8% cloud, peak 22%, no rain",
			Cloud: CloudSummary{Avg: 8, Max: 22, PeakAt: "00:00", MaxHigh: 22},
			Rain:  RainSummary{TotalMm: 0, MaxProbPct: 5}},
		Dark: dark,
		Moon: MoonInfo{IllumPct: 12, PhaseName: "Waxing Crescent", Set: &rise},
	}
}

func TestMessageFormatting(t *testing.T) {
	m := sampleMessage(t, true)
	if !strings.Contains(m.Subject(), "GO for astrophotography tonight") {
		t.Errorf("subject: %q", m.Subject())
	}
	body := m.Body()
	for _, want := range []string{"GO tonight", "Score 82/100", "open-meteo",
		"dusk 18:45", "dawn Thu 06:02", "avg 8%", "Moon:", "Waxing Crescent", "sets 21:03"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}
}

func TestNotifierFanoutAndResilience(t *testing.T) {
	ok := &fakeChannel{}
	bad := &fakeChannel{fail: true}
	n := &Notifier{channels: []channel{bad, ok}}
	if !n.Enabled() {
		t.Fatal("expected enabled")
	}
	// A failing channel must not stop the others; Notify never panics or blocks.
	n.Notify(context.Background(), sampleMessage(t, true))
	if ok.sent != 1 || bad.sent != 1 {
		t.Errorf("each channel should be attempted once: ok=%d bad=%d", ok.sent, bad.sent)
	}
}

func TestNotifierDisabled(t *testing.T) {
	n := &Notifier{}
	if n.Enabled() {
		t.Error("no channels => disabled")
	}
	n.Notify(context.Background(), sampleMessage(t, true)) // must be a no-op, no panic
}
