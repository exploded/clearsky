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
//
// The same client serves every provider we use, because Open-Meteo will run a named
// numerical model on request (`models=`). Pinning the model is the whole point: the
// default "best_match" blend is opaque, and asking two APIs that both resolve to the
// same underlying model is not a second opinion (see NewOpenMeteoModel).
type OpenMeteo struct {
	client  *http.Client
	tz      string // IANA name passed to the API so returned times are site-local
	model   string // Open-Meteo `models=` value; "" leaves them to pick (best_match)
	name    string // provider name recorded against the night
	baseURL string // overridden in tests; empty means the real API
}

// NewOpenMeteo builds a client against Open-Meteo's own best_match blend. tz is the
// site timezone (e.g. "Australia/Melbourne"); Open-Meteo localises the returned hourly
// timestamps to it.
func NewOpenMeteo(tz string) *OpenMeteo {
	return &OpenMeteo{
		client: &http.Client{Timeout: 15 * time.Second},
		tz:     tz,
	}
}

// NewOpenMeteoModel pins the fetch to one named model (e.g. "ecmwf_ifs025",
// "gfs_seamless", "icon_seamless"), reported under the short name given.
//
// Why this exists: on 2026-08-03 the "agreement" gate passed a night that every other
// model called overcast, because its two sources — Open-Meteo best_match and yr.no —
// returned cloud within ~4% of each other on every hour. They were the same forecast
// twice, so taking the pessimistic merge of them was a no-op. Naming the model is what
// makes the sources actually independent.
func NewOpenMeteoModel(tz, model, name string) *OpenMeteo {
	return &OpenMeteo{
		client: &http.Client{Timeout: 15 * time.Second},
		tz:     tz,
		model:  model,
		name:   name,
	}
}

func (o *OpenMeteo) Name() string {
	if o.name != "" {
		return o.name
	}
	return "open-meteo"
}

// openMeteoResponse mirrors the subset of the JSON we request. Open-Meteo returns
// parallel arrays under "hourly" indexed by the "time" array.
//
// The numeric arrays are POINTER slices on purpose. Open-Meteo serves JSON null for a
// field a given model does not carry (BOM's ACCESS-G returns null for every field we
// ask for), and encoding/json unmarshals null into a plain int as a silent 0. For
// cloud cover that reads as "perfectly clear" — the worst possible failure mode for
// this app. Pointers let us tell "0% cloud" from "no data" and reject the latter.
type openMeteoResponse struct {
	Hourly struct {
		Time                     []string   `json:"time"`
		CloudCover               []*int     `json:"cloud_cover"`
		CloudCoverLow            []*int     `json:"cloud_cover_low"`
		CloudCoverMid            []*int     `json:"cloud_cover_mid"`
		CloudCoverHigh           []*int     `json:"cloud_cover_high"`
		Precipitation            []*float64 `json:"precipitation"`
		PrecipitationProbability []*int     `json:"precipitation_probability"`
		Visibility               []*float64 `json:"visibility"`
	} `json:"hourly"`
}

func (o *OpenMeteo) Fetch(ctx context.Context, lat, lon float64) (Forecast, error) {
	q := url.Values{}
	q.Set("latitude", fmt.Sprintf("%g", lat))
	q.Set("longitude", fmt.Sprintf("%g", lon))
	q.Set("hourly", "cloud_cover,cloud_cover_low,cloud_cover_mid,cloud_cover_high,precipitation,precipitation_probability,visibility")
	q.Set("timezone", o.tz)
	q.Set("forecast_days", "2") // tonight + tomorrow so past-midnight hours are covered
	if o.model != "" {
		q.Set("models", o.model)
	}
	base := o.baseURL
	if base == "" {
		base = "https://api.open-meteo.com/v1/forecast"
	}
	endpoint := base + "?" + q.Encode()

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
	cloudPoints := 0
	for i := 0; i < n; i++ {
		t, err := time.ParseInLocation("2006-01-02T15:04", h.Time[i], loc)
		if err != nil {
			return Forecast{}, fmt.Errorf("open-meteo time %q: %w", h.Time[i], err)
		}
		if at(h.CloudCover, i) != nil {
			cloudPoints++
		}
		hours = append(hours, HourlyPoint{
			At:            t,
			CloudTotal:    deref(at(h.CloudCover, i)),
			CloudLow:      deref(at(h.CloudCoverLow, i)),
			CloudMid:      deref(at(h.CloudCoverMid, i)),
			CloudHigh:     deref(at(h.CloudCoverHigh, i)),
			PrecipMm:      deref(at(h.Precipitation, i)),
			PrecipProbPct: deref(at(h.PrecipitationProbability, i)),
			VisibilityM:   int(deref(at(h.Visibility, i))),
		})
	}

	// A model that carries no cloud data at all must fail loudly rather than report a
	// flawless night. Open-Meteo happily accepts `models=bom_access_global` and answers
	// 200 with null in every slot.
	if n > 0 && cloudPoints == 0 {
		return Forecast{}, fmt.Errorf("open-meteo model %q returned no cloud data over %d hours", o.model, n)
	}
	return Forecast{Source: o.Name(), Hours: hours}, nil
}

// at safely indexes a parallel array that may be shorter than time[], returning nil
// for both "past the end" and "present but null".
func at[T any](s []*T, i int) *T {
	if i < len(s) {
		return s[i]
	}
	return nil
}

// deref turns a missing reading into the zero value. Safe for every field EXCEPT
// cloud cover, whose absence Fetch rejects outright before returning.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}
