package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file is the live-DB half of PRD #1247 M4: it EXECUTES the new set-token queries
// (SetRunCredentialOverride, PromoteLimitWaitRunNow, PromoteRecoveryWaitRunNow) and reuses
// PromotePoolWaitRun / ResumePausedRun against a REAL Postgres — sqlc's type deduction is
// not Postgres's, so a guarded UPDATE can pass `sqlc generate` yet fail at prepare/execute.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (setupCodexLiveDB
// skips). It reuses the M3 seed helpers (seedReevalOwner, seedParkedLimitWait, statusOf).

// seedRunInStatus inserts one run owned by o in the given non-held status, with started_at
// and status_since set as a real parked run would carry them, and returns its id. For
// limit_wait it records the dead token + a FUTURE retry_not_before so claimExclude keeps
// excluding it; for recovery_wait a FUTURE recovery_retry_not_before; for paused a
// status_since 5 minutes back so ResumePausedRun banks a measurable gap.
func seedRunInStatus(t *testing.T, env codexTestEnv, o reevalOwner, issueIID int64, status string, now time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	switch status {
	case "queued":
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, status_since, worker_id)
		          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', now(), $5)`,
			id, o.userID, o.repoID, issueIID, o.workerID)
	case "limit_wait":
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
		             status, status_since, worker_id, anthropic_secret_id, limit_dead_secret_id,
		             retry_not_before, limit_wait_count, wait_on_limit, started_at)
		          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'limit_wait', now(), $5, $6, $6, $7, 1, true, now())`,
			id, o.userID, o.repoID, issueIID, o.workerID, o.deadTok, pgconv.Time(now.Add(time.Hour)))
	case "pool_wait":
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
		             status, status_since, worker_id, wait_on_limit, started_at)
		          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'pool_wait', now(), $5, true, now())`,
			id, o.userID, o.repoID, issueIID, o.workerID)
	case "recovery_wait":
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
		             status, status_since, worker_id, recovery_wait_count, recovery_retry_not_before, started_at)
		          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'recovery_wait', now(), $5, 1, $6, now())`,
			id, o.userID, o.repoID, issueIID, o.workerID, pgconv.Time(now.Add(time.Hour)))
	case "paused":
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
		             status, status_since, worker_id, started_at)
		          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'paused', now() - interval '5 minutes', $5, now() - interval '20 minutes')`,
			id, o.userID, o.repoID, issueIID, o.workerID)
	default:
		t.Fatalf("seedRunInStatus: unhandled status %q", status)
	}
	return id
}

// TestSetRunCredentialParkedStatesPromoteLiveDB is D4's state table for the NON-HELD
// states: each of queued/limit_wait/pool_wait/recovery_wait/paused takes a pinned override
// and lands (or stays) at `queued` with the override columns written, and each carries the
// right post-resume deadline accounting — a parked-park early-promote gets a FRESH wall
// (started_at NULL) so it cannot time out on its first report, while a resumed pause KEEPS
// its wall and BANKS its budget. The limit_wait case additionally proves the limit stamp
// and limit_dead_secret_id are UNTOUCHED (claimExclude depends on both).
func TestSetRunCredentialParkedStatesPromoteLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	now := time.Now().UTC()
	o := seedReevalOwner(t, env, BindModeAuto, false)

	cases := []struct {
		status           string
		expectFreshWall  bool // started_at NULL after (an early promote)
		expectBankBudget bool // budget_paused_seconds > 0 after (a resumed pause)
	}{
		{"queued", false, false},
		{"limit_wait", true, false},
		{"pool_wait", true, false},
		{"recovery_wait", true, false},
		{"paused", false, true},
	}
	for i, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			runID := seedRunInStatus(t, env, o, int64(4400+i), tc.status, now)
			res, err := svc.SetRunCredential(env.ctx, o.userID, runID, CredentialOverrideModePinned, &o.altTok)
			if err != nil {
				t.Fatalf("SetRunCredential(%s): %v", tc.status, err)
			}
			if res.Run.Status != "queued" {
				t.Fatalf("%s → status %q, want queued", tc.status, res.Run.Status)
			}
			// The override columns are written for every writable state.
			if res.Run.CredentialOverrideMode.String != CredentialOverrideModePinned {
				t.Errorf("%s: credential_override_mode = %q, want pinned", tc.status, res.Run.CredentialOverrideMode.String)
			}
			if !res.Run.CredentialOverrideSecretID.Valid || uuid.UUID(res.Run.CredentialOverrideSecretID.Bytes) != o.altTok {
				t.Errorf("%s: credential_override_secret_id = %+v, want %s", tc.status, res.Run.CredentialOverrideSecretID, o.altTok)
			}
			// Post-resume deadline accounting per state.
			if tc.expectFreshWall && res.Run.StartedAt.Valid {
				t.Errorf("%s: started_at = %v, want NULL (fresh wall so it cannot time out on its first report)", tc.status, res.Run.StartedAt.Time)
			}
			if tc.expectBankBudget {
				if !res.Run.StartedAt.Valid {
					t.Errorf("paused: started_at = NULL, want it PRESERVED (a resumed pause keeps its wall)")
				}
				if res.Run.BudgetPausedSeconds <= 0 {
					t.Errorf("paused: budget_paused_seconds = %d, want > 0 (the parked gap is banked)", res.Run.BudgetPausedSeconds)
				}
			}
			// limit_wait: the limit stamp + dead-token id must be UNTOUCHED so claimExclude
			// keeps excluding the dead token while its window is closed.
			if tc.status == "limit_wait" {
				if !res.Run.LimitDeadSecretID.Valid || uuid.UUID(res.Run.LimitDeadSecretID.Bytes) != o.deadTok {
					t.Errorf("limit_wait: limit_dead_secret_id = %+v, want it UNTOUCHED (%s)", res.Run.LimitDeadSecretID, o.deadTok)
				}
				if !res.Run.RetryNotBefore.Valid || !res.Run.RetryNotBefore.Time.After(now) {
					t.Errorf("limit_wait: retry_not_before = %+v, want it UNTOUCHED (still future)", res.Run.RetryNotBefore)
				}
			}
		})
	}
}

