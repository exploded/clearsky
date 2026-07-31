package main

import (
	"testing"
	"time"
)

func defaultThresholds() Thresholds {
	return Thresholds{
		RainMmVetoHour:  0.0,
		RainProbVetoPct: 35,
		RainMmTotalVeto: 0.2,
		RainMarginHours: 2,
		MinUsableHours:  3,
		CloudAvgMaxPct:  25,
		CloudMaxPct:     40,
		CloudLowMaxPct:  30,
		CloudMidMaxPct:  30,
		VisibilityMinM:  20000,
	}
}

var testLoc = mustLoad("Australia/Melbourne")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// hp builds an hourly point with total cloud split across layers for brevity. Hours
// 0..11 are read as the small hours of the following morning, so a night reads in
// chronological order.
func hp(hour, cloud, low, mid, high, prob int, mm float64) HourlyPoint {
	day := 1
	if hour < 12 {
		day = 2
	}
	return HourlyPoint{
		At:            time.Date(2026, 7, day, hour, 0, 0, 0, testLoc),
		CloudTotal:    cloud,
		CloudLow:      low,
		CloudMid:      mid,
		CloudHigh:     high,
		PrecipProbPct: prob,
		PrecipMm:      mm,
		VisibilityM:   24000,
	}
}

// testDark is a plausible mid-winter Melbourne dark window for the nights below.
func testDark() Darkness {
	return Darkness{
		Dusk: time.Date(2026, 7, 1, 19, 2, 0, 0, testLoc),
		Dawn: time.Date(2026, 7, 2, 5, 50, 0, 0, testLoc),
		Kind: "astronomical",
	}
}

// run builds a night of consecutive hourly points from a list of total-cloud values,
// starting at startHour, with the cloud carried entirely as LOW cloud (the worst case
// and what Open-Meteo reports for Melbourne stratus).
func run(startHour int, clouds ...int) []HourlyPoint {
	out := make([]HourlyPoint, 0, len(clouds))
	for i, c := range clouds {
		out = append(out, hp((startHour+i)%24, c, c, 0, 0, 0, 0))
	}
	return out
}

func TestEvaluate(t *testing.T) {
	th := defaultThresholds()
	dark := testDark()
	tests := []struct {
		name      string
		hours     []HourlyPoint
		wantGO    bool
		wantHours int // expected usable-window length; -1 to skip the check
	}{
		{
			name:      "clear night -> GO",
			hours:     run(20, 5, 10, 15, 10, 5),
			wantGO:    true,
			wantHours: 5,
		},
		{
			// The case that motivated the windowed evaluator: clear until midnight,
			// overcast after. Whole-night aggregates called this a washout.
			name:      "clear first half, overcast second half -> GO on the first half",
			hours:     run(19, 1, 9, 15, 16, 6, 95, 95, 98, 100, 100, 100),
			wantGO:    true,
			wantHours: 5,
		},
		{
			name:      "one bad hour mid-run no longer kills the night",
			hours:     run(20, 5, 5, 90, 5, 5, 5, 5),
			wantGO:    true,
			wantHours: 4, // the longer run after the bad hour
		},
		{
			name:      "clear run too short -> NO-GO",
			hours:     run(20, 5, 5, 100, 100, 100, 100),
			wantGO:    false,
			wantHours: 2,
		},
		{
			name:      "overcast all night -> NO-GO",
			hours:     run(20, 95, 98, 100, 100, 100),
			wantGO:    false,
			wantHours: 0,
		},
		{
			name: "rain inside the clear run splits it -> NO-GO",
			hours: []HourlyPoint{
				hp(20, 5, 5, 0, 0, 0, 0), hp(21, 5, 5, 0, 0, 0, 0),
				hp(22, 5, 5, 0, 0, 10, 0.3), // rains
				hp(23, 5, 5, 0, 0, 0, 0), hp(0, 5, 5, 0, 0, 0, 0),
			},
			wantGO:    false,
			wantHours: 2,
		},
		{
			// Rain after the window is a deadline, not a veto.
			name: "rain after the usable window -> GO with a pack-up time",
			hours: []HourlyPoint{
				hp(20, 5, 5, 0, 0, 0, 0), hp(21, 5, 5, 0, 0, 0, 0), hp(22, 5, 5, 0, 0, 0, 0),
				hp(23, 5, 5, 0, 0, 0, 0), hp(0, 90, 90, 0, 0, 80, 0.5),
			},
			wantGO:    true,
			wantHours: 4,
		},
		{
			// Each hour clears the per-hour caps (thin cirrus only), but five of them
			// in a row average well over the window cap.
			name: "long run of marginal hours fails the window average",
			hours: []HourlyPoint{
				hp(20, 38, 0, 0, 38, 0, 0), hp(21, 39, 0, 0, 39, 0, 0), hp(22, 40, 0, 0, 40, 0, 0),
				hp(23, 38, 0, 0, 38, 0, 0), hp(0, 39, 0, 0, 39, 0, 0),
			},
			wantGO:    false,
			wantHours: 5,
		},
		{
			name:      "thin high cloud only -> GO",
			hours:     []HourlyPoint{hp(20, 20, 0, 0, 20, 0, 0), hp(21, 30, 5, 5, 30, 0, 0), hp(22, 25, 0, 0, 25, 0, 0)},
			wantGO:    true,
			wantHours: 3,
		},
		{
			name:      "thick low cloud fails the per-hour cap even at moderate totals",
			hours:     run(20, 35, 35, 35),
			wantGO:    false,
			wantHours: 0, // low == total == 35 > CloudLowMaxPct 30
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.hours, dark, th)
			if got.GO != tc.wantGO {
				t.Errorf("GO=%v want %v (reason: %s)", got.GO, tc.wantGO, got.Reason)
			}
			if tc.wantHours >= 0 && got.Window.Hours != tc.wantHours {
				t.Errorf("window hours = %d, want %d (%s)", got.Window.Hours, tc.wantHours, got.Window.Label)
			}
			if got.Reason == "" {
				t.Error("expected a non-empty reason")
			}
			if got.Score < 0 || got.Score > 100 {
				t.Errorf("score %d out of range", got.Score)
			}
		})
	}
}

