package main

import "testing"

func TestStateGlyphWordAccountCooldown(t *testing.T) {
	for _, tc := range []struct {
		status, health     string
		planning, revising bool
		glyph, word        string
	}{
		{"claimed", "waiting_account_cooldown", false, false, "~", "account cooldown"},
		{"running", "waiting_account_cooldown", false, false, "~", "account cooldown"},
		{"running", "waiting_account_cooldown", true, false, "~", "account cooldown"},
		{"awaiting_approval", "waiting_account_cooldown", false, true, "~", "account cooldown"},
		{"awaiting_approval", "waiting_account_cooldown", false, false, "~", "account cooldown"},
		{"completed", "waiting_account_cooldown", false, false, "✓", "done"},
		{"failed", "waiting_account_cooldown", false, false, "✗", "failed"},
		{"queued", "waiting_account_cooldown", false, false, "·", "queued"},
		{"running", "slow", true, false, "▲", "near timeout"},
		{"running", "looping", false, false, "▲", "looping"},
		{"running", "ok", true, false, "○", "planning"},
	} {
		t.Run(tc.status+"/"+tc.health+"/"+tc.word, func(t *testing.T) {
			g, w := stateGlyphWord(tc.status, tc.health, tc.planning, tc.revising, "", "")
			if g != tc.glyph || w != tc.word {
				t.Fatalf("got %q %q, want %q %q", g, w, tc.glyph, tc.word)
			}
		})
	}
}
