package slacksvc

import (
	"strings"
	"testing"
)

// A run the weekly usage reserve parked (PRD #3522) reads as a system park in both the root and the
// thread notice: it names the reserve and how it ends, never the neutral owner-pause wording, and an
// ordinary pause keeps that wording.
func TestWeeklyReservePauseNotice(t *testing.T) {
	rc := providerHoldRun(t)
	rc.HoldReason = txt("weekly_reserve")

	blocks, fallback := rootBlocks(rc, "")
	_, rootSection := blockSummary(blocks)
	if !strings.Contains(rootSection, "Paused · weekly usage reserve") || !strings.Contains(fallback, "Paused · weekly usage reserve") {
		t.Fatalf("root = %q / %q, want the reserve cause named", rootSection, fallback)
	}

	thread, tfallback, ok := renderThreadBlocks(rc, "")
	if !ok {
		t.Fatal("a reserve park must render a thread event")
	}
	_, threadSection := blockSummary(thread)
	copy := threadSection + tfallback + contextText(thread)
	for _, want := range []string{"weekly usage reserve", "usage drops below", "uzi run resume " + rc.ID.String()} {
		if !strings.Contains(copy, want) {
			t.Errorf("thread notice lacks %q: %s", want, copy)
		}
	}
	if strings.Contains(threadSection, "‖ *Paused*") {
		t.Errorf("a reserve park used the neutral owner-pause header: %q", threadSection)
	}

	// Control: an ordinary pause (no hold) keeps the neutral wording.
	rc.HoldReason = txt("")
	rc.HoldReason.Valid = false
	ordinary, _, _ := renderThreadBlocks(rc, "")
	_, section := blockSummary(ordinary)
	if !strings.Contains(section, "‖ *Paused*") || strings.Contains(section, "weekly usage reserve") {
		t.Errorf("ordinary pause section = %q, want the neutral header", section)
	}
}