// TestEvaluateRegression2026_07_31 replays the real Open-Meteo forecast for the night
// of 31 Jul 2026 — clear 19:00–00:00, overcast from 00:00. The whole-night gate scored
// this 37 and called it NO-GO, discarding five genuinely clear hours.
func TestEvaluateRegression2026_07_31(t *testing.T) {
	hours := run(19, 1, 9, 15, 16, 6, 95, 95, 98, 100, 100, 100)
	got := Evaluate(hours, testDark(), defaultThresholds())

	if !got.GO {
		t.Fatalf("want GO, got NO-GO: %s", got.Reason)
	}
	if got.Window.Hours != 5 {
		t.Errorf("usable hours = %d, want 5", got.Window.Hours)
	}
	// Clamped to the 19:02 dusk rather than the 19:00 bucket start.
	if got.Window.Label != "19:02→00:00" {
		t.Errorf("window label = %q, want %q", got.Window.Label, "19:02→00:00")
	}
	if got.Window.AvgCloud != 9 {
		t.Errorf("window avg cloud = %d%%, want 9%%", got.Window.AvgCloud)
	}
	// The whole-night average is still recorded — it is just no longer the gate.
	// Production logged 63% over the ten hours it kept; including the 19:00 bucket
	// that the old strict dusk comparison discarded brings it to 57%.
	if got.Cloud.Avg != 57 {
		t.Errorf("night avg cloud = %d%%, want 57%%", got.Cloud.Avg)
	}
	if got.Score <= 37 {
		t.Errorf("score = %d, want well above the old whole-night score of 37", got.Score)
	}
}

func TestEvaluateEmpty(t *testing.T) {
	got := Evaluate(nil, testDark(), defaultThresholds())
	if got.GO {
		t.Error("empty window must be NO-GO")
	}
	if got.Score != 0 {
		t.Errorf("score = %d, want 0", got.Score)
	}
}

// A gap in the data must break a run — we cannot vouch for hours we never saw.
func TestBestWindowBreaksOnDataGap(t *testing.T) {
	hours := []HourlyPoint{
		hp(20, 5, 5, 0, 0, 0, 0), hp(21, 5, 5, 0, 0, 0, 0),
		// 22:00 and 23:00 missing entirely.
		hp(0, 5, 5, 0, 0, 0, 0), hp(1, 5, 5, 0, 0, 0, 0),
	}
	win, _ := bestWindow(hours, testDark(), defaultThresholds())
	if win.Hours != 2 {
		t.Errorf("window hours = %d, want 2 (the gap must not be bridged)", win.Hours)
	}
}

func TestWindowScore(t *testing.T) {
	tests := []struct {
		name      string
		win       WindowSummary
		darkHours int
		wantMin   int
		wantMax   int
	}{
		{"full clear night", WindowSummary{Hours: 10, AvgCloud: 5}, 11, 94, 96},
		{"half a long night, very clear", WindowSummary{Hours: 5, AvgCloud: 9}, 11, 74, 78},
		{"short summer night used fully", WindowSummary{Hours: 5, AvgCloud: 10}, 5, 88, 92},
		{"most of a short summer night", WindowSummary{Hours: 4, AvgCloud: 10}, 5, 70, 74},
		{"no usable window", WindowSummary{}, 11, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := windowScore(tc.win, tc.darkHours)
			if got < tc.wantMin || got > tc.wantMax {
				t.Errorf("score = %d, want %d..%d", got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

func TestSummarize(t *testing.T) {
	th := defaultThresholds()
	hours := []HourlyPoint{hp(20, 10, 0, 0, 10, 0, 0), hp(23, 30, 5, 10, 20, 0, 0)}
	cloud, rain := summarize(hours, th)
	if cloud.Avg != 20 {
		t.Errorf("avg = %d, want 20", cloud.Avg)
	}
	if cloud.Max != 30 || cloud.PeakAt != "23:00" {
		t.Errorf("max = %d at %q, want 30 at 23:00", cloud.Max, cloud.PeakAt)
	}
	if rain.AnyHour {
		t.Error("no rain expected")
	}
}
