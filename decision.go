package main

import (
	"fmt"
	"time"
)

// fullSessionHours is what counts as "a full imaging session" when scoring window
// length. Beyond this, extra clear hours no longer improve the score — six hours of
// clear sky is already a complete night's work.
const fullSessionHours = 6

// maxHourGap is the largest spacing between consecutive samples that still counts as
// contiguous. Providers thin out to 3- or 6-hourly further into the horizon; without
// this a run could silently bridge a gap it has no data for.
const maxHourGap = 90 * time.Minute

// CloudSummary and RainSummary are the persisted JSON snapshots (into nights.cloud_summary
// / rain_summary) and are also reused in the notification text. Both describe the WHOLE
// dark window and are informational — the decision is made on the usable window below.
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
	AnyHour    bool    `json:"anyHour"` // any hour of the night breached a rain veto
	At         string  `json:"at"`      // local HH:MM of the worst rain hour ("" if none)
	PackUpAt   string  `json:"packUpAt"`// local HH:MM rain arrives after the usable window ("" if none)
}

// WindowSummary is the longest contiguous run of dark hours that each pass the
// per-hour usability gate — the session you would realistically image. This, not the
// whole-night average, is what the GO/NO-GO decision is made on. Persisted as JSON
// into nights.window_json.
type WindowSummary struct {
	StartAt  int64  `json:"startAt"`  // unix; 0 when no hour was usable
	EndAt    int64  `json:"endAt"`    // unix, exclusive (last usable hour + 1h, clamped to dawn)
	Label    string `json:"label"`    // site-local "19:02→00:00", "—" when empty
	Hours    int    `json:"hours"`    // number of usable hourly buckets
	AvgCloud int    `json:"avgCloud"` // mean total cloud over the window (%)
	MaxCloud int    `json:"maxCloud"` // peak total cloud over the window (%)
}

// Result is the outcome of evaluating a night.
type Result struct {
	GO     bool
	Score  int // 0..100: window clearness scaled by window length
	Reason string
	Cloud  CloudSummary
	Rain   RainSummary
	Window WindowSummary
}

// Evaluate decides GO / NO-GO for a night. hours must already be filtered to the
// [dusk, dawn] window; dark supplies the exact boundaries used to clamp the reported
// window. The moon is never consulted here — it is display-only.
//
// The night is judged on its best contiguous run of imageable hours, never on
// whole-night aggregates: a single overcast hour at 03:00 must not discard a clear
// evening. Rain is handled at the hour level (a raining hour is simply not usable),
// with a pack-up warning when rain arrives soon after the window closes.
func Evaluate(hours []HourlyPoint, dark Darkness, th Thresholds) Result {
	if len(hours) == 0 {
		return Result{Reason: "no forecast hours in the darkness window"}
	}

	cloud, rain := summarize(hours, th)
	win, winHours := bestWindow(hours, dark, th)
	rain.PackUpAt = packUpAt(hours, winHours, th)

	res := Result{
		Score:  windowScore(win, len(hours)),
		Cloud:  cloud,
		Rain:   rain,
		Window: win,
	}

	// 1. Enough usable sky to be worth setting up for.
	if win.Hours < th.MinUsableHours {
		res.Reason = shortWindowReason(win, cloud, th)
		return res
	}

	// 2. The window itself must be genuinely clear on average — the per-hour gate
	//    tolerates individual marginal hours, this stops a run of them passing.
	if win.AvgCloud > th.CloudAvgMaxPct {
		res.Reason = fmt.Sprintf("best window %s (%dh) too cloudy: avg %d%%, peak %d%%",
			win.Label, win.Hours, win.AvgCloud, win.MaxCloud)
		return res
	}

	// 3. Trace-precipitation veto over the window. Individually-raining hours are
	//    already excluded above, so this only fires when RainMmVetoHour is set to
	//    tolerate traces and those traces accumulate.
	if mm := totalMm(winHours); mm > th.RainMmTotalVeto {
		res.Reason = fmt.Sprintf("rain over the usable window %s: %.1fmm total", win.Label, mm)
		return res
	}

	res.GO = true
	res.Reason = fmt.Sprintf("clear %s — %dh usable, avg %d%% cloud", win.Label, win.Hours, win.AvgCloud)
	if rain.PackUpAt != "" {
		res.Reason += fmt.Sprintf("; rain from %s, pack up by then", rain.PackUpAt)
	}
	return res
}

// shortWindowReason explains a NO-GO caused by too few usable hours, naming the best
// run found (if any) so a near miss is visible rather than hidden behind an average.
func shortWindowReason(win WindowSummary, cloud CloudSummary, th Thresholds) string {
	if win.Hours == 0 {
		return fmt.Sprintf("no usable hours: avg %d%% cloud, peak %d%% at %s (low %d%%, mid %d%%)",
			cloud.Avg, cloud.Max, cloud.PeakAt, cloud.MaxLow, cloud.MaxMid)
	}
	return fmt.Sprintf("only %dh usable (%s, avg %d%% cloud) — need %dh",
		win.Hours, win.Label, win.AvgCloud, th.MinUsableHours)
}

