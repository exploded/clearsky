package main

import (
	"time"

	"github.com/kixorz/suncalc"
)

// Darkness is the observing window for a night: astronomical dusk this evening to
// astronomical dawn the next morning. Kind records which twilight level was actually
// used — normally "astronomical", but it falls back gracefully if true astronomical
// darkness doesn't occur (e.g. high summer at high latitude).
type Darkness struct {
	Dusk time.Time
	Dawn time.Time
	Kind string
}

// MoonInfo is informational only — it never affects the GO/NO-GO decision.
type MoonInfo struct {
	IllumPct  float64    // 0..100 illuminated fraction, at the middle of the night
	PhaseName string     // e.g. "Waxing Crescent"
	Rise      *time.Time // moonrise within the darkness window, nil if none
	Set       *time.Time // moonset within the darkness window, nil if none
}

// darknessWindow computes tonight's dark window for the evening of date at the given
// site. date is interpreted in loc; only its calendar day matters.
func darknessWindow(date time.Time, lat, lon float64, loc *time.Location) Darkness {
	noon := time.Date(date.Year(), date.Month(), date.Day(), 12, 0, 0, 0, loc)
	tonight := suncalc.GetTimes(noon, lat, lon)
	tomorrow := suncalc.GetTimes(noon.AddDate(0, 0, 1), lat, lon)

	// Evening dusk (night begins) and next-morning dawn (night ends), preferring
	// astronomical darkness and degrading to nautical/civil/sunset if unavailable.
	dusk, kd := pick(tonight, suncalc.Night, suncalc.NauticalDusk, suncalc.Dusk, suncalc.Sunset)
	dawn, _ := pick(tomorrow, suncalc.NightEnd, suncalc.NauticalDawn, suncalc.Dawn, suncalc.Sunrise)

	return Darkness{Dusk: dusk.In(loc), Dawn: dawn.In(loc), Kind: kd}
}

// pick returns the first available (non-zero) time among the named events, plus a
// label for which twilight level it corresponds to.
func pick(times map[suncalc.DayTimeName]suncalc.DayTime, names ...suncalc.DayTimeName) (time.Time, string) {
	for _, n := range names {
		if dt, ok := times[n]; ok && !dt.Value.IsZero() {
			return dt.Value, twilightKind(n)
		}
	}
	return time.Time{}, "none"
}

func twilightKind(n suncalc.DayTimeName) string {
	switch n {
	case suncalc.Night, suncalc.NightEnd:
		return "astronomical"
	case suncalc.NauticalDusk, suncalc.NauticalDawn:
		return "nautical"
	case suncalc.Dusk, suncalc.Dawn:
		return "civil"
	default:
		return "sunset"
	}
}

// moonInfo computes illumination and the moonrise/moonset that fall within the dark
// window. Illumination is sampled at the midpoint of the window (~local midnight).
func moonInfo(d Darkness, lat, lon float64, loc *time.Location) MoonInfo {
	mid := d.Dusk.Add(d.Dawn.Sub(d.Dusk) / 2)
	illum := suncalc.GetMoonIllumination(mid)

	info := MoonInfo{
		IllumPct:  illum.Fraction * 100,
		PhaseName: phaseName(illum.Phase),
	}

	// Moon rise/set can land on either the evening date or the following morning, so
	// gather candidates from both days and keep those inside the dark window.
	for _, day := range []time.Time{d.Dusk, d.Dusk.AddDate(0, 0, 1)} {
		mt := suncalc.GetMoonTimes(day, lat, lon)
		if info.Rise == nil && withinWindow(mt.Rise, d) {
			r := mt.Rise.In(loc)
			info.Rise = &r
		}
		if info.Set == nil && withinWindow(mt.Set, d) {
			s := mt.Set.In(loc)
			info.Set = &s
		}
	}
	return info
}

func withinWindow(t time.Time, d Darkness) bool {
	if t.IsZero() {
		return false
	}
	return !t.Before(d.Dusk) && !t.After(d.Dawn)
}

// phaseName maps suncalc's phase (0=new, 0.25=first quarter, 0.5=full, 0.75=last
// quarter) to a human name including waxing/waning.
func phaseName(p float64) string {
	switch {
	case p < 0.03 || p > 0.97:
		return "New Moon"
	case p < 0.22:
		return "Waxing Crescent"
	case p < 0.28:
		return "First Quarter"
	case p < 0.47:
		return "Waxing Gibbous"
	case p < 0.53:
		return "Full Moon"
	case p < 0.72:
		return "Waning Gibbous"
	case p < 0.78:
		return "Last Quarter"
	default:
		return "Waning Crescent"
	}
}
