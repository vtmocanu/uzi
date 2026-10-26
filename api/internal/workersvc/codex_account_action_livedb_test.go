package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// seedActionRecovery puts a properly sealed recovery login at the account's current generation
// (the survivor pass's promotion candidate). The survivor pass scans every account in the shared
// database, so the slot and the reauth flag are cleared when the test ends, leaving no candidate
// behind for another test's sweep.
func seedActionRecovery(t *testing.T, fx *codexClaimFix) {
	t.Helper()
	fx.env.exec(`UPDATE codex_provider_account SET recovery_sealed = $2, recovery_sealed_with = 'master',
		recovery_generation = generation WHERE id = $1`, fx.accountID, sealedLogin(t, fx.env, codexToken("recovery-access")))
	t.Cleanup(func() {
		fx.env.exec(`UPDATE codex_provider_account SET recovery_sealed = NULL, recovery_sealed_with = NULL,
			recovery_generation = NULL, reauth_required = false, reauth_generation = NULL,
			reauth_credential_revision = NULL, reauth_reason = NULL WHERE id = $1`, fx.accountID)
	})
}

// codex_account_action_livedb_test.go pins PRD #1590 D6's derived codex_account_action against a
// real Postgres: CodexAccountActionsForRuns reads real held rows (the M2 park's hold, then real
// alias and account transitions) through ListCodexAccountActionInputs, and a run that is not held
// gets no action. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh.

