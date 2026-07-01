package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// OpenMeteo fetches hourly cloud + precipitation forecasts from the free Open-Meteo
// API (no API key). See https://open-meteo.com/en/docs.
type OpenMeteo struct {
	client *http.Client
	tz     string // IANA name passed to the API so returned times are site-local
}

// NewOpenMeteo builds a client. tz is the site timezone (e.g. "Australia/Melbourne");
// Open-Meteo localises the returned hourly timestamps to it.
func NewOpenMeteo(tz string) *OpenMeteo {
	return &OpenMeteo{
		client: &http.Client{Timeout: 15 * time.Second},
		tz:     tz,
	}
}

func (o *OpenMeteo) Name() string { return "open-meteo" }

// openMeteoResponse mirrors the subset of the JSON we request. Open-Meteo returns
// parallel arrays under "hourly" indexed by the "time" array.
type openMeteoResponse struct {
	Hourly struct {
		Time                     []string  `json:"time"`
		CloudCover               []int     `json:"cloud_cover"`
		CloudCoverLow            []int     `json:"cloud_cover_low"`
		CloudCoverMid            []int     `json:"cloud_cover_mid"`
		CloudCoverHigh           []int     `json:"cloud_cover_high"`
		Precipitation            []float64 `json:"precipitation"`
		PrecipitationProbability []int     `json:"precipitation_probability"`
		Visibility               []float64 `json:"visibility"`
	} `json:"hourly"`
}

func (o *OpenMeteo) Fetch(ctx context.Context, lat, lon float64) (Forecast, error) {
	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%g", lat))
	q.Set("longitude", fmt.Sprintf("%g", lon))
	q.Set("hourly", "cloud_cover,cloud_cover_low,cloud_cover_mid,cloud_cover_high,precipitation,precipitation_probability,visibility")
	q.Set("timezone", o.tz)
	q.Set("forecast_days", "2") // tonight + tomorrow so past-midnight hours are covered
	endpoint := "https://api.open-meteo.com/v1/forecast?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Forecast{}, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return Forecast{}, fmt.Errorf("open-meteo request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Forecast{}, fmt.Errorf("open-meteo status %d", resp.StatusCode)
	}

	var body openMeteoResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return Forecast{}, fmt.Errorf("open-meteo decode: %w", err)
	}

	// Parse the site-local timestamps against the same location so the returned
	// times are wall-clock-correct for the observing site.
	loc, err := time.LoadLocation(o.tz)
	if err != nil {
		loc = time.UTC
	}
	h := body.Hourly
	n := len(h.Time)
	hours := make([]HourlyPoint, 0, n)
	for i := 0; i < n; i++ {
		t, err := time.ParseInLocation("2006-01-02T15:04", h.Time[i], loc)
		if err != nil {
			return Forecast{}, fmt.Errorf("open-meteo time %q: %w", h.Time[i], err)
		}
		hours = append(hours, HourlyPoint{
			At:            t,
			CloudTotal:    at(h.CloudCover, i),
			CloudLow:      at(h.CloudCoverLow, i),
			CloudMid:      at(h.CloudCoverMid, i),
			CloudHigh:     at(h.CloudCoverHigh, i),
			PrecipMm:      atf(h.Precipitation, i),
			PrecipProbPct: at(h.PrecipitationProbability, i),
			VisibilityM:   int(atf(h.Visibility, i)),
		})
	}
	return Forecast{Source: o.Name(), Hours: hours}, nil
}

// at / atf safely index parallel arrays that may be shorter than time[].
func at(s []int, i int) int {
	if i < len(s) {
		return s[i]
	}
	return 0
}

func atf(s []float64, i int) float64 {
	if i < len(s) {
		return s[i]
	}
	return 0
}
