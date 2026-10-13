package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// PRD #1733 (R2): `uzi run set-token` on a queued or parked run is ONE transaction (override
// write, early promote, provider-park streak clear) under the owner's secret mutation lock and
// the run's row lock. These execute it against a REAL Postgres; skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres. They reuse seedRunInStatus.

type ppStreak struct {
	status       string
	overrideMode string
	count        int32
	started      time.Time
	startedValid bool
}

func ppRead(t *testing.T, env codexTestEnv, id uuid.UUID) ppStreak {
	t.Helper()
	r := mustRun(t, env, id)
	return ppStreak{
		status:       r.Status,
		overrideMode: r.CredentialOverrideMode.String,
		count:        r.ProviderParkCount,
		started:      r.ProviderParkStreakStartedAt.Time,
		startedValid: r.ProviderParkStreakStartedAt.Valid,
	}
}

func ppSeedStreak(env codexTestEnv, id uuid.UUID) {
	env.exec(`UPDATE runs SET provider_park_count = 4, provider_park_streak_started_at = now() - interval '3 hours' WHERE id = $1`, id)
}

// TestSetRunCredentialClearsProviderStreakLiveDB: a successful switch on every writable state
// clears the open streak in the same transaction as the promote; a provider hold (paused) clears
// it through ResumePausedRun.
//
// MUTATION: drop the ResetProviderParkStreakForOwner call (queued/limit_wait/pool_wait) or the
// streak columns from PromoteRecoveryWaitRunNow / ResumePausedRun and the matching case fails.
func TestSetRunCredentialClearsProviderStreakLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	now := time.Now().UTC()

	for i, status := range []string{"queued", "limit_wait", "pool_wait", "recovery_wait", "paused"} {
		t.Run(status, func(t *testing.T) {
			id := seedRunInStatus(t, env, o, int64(4700+i), status, now)
			if status == "paused" {
				env.exec(`UPDATE runs SET hold_reason = 'provider_unavailable' WHERE id = $1`, id)
			}
			ppSeedStreak(env, id)
			if _, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok); err != nil {
				t.Fatalf("SetRunCredential: %v", err)
			}
			got := ppRead(t, env, id)
			if got.status != "queued" || got.overrideMode != CredentialOverrideModePinned {
				t.Fatalf("status=%q override=%q, want queued/pinned", got.status, got.overrideMode)
			}
			if got.count != 0 || got.startedValid {
				t.Fatalf("streak after the switch: count=%d start=%v valid=%v, want cleared", got.count, got.started, got.startedValid)
			}
		})
	}
}

// TestSetRunCredentialRefusedChangesNothingLiveDB: a switch refused INSIDE the transaction
// (a pin disabled after validation) rolls back the override and leaves the status and the streak
// untouched, for every state the transaction serves.
func TestSetRunCredentialRefusedChangesNothingLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	now := time.Now().UTC()

	for i, status := range []string{"queued", "limit_wait", "pool_wait", "recovery_wait"} {
		t.Run(status, func(t *testing.T) {
			svc := New(env.q, env.box, testParams())
			svc.SetTxBeginner(env.pool)
			id := seedRunInStatus(t, env, o, int64(4720+i), status, now)
			ppSeedStreak(env, id)
			before := ppRead(t, env, id)
			// The pin is valid when the verb validates it and disabled by the time its transaction
			// re-checks it under the lock.
			svc.beforeCredentialSwitchTx = func() {
				env.exec(`UPDATE user_secrets SET disabled_at = now() WHERE id = $1`, o.altTok)
			}
			defer env.exec(`UPDATE user_secrets SET disabled_at = NULL WHERE id = $1`, o.altTok)

			_, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok)
			if !errors.Is(err, ErrCredentialDisabled) {
				t.Fatalf("SetRunCredential err = %v, want ErrCredentialDisabled", err)
			}
			if after := ppRead(t, env, id); after != before {
				t.Fatalf("a refused switch changed the run: %+v -> %+v", before, after)
			}
		})
	}
}

// TestSetRunCredentialRacedKeepsNewerStreakLiveDB: when the run moves between the verb's read and
// its row lock (a claim, or a provider re-park that lands it back in the SAME status with a newer
// streak) the switch is ErrCredentialSwitchRaced, everything is rolled back, and the newer streak
// survives.
//
// MUTATION: drop the status_since / claim_generation comparison from setCredentialAndPromote and
// the re-park case erases the newer streak; drop the status comparison and the claim case does.
func TestSetRunCredentialRacedKeepsNewerStreakLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	now := time.Now().UTC()

	moves := map[string]string{
		"claimed in between":     `UPDATE runs SET status = 'running', claim_generation = claim_generation + 1 WHERE id = $1`,
		"re-parked in between":   `UPDATE runs SET status_since = status_since + interval '1 minute', provider_park_count = 9, provider_park_streak_started_at = now() WHERE id = $1`,
		"generation moved on":    `UPDATE runs SET claim_generation = claim_generation + 1, provider_park_count = 9, provider_park_streak_started_at = now() WHERE id = $1`,
		"status moved to queued": `UPDATE runs SET status = 'queued', status_since = now(), provider_park_count = 9, provider_park_streak_started_at = now() WHERE id = $1`,
	}
	i := 0
	for _, status := range []string{"queued", "limit_wait", "pool_wait", "recovery_wait"} {
		for name, move := range moves {
			if status == "queued" && name == "status moved to queued" {
				continue // no change to observe
			}
			i++
			t.Run(status+"/"+name, func(t *testing.T) {
				svc := New(env.q, env.box, testParams())
				svc.SetTxBeginner(env.pool)
				id := seedRunInStatus(t, env, o, int64(4740+i), status, now)
				ppSeedStreak(env, id)
				svc.beforeCredentialSwitchTx = func() { env.exec(move, id) }

				_, err := svc.SetRunCredential(env.ctx, o.userID, id, CredentialOverrideModePinned, &o.altTok)
				if !errors.Is(err, ErrCredentialSwitchRaced) {
					t.Fatalf("SetRunCredential err = %v, want ErrCredentialSwitchRaced", err)
				}
				got := ppRead(t, env, id)
				if got.overrideMode != "" {
					t.Fatalf("the raced switch left an override behind: %q", got.overrideMode)
				}
				if name == "claimed in between" {
					if got.count != 4 {
						t.Fatalf("streak count = %d, want 4 (untouched)", got.count)
					}
					return
				}
				if got.count != 9 || !got.startedValid {
					t.Fatalf("the newer streak was erased: count=%d valid=%v, want 9/true", got.count, got.startedValid)
				}
			})
		}
	}
}