// TestCodexAccountActionsForRunsLiveDB: one leg per real state, each derived from the rows the
// promoter itself would decide on, plus a queued (not held) run in the same batch that gets no
// action and a leg proving the batch returns nothing once the hold ends.
func TestCodexAccountActionsForRunsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, fx *codexClaimFix)
		want   string
	}{
		{"quarantined, no recovery material, no lease", func(*testing.T, *codexClaimFix) {},
			CodexAccountActionReloginRequired},
		{"quarantined, recovery material at the current generation", func(t *testing.T, fx *codexClaimFix) {
			seedActionRecovery(t, fx)
		}, CodexAccountActionReconciling},
		// The reauth flag outranks recovery material: survivor promotion can defer
		// indefinitely (nil codexRefresh, locked vault, ErrIdentityIncomplete, a non-2xx
		// DiscoverIdentity), while a same-alias re-login is always a valid way out.
		{"quarantined, recovery material but reauth required", func(t *testing.T, fx *codexClaimFix) {
			seedActionRecovery(t, fx)
			fx.setAccount(t, `reauth_required = true, reauth_generation = generation,
				reauth_credential_revision = credential_revision`)
		}, CodexAccountActionReloginRequired},
		{"quarantined, reauth required, no recovery material", func(t *testing.T, fx *codexClaimFix) {
			fx.setAccount(t, `reauth_required = true, reauth_generation = generation,
				reauth_credential_revision = credential_revision`)
			t.Cleanup(func() {
				fx.setAccount(t, `reauth_required = false, reauth_generation = NULL,
					reauth_credential_revision = NULL, reauth_reason = NULL`)
			})
		}, CodexAccountActionReloginRequired},
		{"quarantined, live lease", func(t *testing.T, fx *codexClaimFix) {
			fx.setAccount(t, `lease_deadline = now() + interval '1 hour'`)
		}, CodexAccountActionReconciling},
		// A same-identity re-login whose re-admission is held back by an in_progress account
		// with an EXPIRED lease (ReadmitRunCodexBinding's coord_state fence): the survivor pass
		// reaps the lease into quarantine on its next tick, so this is reconciling, not an owner
		// action. (Without material ahead an in_progress account passes the release predicate
		// and the run resumes, whatever its lease.) Restored on cleanup so no other test's
		// survivor sweep finds a reap candidate.
		{"re-login ahead, in_progress with an expired lease", func(t *testing.T, fx *codexClaimFix) {
			sameIdentityRelogin(t, fx.env, fx, codexToken("access-relogin"))
			fx.setAccount(t, "coord_state = 'in_progress', coord_operation_id = gen_random_uuid(), lease_deadline = now() - interval '1 minute'")
			t.Cleanup(func() {
				fx.setAccount(t, "coord_state = 'idle', coord_operation_id = NULL, lease_deadline = NULL")
			})
		}, CodexAccountActionReconciling},
		{"re-login being verified (alias staging)", func(t *testing.T, fx *codexClaimFix) {
			startRelogin(t, fx.env, fx, "staging")
		}, CodexAccountActionVerifyingLogin},
		{"re-login failed (alias failed)", func(t *testing.T, fx *codexClaimFix) {
			startRelogin(t, fx.env, fx, "failed")
		}, CodexAccountActionReloginRequired},
		{"alias deleted", func(t *testing.T, fx *codexClaimFix) {
			fx.env.exec(`DELETE FROM user_secrets WHERE id = $1`, fx.aliasID)
		}, CodexAccountActionReloginRequired},
		{"account recovered (predicate passes)", func(t *testing.T, fx *codexClaimFix) {
			relogin(t, fx.env, fx.env.q, fx, codexToken("access-new"))
		}, CodexAccountActionResuming},
		{"same-identity re-login completed (re-admitted next tick)", func(t *testing.T, fx *codexClaimFix) {
			sameIdentityRelogin(t, fx.env, fx, codexToken("access-relogin"))
		}, CodexAccountActionResuming},
		{"fallback: pending terminal credential revision change", func(t *testing.T, fx *codexClaimFix) {
			fx.setAccount(t, "coord_state = 'idle', credential_revision = credential_revision + 1")
		}, CodexAccountActionReloginRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, svc := heldCodexFix(t, env)
			queued := newCodexClaimFix(t, env, false) // a Codex run that is not held
			tc.mutate(t, fx)
			if r := mustRun(t, env, fx.runID); r.Status != "recovery_wait" {
				t.Fatalf("seed: status=%s, want the hold kept through the mutation", r.Status)
			}

			got, err := svc.CodexAccountActionsForRuns(env.ctx, []uuid.UUID{fx.runID, queued.runID})
			if err != nil {
				t.Fatalf("CodexAccountActionsForRuns: %v", err)
			}
			if got[fx.runID].Action != tc.want {
				t.Fatalf("held run action = %q, want %q", got[fx.runID].Action, tc.want)
			}
			if a, ok := got[queued.runID]; ok {
				t.Fatalf("queued run hold = %+v, want none", a)
			}
			if len(got) != 1 {
				t.Fatalf("actions = %v, want exactly the held run", got)
			}

			// "resuming" is a claim about the promoter's next decision, so check it against
			// the promoter itself: the very next pass must promote this run out of the hold.
			if tc.want == CodexAccountActionResuming {
				if n, err := promoteOnly(t, svc, fx.runID); err != nil || n != 1 {
					t.Fatalf("resuming but promote = (%d, %v), want (1, nil)", n, err)
				}
				if r := mustRun(t, env, fx.runID); r.Status == "recovery_wait" {
					t.Fatalf("resuming but the run is still held after the promoter ran")
				}
			} else if n, err := promoteOnly(t, svc, fx.runID); err != nil || n != 0 {
				// The converse: any other action must not be a run the promoter resumes.
				t.Fatalf("action %q but promote = (%d, %v), want (0, nil)", tc.want, n, err)
			}
		})
	}

	// The label is the run's OWN snapshot (runs.codex_secret_label), not the alias row's
	// current label: renaming the alias after the park must not change what the hold shows.
	t.Run("label is the run's own snapshot", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		fx, svc := heldCodexFix(t, env)
		snap := mustRun(t, env, fx.runID).CodexSecretLabel
		if !snap.Valid || snap.String == "" {
			t.Fatalf("seed: codex_secret_label = %+v, want a non-empty snapshot", snap)
		}
		env.exec(`UPDATE user_secrets SET label = 'renamed-alias' WHERE id = $1`, fx.aliasID)
		got, err := svc.CodexAccountActionsForRuns(env.ctx, []uuid.UUID{fx.runID})
		if err != nil {
			t.Fatalf("CodexAccountActionsForRuns: %v", err)
		}
		if l := got[fx.runID].Label; l == nil || *l != snap.String {
			t.Fatalf("held run label = %v, want the run's snapshot %q", l, snap.String)
		}
	})

	t.Run("hold ended", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		fx, svc := heldCodexFix(t, env)
		relogin(t, env, env.q, fx, codexToken("access-new"))
		if n, err := promoteOnly(t, svc, fx.runID); err != nil || n != 1 {
			t.Fatalf("promote = (%d, %v), want (1, nil)", n, err)
		}
		got, err := svc.CodexAccountActionsForRuns(env.ctx, []uuid.UUID{fx.runID})
		if err != nil || len(got) != 0 {
			t.Fatalf("after promotion: actions = %v, err = %v, want none", got, err)
		}
	})
}

