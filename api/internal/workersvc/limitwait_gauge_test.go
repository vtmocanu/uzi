package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #217 M1 — setLimitWait's park branch marks the dead credential's exhausted
// window down to 100% in the gauge, via limitWindowFor(d.RateLimitType). These drive
// the whole SetState arm (a park that LANDS) and assert which Mark* query fired, for
// the run's OWN anthropic_secret_id, and that the other window's query did not.

// runningRunWithCredential is runningRun plus a recorded credential, which is what
// makes setLimitWait fetch candidates AND reach the gauge-write branch (both are
// gated on run.AnthropicSecretID.Valid).
func runningRunWithCredential(dead uuid.UUID) store.Run {
	run := runningRun(true)
	run.AnthropicSecretID = pgtype.UUID{Bytes: dead, Valid: true}
	return run
}

// TestSetLimitWaitParkMarksTheDeadCredentialsWindow: a five_hour park marks the
// five-hour window and not the seven-day one; each seven-day spelling marks the
// seven-day window and not the five-hour one; overage and unknown mark NEITHER (the
// D2 case M1 structurally cannot cover — those types name no gauge column).
//
// The marked id is asserted to be the run's own anthropic_secret_id (dead), because
// R1 turns on that being the server's record of what the claim opened, never anything
// the worker names.
//
// MUTATION THIS CATCHES: writing the wrong window (swapping the two Mark calls), or
// dropping the `overage`/`unknown` no-op so a nonexistent column gets marked.
func TestSetLimitWaitParkMarksTheDeadCredentialsWindow(t *testing.T) {
	for _, tc := range []struct {
		rateLimitType       string
		wantFive, wantSeven bool
	}{
		{"five_hour", true, false},
		{"seven_day", false, true},
		{"seven_day_opus", false, true},
		{"seven_day_sonnet", false, true},
		{"seven_day_overage_included", false, true},
		{"overage", false, false},
		{"unknown", false, false},
	} {
		t.Run(tc.rateLimitType, func(t *testing.T) {
			dead := uuid.New()
			run := runningRunWithCredential(dead)
			fs, svc, wkr := limitParkFixture(t, run)
			fs.setLimitWaitRows = 1

			ty := tc.rateLimitType
			if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
				State: "limit_wait", LimitResetsAt: ms(2 * time.Hour), RateLimitType: &ty,
			}); err != nil {
				t.Fatalf("SetState: %v", err)
			}
			if fs.setLimitWait == nil {
				t.Fatalf("the park never landed, so the gauge branch was never reached")
			}

			if tc.wantFive {
				if len(fs.markedFiveHour) != 1 || fs.markedFiveHour[0] != dead {
					t.Fatalf("five-hour marks = %v, want exactly [%v] — the run's OWN "+
						"anthropic_secret_id", fs.markedFiveHour, dead)
				}
			} else if len(fs.markedFiveHour) != 0 {
				t.Fatalf("a %s park marked the five-hour window (%v); that window is not the one "+
					"this type names", tc.rateLimitType, fs.markedFiveHour)
			}

			if tc.wantSeven {
				if len(fs.markedSevenDay) != 1 || fs.markedSevenDay[0] != dead {
					t.Fatalf("seven-day marks = %v, want exactly [%v]", fs.markedSevenDay, dead)
				}
			} else if len(fs.markedSevenDay) != 0 {
				t.Fatalf("a %s park marked the seven-day window (%v); overage/unknown name no gauge "+
					"column at all, and a five_hour park must not touch it", tc.rateLimitType, fs.markedSevenDay)
			}
		})
	}
}

// TestSetLimitWaitParkWithoutACredentialWritesNoGauge: a run that never recorded a
// credential (AnthropicSecretID invalid) has nothing to mark down, so neither Mark
// query fires — the whole gauge branch is gated on run.AnthropicSecretID.Valid,
// exactly like the candidate fetch.
func TestSetLimitWaitParkWithoutACredentialWritesNoGauge(t *testing.T) {
	run := runningRun(true) // AnthropicSecretID left invalid
	fs, svc, wkr := limitParkFixture(t, run)
	fs.setLimitWaitRows = 1

	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "limit_wait", LimitResetsAt: ms(2 * time.Hour), RateLimitType: strPtr("five_hour"),
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setLimitWait == nil {
		t.Fatalf("the park never landed")
	}
	if len(fs.markedFiveHour) != 0 || len(fs.markedSevenDay) != 0 {
		t.Fatalf("marked a gauge window (five=%v seven=%v) for a run with no recorded credential; "+
			"there is no row to mark and nothing to name it", fs.markedFiveHour, fs.markedSevenDay)
	}
}

// TestSetLimitWaitFailureWritesNoGauge: the gauge write lives in the PARK branch, so
// a report that FAILS the run instead of parking it (here: opted out) must not touch
// the gauge — the dead credential is not being parked on, it is being abandoned.
func TestSetLimitWaitFailureWritesNoGauge(t *testing.T) {
	run := runningRunWithCredential(uuid.New())
	run.WaitOnLimit = false // opt-out ⇒ the report is coerced to a failure, not a park
	fs, svc, wkr := limitParkFixture(t, run)

	if _, _, err := svc.SetState(context.Background(), wkr, run.ID, StateRequest{
		State: "limit_wait", LimitResetsAt: ms(2 * time.Hour), RateLimitType: strPtr("five_hour"),
	}); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if fs.setFailed == nil {
		t.Fatalf("the opt-out report was not failed; the fixture is wrong")
	}
	if len(fs.markedFiveHour) != 0 || len(fs.markedSevenDay) != 0 {
		t.Fatalf("a FAILED report still wrote the gauge (five=%v seven=%v); the write belongs only "+
			"to a landed park", fs.markedFiveHour, fs.markedSevenDay)
	}
}
