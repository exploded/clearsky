package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// MetNo fetches hourly cloud + precipitation from the MET Norway Locationforecast API
// (the data behind yr.no). Free and keyless, but it REQUIRES a descriptive User-Agent
// with contact info — generic library agents are 403-banned.
// See https://api.met.no/weatherapi/locationforecast/2.0/documentation.
type MetNo struct {
	client    *http.Client
	userAgent string
}

func NewMetNo(userAgent string) *MetNo {
	return &MetNo{
		client:    &http.Client{Timeout: 15 * time.Second},
		userAgent: userAgent,
	}
}

func (m *MetNo) Name() string { return "met-no" }

// metNoResponse mirrors the subset of the /complete GeoJSON we use.
type metNoResponse struct {
	Properties struct {
		Timeseries []struct {
			Time time.Time `json:"time"`
			Data struct {
				Instant struct {
					Details struct {
						CloudAreaFraction       float64 `json:"cloud_area_fraction"`
						CloudAreaFractionLow    float64 `json:"cloud_area_fraction_low"`
						CloudAreaFractionMedium float64 `json:"cloud_area_fraction_medium"`
						CloudAreaFractionHigh   float64 `json:"cloud_area_fraction_high"`
					} `json:"details"`
				} `json:"instant"`
				Next1Hours struct {
					Details struct {
						PrecipitationAmount float64  `json:"precipitation_amount"`
						ProbabilityOfPrecip *float64 `json:"probability_of_precipitation"`
					} `json:"details"`
				} `json:"next_1_hours"`
			} `json:"data"`
		} `json:"timeseries"`
	} `json:"properties"`
}

func (m *MetNo) Fetch(ctx context.Context, lat, lon float64) (Forecast, error) {
	endpoint := fmt.Sprintf("https://api.met.no/weatherapi/locationforecast/2.0/complete?lat=%g&lon=%g", lat, lon)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Forecast{}, err
	}
	req.Header.Set("User-Agent", m.userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := m.client.Do(req)
	if err != nil {
		return Forecast{}, fmt.Errorf("met.no request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Forecast{}, fmt.Errorf("met.no status %d (check User-Agent)", resp.StatusCode)
	}

	var body metNoResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Forecast{}, fmt.Errorf("met.no decode: %w", err)
	}

	hours := make([]HourlyPoint, 0, len(body.Properties.Timeseries))
	for _, ts := range body.Properties.Timeseries {
		inst := ts.Data.Instant.Details
		next := ts.Data.Next1Hours.Details
		// probability_of_precipitation is Nordic-only; absent globally, so default 0
		// and let the rain veto lean on precipitation amount (mm).
		prob := 0
		if next.ProbabilityOfPrecip != nil {
			prob = int(math.Round(*next.ProbabilityOfPrecip))
		}
		hours = append(hours, HourlyPoint{
			At:            ts.Time,
			CloudTotal:    int(math.Round(inst.CloudAreaFraction)),
			CloudLow:      int(math.Round(inst.CloudAreaFractionLow)),
			CloudMid:      int(math.Round(inst.CloudAreaFractionMedium)),
			CloudHigh:     int(math.Round(inst.CloudAreaFractionHigh)),
			PrecipMm:      next.PrecipitationAmount,
			PrecipProbPct: prob,
			// MET does not provide surface visibility; leave 0 (unused by the gate).
		})
	}
	return Forecast{Source: m.Name(), Hours: hours}, nil
}