// TestCodexAccountActionRetryPendingLiveDB exercises the display query and service
// over receipt states. Intent age is fixed in SQL; consumed retry age is independent.
func TestCodexAccountActionRetryPendingLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		intentSet, accountSet string
		disabled, recovery    bool
		pending               bool
		want                  string
	}{
		{name: "eligible", pending: true, want: CodexAccountActionReconciling},
		{name: "consumed disabled alias old intent", intentSet: "created_at=now()-interval '1 hour', reuse_attempted_at=now()", disabled: true, pending: true, want: CodexAccountActionReconciling},
		{name: "expired unconsumed window", intentSet: "created_at=now()-interval '31 minutes'", want: CodexAccountActionReloginRequired},
		{name: "consumed crash bound expired", intentSet: "reuse_attempted_at=now()-interval '53 seconds'", want: CodexAccountActionReloginRequired},
		{name: "consumed inside bound", intentSet: "reuse_attempted_at=now()-interval '30 seconds'", pending: true, want: CodexAccountActionReconciling},
		{name: "wrong operation", accountSet: "coord_operation_id=gen_random_uuid()", want: CodexAccountActionReloginRequired},
		{name: "moved generation", accountSet: "generation=generation+1", want: CodexAccountActionReloginRequired},
		{name: "no receipt", intentSet: "provider_error_at=NULL", want: CodexAccountActionReloginRequired},
		{name: "unconsumed disabled alias", disabled: true, want: CodexAccountActionReloginRequired},
		{name: "failed retry", intentSet: "reuse_attempted_at=now(), state='unrecoverable'", want: CodexAccountActionReloginRequired},
		{name: "reauth precedence", accountSet: "reauth_required=true, reauth_generation=generation, reauth_credential_revision=credential_revision", want: CodexAccountActionReloginRequired},
		{name: "recovery control", recovery: true, want: CodexAccountActionReconciling},
		{name: "consumed recovery control", intentSet: "reuse_attempted_at=now()", recovery: true, want: CodexAccountActionReconciling},
		{name: "committed control", accountSet: "coord_state='committed'", want: CodexAccountActionResuming},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx, svc := heldCodexFix(t, env)
			op := uuid.New()
			env.exec("UPDATE codex_provider_account SET coord_operation_id=$2, lease_deadline=NULL WHERE id=$1", fx.accountID, op)
			env.exec(`INSERT INTO codex_refresh_intent
                (operation_id,user_id,provider_account_id,from_generation,state,created_at,provider_error_at)
                SELECT $1,user_id,id,generation,'rotating',now()-interval '1 minute',now()
                FROM codex_provider_account WHERE id=$2`, op, fx.accountID)
			// Never leave an unresolved intent for another test's survivor sweep.
			t.Cleanup(func() { env.exec("DELETE FROM codex_refresh_intent WHERE operation_id=$1", op) })
			if tc.intentSet != "" {
				env.exec("UPDATE codex_refresh_intent SET "+tc.intentSet+" WHERE operation_id=$1", op)
			}
			if tc.accountSet != "" {
				fx.setAccount(t, tc.accountSet)
			}
			if tc.disabled {
				env.exec("UPDATE user_secrets SET disabled_at=now() WHERE id=$1", fx.aliasID)
			}
			if tc.recovery {
				seedActionRecovery(t, fx)
			}
			ids := []uuid.UUID{fx.runID}
			rows, err := env.q.ListCodexAccountActionInputs(env.ctx, store.ListCodexAccountActionInputsParams{
				RunIds: ids, ReuseInFlightMicroseconds: codexRefreshReuseInFlightBound.Microseconds(),
			})
			if err != nil || len(rows) != 1 {
				t.Fatalf("query rows=%d err=%v", len(rows), err)
			}
			if rows[0].RetryPending != tc.pending {
				t.Fatalf("retry_pending=%v want %v", rows[0].RetryPending, tc.pending)
			}
			// Retry eligibility is DB-clocked even when the service clock is far ahead.
			svc.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
			got, err := svc.CodexAccountActionsForRuns(env.ctx, ids)
			if err != nil || len(got) != 1 || got[fx.runID].Action != tc.want {
				t.Fatalf("actions=%v err=%v want %s", got, err, tc.want)
			}
		})
	}
}

func TestCodexAccountActionRetryBoundParamLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	fx, _ := heldCodexFix(t, env)
	op := uuid.New()
	env.exec("UPDATE codex_provider_account SET coord_operation_id=$2, lease_deadline=NULL WHERE id=$1", fx.accountID, op)
	env.exec(`INSERT INTO codex_refresh_intent
        (operation_id,user_id,provider_account_id,from_generation,state,created_at,provider_error_at,reuse_attempted_at)
        SELECT $1,user_id,id,generation,'rotating',now()-interval '1 hour',now()-interval '1 minute',now()-interval '1 minute'
        FROM codex_provider_account WHERE id=$2`, op, fx.accountID)
	t.Cleanup(func() { env.exec("DELETE FROM codex_refresh_intent WHERE operation_id=$1", op) })
	for _, tc := range []struct {
		bound   time.Duration
		pending bool
	}{
		{codexRefreshReuseInFlightBound, false},
		{2 * time.Minute, true},
	} {
		rows, err := env.q.ListCodexAccountActionInputs(env.ctx, store.ListCodexAccountActionInputsParams{
			RunIds: []uuid.UUID{fx.runID}, ReuseInFlightMicroseconds: tc.bound.Microseconds(),
		})
		if err != nil || len(rows) != 1 {
			t.Fatalf("bound=%v rows=%d err=%v", tc.bound, len(rows), err)
		}
		if rows[0].RetryPending != tc.pending {
			t.Fatalf("bound=%v retry_pending=%v want %v", tc.bound, rows[0].RetryPending, tc.pending)
		}
	}
}
