package workersvc

import (
	"testing"

	"github.com/google/uuid"
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
		{"quarantined, recovery material but reauth required", func(t *testing.T, fx *codexClaimFix) {
			seedActionRecovery(t, fx)
			fx.setAccount(t, `reauth_required = true, reauth_generation = generation,
				reauth_credential_revision = credential_revision`)
		}, CodexAccountActionReloginRequired},
		{"quarantined, live lease", func(t *testing.T, fx *codexClaimFix) {
			fx.setAccount(t, `lease_deadline = now() + interval '1 hour'`)
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
