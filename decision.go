package main

import (
	"fmt"
	"time"
)

// CloudSummary and RainSummary are the persisted JSON snapshots (into nights.cloud_summary
// / rain_summary) and are also reused in the notification text.
type CloudSummary struct {
	Avg     int    `json:"avg"`     // mean total cloud across the window (%)
	Max     int    `json:"max"`     // peak total cloud (%)
	MaxLow  int    `json:"maxLow"`  // peak low cloud (%)
	MaxMid  int    `json:"maxMid"`  // peak mid cloud (%)
	MaxHigh int    `json:"maxHigh"` // peak high cloud (%)
	PeakAt  string `json:"peakAt"`  // local HH:MM of the peak total-cloud hour
}

type RainSummary struct {
	TotalMm    float64 `json:"totalMm"`
	MaxProbPct int     `json:"maxProbPct"`
	AnyHour    bool    `json:"anyHour"` // any hour breached a rain veto
	At         string  `json:"at"`      // local HH:MM of the worst rain hour ("" if none)
}

// Result is the outcome of evaluating a night.
type Result struct {
	GO     bool
	Score  int // 0..100 cloud clearness, for ranking/display only (not a gate)
	Reason string
	Cloud  CloudSummary
	Rain   RainSummary
}

// Evaluate decides GO / NO-GO from the darkness-window hours only. The decision is
// purely rain veto + cloud gate; the moon is never consulted here. hours must already
// be filtered to the [dusk, dawn] window.
func Evaluate(hours []HourlyPoint, th Thresholds) Result {
	if len(hours) == 0 {
		return Result{GO: false, Reason: "no forecast hours in the darkness window"}
	}

	cloud, rain := summarize(hours, th)
	score := clamp(100-cloud.Avg, 0, 100)

	// 1. Rain veto — hard NO-GO if any measurable rain (or too-high probability) is
	//    forecast during the dark hours.
	if rain.AnyHour {
		reason := fmt.Sprintf("rain forecast: %.1fmm total, max prob %d%%", rain.TotalMm, rain.MaxProbPct)
		if rain.At != "" {
			reason = fmt.Sprintf("rain forecast at %s: %.1fmm total, max prob %d%%", rain.At, rain.TotalMm, rain.MaxProbPct)
		}
		return Result{GO: false, Score: score, Reason: reason, Cloud: cloud, Rain: rain}
	}

	// 2. Cloud gate — must be "mostly clear": low mean, bounded peak, and tighter caps
	//    on low/mid cloud (worse for imaging than thin high cloud).
	if cloud.Avg > th.CloudAvgMaxPct || cloud.Max > th.CloudMaxPct ||
		cloud.MaxLow > th.CloudLowMaxPct || cloud.MaxMid > th.CloudMidMaxPct {
		reason := fmt.Sprintf("too cloudy: avg %d%%, peak %d%% at %s (low %d%%, mid %d%%)",
			cloud.Avg, cloud.Max, cloud.PeakAt, cloud.MaxLow, cloud.MaxMid)
		return Result{GO: false, Score: score, Reason: reason, Cloud: cloud, Rain: rain}
	}

	// GO.
	reason := fmt.Sprintf("clear: avg %d%% cloud, peak %d%%, no rain", cloud.Avg, cloud.Max)
	return Result{GO: true, Score: score, Reason: reason, Cloud: cloud, Rain: rain}
}

// summarize reduces the window hours to cloud + rain summaries and flags whether any
// hour breaches a rain veto threshold.
func summarize(hours []HourlyPoint, th Thresholds) (CloudSummary, RainSummary) {
	var cloud CloudSummary
	var rain RainSummary
	var cloudSum, worstRainScore int
	peakAt, rainAt := time.Time{}, time.Time{}

	for _, h := range hours {
		cloudSum += h.CloudTotal
		if h.CloudTotal > cloud.Max {
			cloud.Max = h.CloudTotal
			peakAt = h.At
		}
		if h.CloudLow > cloud.MaxLow {
			cloud.MaxLow = h.CloudLow
		}
		if h.CloudMid > cloud.MaxMid {
			cloud.MaxMid = h.CloudMid
		}
		if h.CloudHigh > cloud.MaxHigh {
			cloud.MaxHigh = h.CloudHigh
		}

		rain.TotalMm += h.PrecipMm
		if h.PrecipProbPct > rain.MaxProbPct {
			rain.MaxProbPct = h.PrecipProbPct
		}
		// Per-hour rain veto: measurable precip OR too-high probability.
		if h.PrecipMm > th.RainMmVetoHour || h.PrecipProbPct > th.RainProbVetoPct {
			rain.AnyHour = true
			// Track the "worst" offending hour for the reason string.
			s := h.PrecipProbPct + int(h.PrecipMm*100)
			if s > worstRainScore {
				worstRainScore = s
				rainAt = h.At
			}
		}
	}

	cloud.Avg = cloudSum / len(hours)
	if !peakAt.IsZero() {
		cloud.PeakAt = peakAt.Format("15:04")
	}
	// Window-total precip veto.
	if rain.TotalMm > th.RainMmTotalVeto {
		rain.AnyHour = true
	}
	if !rainAt.IsZero() {
		rain.At = rainAt.Format("15:04")
	}
	return cloud, rain
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
