package main

import (
	"testing"
	"time"
)

func TestNextFireAt(t *testing.T) {
	loc := mustMelbourne(t)

	// Before the fire time on the same day -> today at 18:00.
	now := time.Date(2026, 7, 1, 14, 0, 0, 0, loc)
	got := nextFireAt(now, 18, 0, loc)
	want := time.Date(2026, 7, 1, 18, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("before fire: got %v, want %v", got, want)
	}

	// After the fire time -> tomorrow at 18:00.
	now = time.Date(2026, 7, 1, 19, 30, 0, 0, loc)
	got = nextFireAt(now, 18, 0, loc)
	want = time.Date(2026, 7, 2, 18, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Errorf("after fire: got %v, want %v", got, want)
	}

	// Exactly at the fire time -> next day (strictly after now).
	now = time.Date(2026, 7, 1, 18, 0, 0, 0, loc)
	got = nextFireAt(now, 18, 0, loc)
	if !got.After(now) {
		t.Errorf("at fire: got %v not after now %v", got, now)
	}
}
