package main

import (
	"fmt"
	"strings"
)

// SourceVerdict is one provider's independent opinion of the same night, decided with
// the identical rules and thresholds used for the real decision.
//
// The GO/NO-GO the app acts on is made on the pessimistic MERGE of every source, which
// by construction cannot be more optimistic than its gloomiest member. That is the
// right way to decide, but it throws away the thing you most want to know the morning
// after a bust: did the models actually agree, or did one of them carry the night?
type SourceVerdict struct {
	Name        string `json:"name"`
	GO          bool   `json:"go"`
	WindowLabel string `json:"window"`    // this source's own best usable window
	Hours       int    `json:"hours"`     // usable hours it found
	WindowAvg   int    `json:"windowAvg"` // mean cloud over its window (%)
	NightAvg    int    `json:"nightAvg"`  // mean cloud over the whole dark window (%)
}

// Agreement is the persisted per-source breakdown (nights.sources_json).
type Agreement struct {
	Sources []SourceVerdict `json:"sources"`
	GoCount int             `json:"goCount"` // how many sources would have said GO alone
	Spread  int             `json:"spread"`  // max-min whole-night mean cloud across sources (%)

	// Missing names configured models that never answered. A night decided without
	// them is a weaker night, and saying so is the whole point: the log page cannot
	// otherwise tell a unanimous three-model GO from one model talking to itself.
	Missing []string `json:"missing,omitempty"`
}

// Label summarises the agreement for a table cell: "1/3 agree · spread 36%". A run
// that lost models names them instead of quietly reporting a smaller panel as if it
// were the whole panel — "1/2 agree" hides that a third model was meant to vote.
func (a Agreement) Label() string {
	if len(a.Sources) == 0 {
		return ""
	}
	if len(a.Sources) == 1 {
		if len(a.Missing) == 0 {
			return ""
		}
		return fmt.Sprintf("%s only · %s missing", a.Sources[0].Name, strings.Join(a.Missing, ", "))
	}
	label := fmt.Sprintf("%d/%d agree · spread %d%%", a.GoCount, len(a.Sources), a.Spread)
	if len(a.Missing) > 0 {
		label += fmt.Sprintf(" · %s missing", strings.Join(a.Missing, ", "))
	}
	return label
}

// Degraded reports whether the night was decided on fewer models than configured.
func (a Agreement) Degraded() bool { return len(a.Missing) > 0 }

// Unanimous reports whether every source independently reached the same verdict. A
// split decision is the signal to go outside and look up before committing.
func (a Agreement) Unanimous() bool {
	return len(a.Sources) > 0 && (a.GoCount == 0 || a.GoCount == len(a.Sources))
}

// summarizeAgreement evaluates each contributing source on its own, over the same
// darkness window and thresholds as the real decision. Returns a zero Agreement for a
// single-provider run (no Members), where there is nothing to agree or disagree about.
//
// A one-member Agreement is NOT the same thing: it means a multi-source run lost every
// model but one, and that fact is recorded rather than falling back to the bare source
// name, which reads as an ordinary night.
func summarizeAgreement(fc Forecast, dark Darkness, th Thresholds) Agreement {
	if len(fc.Members) == 0 {
		return Agreement{}
	}

	ag := Agreement{Missing: fc.Missing}
	lo, hi := -1, -1
	for _, m := range fc.Members {
		hours := m.HoursWithin(dark.Dusk, dark.Dawn)
		if len(hours) == 0 {
			continue
		}
		res := Evaluate(hours, dark, th)
		if res.GO {
			ag.GoCount++
		}
		ag.Sources = append(ag.Sources, SourceVerdict{
			Name:        m.Source,
			GO:          res.GO,
			WindowLabel: res.Window.Label,
			Hours:       res.Window.Hours,
			WindowAvg:   res.Window.AvgCloud,
			NightAvg:    res.Cloud.Avg,
		})
		if lo < 0 || res.Cloud.Avg < lo {
			lo = res.Cloud.Avg
		}
		if res.Cloud.Avg > hi {
			hi = res.Cloud.Avg
		}
	}
	if lo >= 0 {
		ag.Spread = hi - lo
	}
	return ag
}
