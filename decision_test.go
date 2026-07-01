package main

import (
	"testing"
	"time"
)

func defaultThresholds() Thresholds {
	return Thresholds{
		RainMmVetoHour:  0.0,
		RainProbVetoPct: 20,
		RainMmTotalVeto: 0.2,
		CloudAvgMaxPct:  25,
		CloudMaxPct:     40,
		CloudLowMaxPct:  15,
		CloudMidMaxPct:  25,
		VisibilityMinM:  20000,
	}
}

// hp builds an hourly point with total cloud split across layers for brevity.
func hp(hour, cloud, low, mid, high, prob int, mm float64) HourlyPoint {
	loc, _ := time.LoadLocation("Australia/Melbourne")
	return HourlyPoint{
		At:            time.Date(2026, 7, 1, hour, 0, 0, 0, loc),
		CloudTotal:    cloud,
		CloudLow:      low,
		CloudMid:      mid,
		CloudHigh:     high,
		PrecipProbPct: prob,
		PrecipMm:      mm,
		VisibilityM:   24000,
	}
}

func TestEvaluate(t *testing.T) {
	th := defaultThresholds()
	tests := []struct {
		name   string
		hours  []HourlyPoint
		wantGO bool
	}{
		{
			name:   "clear night -> GO",
			hours:  []HourlyPoint{hp(20, 5, 0, 0, 5, 0, 0), hp(23, 15, 0, 5, 10, 5, 0), hp(2, 10, 0, 0, 10, 0, 0)},
			wantGO: true,
		},
		{
			name:   "any measurable rain -> NO-GO",
			hours:  []HourlyPoint{hp(20, 5, 0, 0, 5, 0, 0), hp(23, 5, 0, 0, 5, 10, 0.3)},
			wantGO: false,
		},
		{
			name:   "high rain probability -> NO-GO",
			hours:  []HourlyPoint{hp(20, 5, 0, 0, 5, 40, 0)},
			wantGO: false,
		},
		{
			name:   "total precip over window veto -> NO-GO",
			hours:  []HourlyPoint{hp(20, 5, 0, 0, 5, 0, 0.15), hp(23, 5, 0, 0, 5, 0, 0.15)},
			wantGO: false,
		},
		{
			name:   "high average cloud -> NO-GO",
			hours:  []HourlyPoint{hp(20, 60, 0, 20, 40, 0, 0), hp(23, 70, 0, 20, 50, 0, 0)},
			wantGO: false,
		},
		{
			name:   "peak cloud too high despite low average -> NO-GO",
			hours:  []HourlyPoint{hp(20, 0, 0, 0, 0, 0, 0), hp(23, 0, 0, 0, 0, 0, 0), hp(2, 80, 0, 0, 80, 0, 0)},
			wantGO: false,
		},
		{
			name:   "low cloud breaches tight cap even if total ok -> NO-GO",
			hours:  []HourlyPoint{hp(20, 30, 25, 5, 0, 0, 0)},
			wantGO: false,
		},
		{
			name:   "thin high cloud only -> GO",
			hours:  []HourlyPoint{hp(20, 20, 0, 0, 20, 0, 0), hp(23, 30, 5, 5, 30, 0, 0)},
			wantGO: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.hours, th)
			if got.GO != tc.wantGO {
				t.Errorf("GO=%v want %v (reason: %s)", got.GO, tc.wantGO, got.Reason)
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

func TestEvaluateEmpty(t *testing.T) {
	got := Evaluate(nil, defaultThresholds())
	if got.GO {
		t.Error("empty window must be NO-GO")
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