// hourUsable reports whether a single hour is imageable on its own merits. Low and
// mid cloud get their own caps because they block far more than thin cirrus does.
func hourUsable(h HourlyPoint, th Thresholds) bool {
	if hourRains(h, th) {
		return false
	}
	return h.CloudTotal <= th.CloudMaxPct &&
		h.CloudLow <= th.CloudLowMaxPct &&
		h.CloudMid <= th.CloudMidMaxPct
}

func hourRains(h HourlyPoint, th Thresholds) bool {
	return h.PrecipMm > th.RainMmVetoHour || h.PrecipProbPct > th.RainProbVetoPct
}

// bestWindow returns the longest contiguous run of usable hours, breaking ties in
// favour of the clearer run. It returns both the summary and the hours themselves so
// callers can inspect the window without recomputing it.
func bestWindow(hours []HourlyPoint, dark Darkness, th Thresholds) (WindowSummary, []HourlyPoint) {
	var best, cur []HourlyPoint
	flush := func() {
		if betterRun(cur, best) {
			best = cur
		}
		cur = nil
	}
	for i, h := range hours {
		if !hourUsable(h, th) {
			flush()
			continue
		}
		// A gap in the data breaks the run — we cannot vouch for hours we did not see.
		if len(cur) > 0 && h.At.Sub(hours[i-1].At) > maxHourGap {
			flush()
		}
		cur = append(cur, h)
	}
	flush()
	return summarizeWindow(best, dark), best
}

// betterRun ranks candidate runs: longer wins, and equal lengths are broken by the
// lower average cloud.
func betterRun(a, b []HourlyPoint) bool {
	if len(a) != len(b) {
		return len(a) > len(b)
	}
	if len(a) == 0 {
		return false
	}
	return meanCloud(a) < meanCloud(b)
}

func meanCloud(hs []HourlyPoint) int {
	sum := 0
	for _, h := range hs {
		sum += h.CloudTotal
	}
	return sum / len(hs)
}

func totalMm(hs []HourlyPoint) float64 {
	var mm float64
	for _, h := range hs {
		mm += h.PrecipMm
	}
	return mm
}

// summarizeWindow reduces the run to its persisted form. The reported start/end are
// clamped to true darkness so a 19:00 bucket under a 19:02 dusk reads as "19:02".
func summarizeWindow(hs []HourlyPoint, dark Darkness) WindowSummary {
	if len(hs) == 0 {
		return WindowSummary{Label: "—"}
	}
	start, end := hs[0].At, hs[len(hs)-1].At.Add(time.Hour)
	if !dark.Dusk.IsZero() && start.Before(dark.Dusk) {
		start = dark.Dusk
	}
	if !dark.Dawn.IsZero() && end.After(dark.Dawn) {
		end = dark.Dawn
	}

	var sum, mx int
	for _, h := range hs {
		sum += h.CloudTotal
		if h.CloudTotal > mx {
			mx = h.CloudTotal
		}
	}
	return WindowSummary{
		StartAt:  start.Unix(),
		EndAt:    end.Unix(),
		Label:    fmt.Sprintf("%s→%s", start.Format("15:04"), end.Format("15:04")),
		Hours:    len(hs),
		AvgCloud: sum / len(hs),
		MaxCloud: mx,
	}
}

// windowScore rates the night as window clearness scaled by how much of a session it
// gives you. Length is measured against a full session, or against the whole dark
// window on short summer nights — four clear hours out of five available is a good
// night, four out of eleven is a decent half-night.
func windowScore(w WindowSummary, darkHours int) int {
	if w.Hours == 0 || darkHours <= 0 {
		return 0
	}
	target := min(darkHours, fullSessionHours)
	length := float64(min(w.Hours, target)) / float64(target)
	quality := float64(clamp(100-w.AvgCloud, 0, 100))
	return clamp(int(quality*length+0.5), 0, 100)
}

// packUpAt finds the first raining hour that falls after the usable window closes,
// within RainMarginHours. That is a GO with a deadline, not a NO-GO.
func packUpAt(all, win []HourlyPoint, th Thresholds) string {
	if len(win) == 0 || th.RainMarginHours <= 0 {
		return ""
	}
	end := win[len(win)-1].At.Add(time.Hour)
	limit := end.Add(time.Duration(th.RainMarginHours) * time.Hour)
	for _, h := range all {
		if h.At.Before(end) || h.At.After(limit) {
			continue
		}
		if hourRains(h, th) {
			return h.At.Format("15:04")
		}
	}
	return ""
}

// summarize reduces the whole dark window to cloud + rain summaries. These are
// display/notification detail only; the decision uses the usable window instead.
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
		if hourRains(h, th) {
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
