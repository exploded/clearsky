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

// A GO carried 2-to-1 names the dissenting model; a unanimous one says nothing extra.
func TestMessageNamesOutvotedModels(t *testing.T) {
	m := sampleMessage(t, true)
	if strings.Contains(m.Body(), "Split call") {
		t.Error("unanimous GO must not mention a split")
	}
	m.Outvoted = []string{"icon"}
	if body := m.Body(); !strings.Contains(body, "Split call: icon disagreed") {
		t.Errorf("body should name the outvoted model\n---\n%s", body)
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

// Regression for 25 Oct 2026: rain until midnight, clear from 01:00. The GO was right,
// but "GO tonight … Rain: 1.0 mm, max prob 100%" arrived during the predicted downpour
// and read as a forecast of clear sky now. The alert must name the start time and put
// the rain before the window.
func TestMessageGOAfterWetEvening(t *testing.T) {
	var hours []HourlyPoint
	for h := 19; h <= 23; h++ {
		hours = append(hours, hp(h, 100, 100, 100, 0, 90, 0.3))
	}
	hours = append(hours, hp(0, 60, 60, 0, 0, 10, 0))
	hours = append(hours, run(1, 10, 5, 8, 12, 20)...)

	dark := testDark()
	res := Evaluate(hours, dark, defaultThresholds())
	if !res.GO {
		t.Fatalf("want GO, got NO-GO: %s", res.Reason)
	}
	if res.Rain.ClearsAt != "00:00" {
		t.Errorf("ClearsAt = %q, want 00:00", res.Rain.ClearsAt)
	}

	m := Message{Date: time.Date(2026, 7, 1, 0, 0, 0, 0, testLoc), Source: "ecmwf+gfs+icon",
		Result: res, Dark: dark}
	if sub := m.Subject(); !strings.Contains(sub, "GO for astrophotography from 01:00") {
		t.Errorf("subject: %q", sub)
	}
	body := m.Body()
	for _, want := range []string{"🔭 GO from 01:00 —",
		"Cloud:     peak 100% at 19:00, outside the window",
		"Rain:      no rain forecast 01:00→05:50, earlier rain clears by 00:00 (1.5 mm overnight"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n---\n%s", want, body)
		}
	}
	if strings.Contains(body, "GO tonight") {
		t.Errorf("a window that opens after dusk must not be headlined \"GO tonight\"\n---\n%s", body)
	}
}

// A window that opens at dusk is still "tonight".
func TestMessageGOFromDuskSaysTonight(t *testing.T) {
	dark := testDark()
	res := Evaluate(run(19, 5, 5, 5, 5, 5, 5), dark, defaultThresholds())
	m := Message{Date: time.Date(2026, 7, 1, 0, 0, 0, 0, testLoc), Result: res, Dark: dark}
	if body := m.Body(); !strings.Contains(body, "🔭 GO tonight") ||
		!strings.Contains(body, "no rain forecast 19:02→01:00 (0.0 mm overnight") {
		t.Errorf("body:\n%s", body)
	}
	if sub := m.Subject(); !strings.Contains(sub, "GO for astrophotography tonight") {
		t.Errorf("subject: %q", sub)
	}
}
