package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/autoselect"
)

// TestLimitWindowFor pins the ONE window mapping shared by deadCredentialReset (the
// park's cross-check) and setLimitWait's gauge write (PRD #217 M1). The four
// seven-day spellings collapse to one window; `overage`, `unknown` and nil name no
// gauge column and map to windowNone.
//
// MUTATION THIS CATCHES: dropping any seven-day spelling from the mapping (it would
// fall through `default` to windowNone), or adding a gauge column for overage/unknown.
func TestLimitWindowFor(t *testing.T) {
	sp := func(s string) *string { return &s }
	for _, tc := range []struct {
		name string
		in   *string
		want limitWindow
	}{
		{"five_hour", sp("five_hour"), windowFiveHour},
		{"seven_day", sp("seven_day"), windowSevenDay},
		{"seven_day_opus", sp("seven_day_opus"), windowSevenDay},
		{"seven_day_sonnet", sp("seven_day_sonnet"), windowSevenDay},
		{"seven_day_overage_included", sp("seven_day_overage_included"), windowSevenDay},
		{"overage", sp("overage"), windowNone},
		{"unknown", sp("unknown"), windowNone},
		{"nil", nil, windowNone},
	} {
		if got := limitWindowFor(tc.in); got != tc.want {
			t.Fatalf("%s: limitWindowFor = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestLimitWindowForStaysInLockstepWithDeadCredentialReset asserts the property the
// mapping's own comment promises: the write (limitWindowFor) and the cross-check
// (deadCredentialReset) must agree on which window a rate_limit_type names, or the
// park would mark down a different window than it later reads back.
//
// For a MEASURED candidate carrying both reset columns, deadCredentialReset returns
// the mapped column's reset exactly when limitWindowFor names a real window, and
// false otherwise.
func TestLimitWindowForStaysInLockstepWithDeadCredentialReset(t *testing.T) {
	dead := uuid.New()
	five := parkNow.Add(3 * time.Hour)
	seven := parkNow.Add(6 * 24 * time.Hour)
	p := autoselect.Policy{MinHeadroom: 15, HeadroomTiePct: 5, MaxStaleness: 15 * time.Minute}
	mk := func() autoselect.Candidate {
		c := autoselectrowCandidate(dead, 90, parkNow.Add(-time.Minute)) // Measured (fresh)
		c.FiveResetsAt, c.SevenResetsAt = &five, &seven
		return c
	}

	sp := func(s string) *string { return &s }
	for _, ty := range []*string{
		sp("five_hour"), sp("seven_day"), sp("seven_day_opus"), sp("seven_day_sonnet"),
		sp("seven_day_overage_included"), sp("overage"), sp("unknown"), nil,
	} {
		win := limitWindowFor(ty)
		gauge, ok := deadCredentialReset([]autoselect.Candidate{mk()}, dead, p, ty, parkNow)

		switch win {
		case windowFiveHour:
			if !ok || !gauge.Equal(five) {
				t.Fatalf("%v maps to five-hour but the cross-check returned (%v, %v), want the "+
					"five-hour reset %v", ty, gauge, ok, five)
			}
		case windowSevenDay:
			if !ok || !gauge.Equal(seven) {
				t.Fatalf("%v maps to seven-day but the cross-check returned (%v, %v), want the "+
					"seven-day reset %v", ty, gauge, ok, seven)
			}
		default: // windowNone
			if ok {
				t.Fatalf("%v maps to no window but the cross-check produced a reset %v; the two "+
					"must agree that there is no gauge column here", ty, gauge)
			}
		}
	}
}