// TestSetRunCredentialLimitWaitAutoClaimExcludesDeadTokenLiveDB is the M4 exclusion proof:
// switching a limit_wait run to `auto` early-promotes it, and because the promote preserves
// limit_dead_secret_id + the still-future retry_not_before, the ensuing auto claim EXCLUDES
// the just-exhausted dead token and spends the alternative instead. The dead token is given
// a BETTER gauge than the alternative, so if the exclusion were lost it would win — the
// claim landing on the alt is what proves claimExclude still fires after the early promote.
func TestSetRunCredentialLimitWaitAutoClaimExcludesDeadTokenLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, autoParams())
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	o := seedReevalOwner(t, env, BindModeAuto, true) // altTok gets a fresh eligible gauge
	env.sealBotPAT(t, o.userID)
	// Give the DEAD token a BETTER gauge (higher headroom) so it would WIN if not excluded.
	env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
	          VALUES ($1, $2, 5, 5, 'usage_endpoint', now())`, o.deadTok, o.userID)

	runID := seedParkedLimitWait(t, env, o, 4500, now.Add(-time.Minute), now.Add(time.Hour))

	res, err := svc.SetRunCredential(env.ctx, o.userID, runID, CredentialOverrideModeAuto, nil)
	if err != nil {
		t.Fatalf("SetRunCredential(auto): %v", err)
	}
	if res.Run.Status != "queued" {
		t.Fatalf("status after switch = %q, want queued", res.Run.Status)
	}
	if res.Run.CredentialOverrideMode.String != CredentialOverrideModeAuto {
		t.Fatalf("credential_override_mode = %q, want auto", res.Run.CredentialOverrideMode.String)
	}
	if !res.Run.LimitDeadSecretID.Valid || uuid.UUID(res.Run.LimitDeadSecretID.Bytes) != o.deadTok {
		t.Fatalf("limit_dead_secret_id = %+v, want the dead token %s UNTOUCHED", res.Run.LimitDeadSecretID, o.deadTok)
	}
	if res.Run.StartedAt.Valid {
		t.Fatalf("started_at = %v after early promote, want NULL (fresh wall)", res.Run.StartedAt.Time)
	}

	// The ensuing auto claim must spend the ALT token — the dead one is excluded while its
	// window (retry_not_before, still future) is closed.
	wkr := store.Worker{ID: o.workerID, UserID: o.userID, Name: "w-claim", Status: "online", AnthropicBindMode: BindModeAuto}
	payload, err := svc.Claim(env.ctx, wkr)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if payload == nil || payload.RunID != runID.String() {
		t.Fatalf("Claim returned %+v, want the promoted run %s", payload, runID)
	}
	claimed := mustRun(t, env, runID)
	if !claimed.AnthropicSecretID.Valid || uuid.UUID(claimed.AnthropicSecretID.Bytes) != o.altTok {
		t.Fatalf("claim spent %+v, want the ALT token %s (the dead token %s must have been EXCLUDED despite its better gauge)",
			claimed.AnthropicSecretID, o.altTok, o.deadTok)
	}
}

// TestSetRunCredentialRefusedStatesLiveDB proves the held / claimed / terminal states are
// refused with the right typed error AND write NO override (the held-state switch protocol
// is M5, and a terminal/claimed run cannot switch). A foreign run is ErrRunNotFound.
func TestSetRunCredentialRefusedStatesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	now := time.Now().UTC()
	o := seedReevalOwner(t, env, BindModeAuto, false)

	seedInStatus := func(status string, issueIID int64) uuid.UUID {
		id := uuid.New()
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, status_since, worker_id)
		          VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, now(), $6)`,
			id, o.userID, o.repoID, issueIID, status, o.workerID)
		return id
	}

	cases := []struct {
		status  string
		wantErr error
	}{
		{"running", ErrCredentialSwitchHeldStateUnsupported},
		{"awaiting_approval", ErrCredentialSwitchHeldStateUnsupported},
		{"awaiting_input", ErrCredentialSwitchHeldStateUnsupported},
		{"awaiting_followup", ErrCredentialSwitchHeldStateUnsupported},
		{"claimed", ErrCredentialSwitchClaimAssembling},
		{"completed", ErrCredentialSwitchRunTerminal},
		{"failed", ErrCredentialSwitchRunTerminal},
		{"cancelled", ErrCredentialSwitchRunTerminal},
	}
	for i, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			runID := seedInStatus(tc.status, int64(4600+i))
			_, err := svc.SetRunCredential(env.ctx, o.userID, runID, CredentialOverrideModePinned, &o.altTok)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("SetRunCredential(%s): err = %v, want %v", tc.status, err, tc.wantErr)
			}
			// No override may be written on a refused switch.
			run := mustRun(t, env, runID)
			if run.CredentialOverrideMode.Valid {
				t.Errorf("%s: credential_override_mode = %q, want NULL (a refused switch writes nothing)", tc.status, run.CredentialOverrideMode.String)
			}
			if run.Status != tc.status {
				t.Errorf("%s: status changed to %q, want it UNCHANGED", tc.status, run.Status)
			}
		})
	}

	// A foreign run (another user's) is ErrRunNotFound, not any switch error.
	t.Run("foreign run is not found", func(t *testing.T) {
		other := seedReevalOwner(t, env, BindModeAuto, false)
		foreignRun := seedRunInStatus(t, env, other, 4699, "queued", now)
		_, err := svc.SetRunCredential(env.ctx, o.userID, foreignRun, CredentialOverrideModePinned, &o.altTok)
		if !errors.Is(err, ErrRunNotFound) {
			t.Fatalf("foreign run: err = %v, want ErrRunNotFound", err)
		}
	})
}

// TestSetRunCredentialWarningsLiveDB covers the D6 warnings: pinning the run's own dead
// token, pinning a token whose gauge reads stale/exhausted, and auto with an empty
// pool-minus-dead all return 200 + a warning; a healthy pin returns none. A warning NEVER
// refuses — the switch still applies (status → queued).
func TestSetRunCredentialWarningsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, autoParams())
	now := time.Now().UTC()
	svc.now = func() time.Time { return now }

	t.Run("pinning the dead token warns", func(t *testing.T) {
		o := seedReevalOwner(t, env, BindModeAuto, false)
		runID := seedParkedLimitWait(t, env, o, 4700, now.Add(-time.Minute), now.Add(time.Hour))
		res, err := svc.SetRunCredential(env.ctx, o.userID, runID, CredentialOverrideModePinned, &o.deadTok)
		if err != nil {
			t.Fatalf("SetRunCredential: %v", err)
		}
		if res.Run.Status != "queued" {
			t.Fatalf("status = %q, want queued (a warning never refuses)", res.Run.Status)
		}
		if res.Warning == "" {
			t.Fatalf("pinning the run's own dead token must return a warning")
		}
	})

	t.Run("healthy pin returns no warning", func(t *testing.T) {
		o := seedReevalOwner(t, env, BindModeAuto, true) // altTok has a fresh eligible gauge
		runID := seedRunInStatus(t, env, o, 4701, "queued", now)
		res, err := svc.SetRunCredential(env.ctx, o.userID, runID, CredentialOverrideModePinned, &o.altTok)
		if err != nil {
			t.Fatalf("SetRunCredential: %v", err)
		}
		if res.Warning != "" {
			t.Fatalf("a healthy pinned token must return NO warning, got %q", res.Warning)
		}
	})

	t.Run("stale-gauge pin warns", func(t *testing.T) {
		o := seedReevalOwner(t, env, BindModeAuto, false)
		// altTok's gauge is far in the past → stale (autoParams MaxStaleness is 15m).
		env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
		          VALUES ($1, $2, 20, 10, 'usage_endpoint', now() - interval '2 hours')`, o.altTok, o.userID)
		runID := seedRunInStatus(t, env, o, 4702, "queued", now)
		res, err := svc.SetRunCredential(env.ctx, o.userID, runID, CredentialOverrideModePinned, &o.altTok)
		if err != nil {
			t.Fatalf("SetRunCredential: %v", err)
		}
		if res.Warning == "" {
			t.Fatalf("pinning a token whose gauge reads stale must return a warning")
		}
	})

	t.Run("auto with empty pool-minus-dead warns", func(t *testing.T) {
		o := seedReevalOwner(t, env, BindModeAuto, false)
		// The only pooled token is the dead one; un-pool the alt so auto's pool minus the
		// dead token is empty → the run will hold in pool_wait.
		env.exec(`UPDATE user_secrets SET auto_eligible = false WHERE id = $1`, o.altTok)
		runID := seedParkedLimitWait(t, env, o, 4703, now.Add(-time.Minute), now.Add(time.Hour))
		res, err := svc.SetRunCredential(env.ctx, o.userID, runID, CredentialOverrideModeAuto, nil)
		if err != nil {
			t.Fatalf("SetRunCredential(auto): %v", err)
		}
		if res.Run.Status != "queued" {
			t.Fatalf("status = %q, want queued", res.Run.Status)
		}
		if res.Warning == "" {
			t.Fatalf("auto with an empty pool-minus-dead-token must warn (will hold in pool_wait)")
		}
	})
}
