package workersvc

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// lead_config_claim_livedb_test.go pins, on the REAL claim path against Postgres, that the two
// lead-source claim keys (default_model_source, default_effort_source; PRD #3244 section 3) appear
// only on lead-lane claims, and that a cross-check (checker) claim keeps its own resolution and is
// never touched by the lead resolver. Skipped unless UZI_TEST_DATABASE_URL is set
// (./e2e/run-store-it.sh).

// claimConfigKeys marshals the payload and returns the keys of its config object, so absence is
// asserted on the wire form (omitempty), not on a Go field.
func claimConfigKeys(t *testing.T, p *ClaimPayload) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(top["config"], &cfg); err != nil {
		t.Fatalf("claim config is not an object: %s", top["config"])
	}
	return cfg
}

func requireNoLeadSourceKeys(t *testing.T, label string, p *ClaimPayload) {
	t.Helper()
	cfg := claimConfigKeys(t, p)
	for _, k := range []string{"default_model_source", "default_effort_source"} {
		if v, ok := cfg[k]; ok {
			t.Errorf("%s: claim config carries %s=%s, want the key absent", label, k, v)
		}
	}
}

// TestCheckerClaimExcludedFromLeadResolutionLiveDB: a Claude checker child claimed while the
// owner's stored Claude lane holds a Codex role token. The lead resolver's #3245 lane guard and its
// fallback note must not run for the checker: no harness_model_fallback note on the child or the
// lead, the checker keeps the cross-check default effort (high, not the owner's worker effort), an
// explicit checker effort pin still wins, and neither lead-source key is shipped. Green on base:
// it preserves the assembleClaim gate (checkerResolution == nil) and #3602.
func TestCheckerClaimExcludedFromLeadResolutionLiveDB(t *testing.T) {
	const roleToken = "gpt-6-luna"
	for _, tc := range []struct {
		name       string
		pin        string // explicit code/claude checker effort pin ("" = none)
		wantEffort string
	}{
		{"unpinned checker keeps the cross-check default", "", "high"},
		{"explicit checker effort pin wins", "low", "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newClaudeCodeCheckerFix(t)
			f.env.exec(`UPDATE users SET default_claude_model = $2, default_effort = 'max' WHERE id = $1`, f.userID, roleToken)
			if tc.pin != "" {
				f.env.exec(`INSERT INTO user_cross_check_pins(user_id,stage,harness,effort) VALUES($1,'code','claude',$2)`, f.userID, tc.pin)
			}
			p := laneClaim(t, f, "cross_check", nil)
			if p == nil || p.CrossCheck == nil {
				t.Fatalf("checker claim not delivered: %+v", p)
			}
			if got := derefOr(p.Config.DefaultEffort, ""); got != tc.wantEffort {
				t.Errorf("checker default_effort = %q, want %q (not the owner's worker effort)", got, tc.wantEffort)
			}
			// The checker's model is its own resolution, which never goes through the Claude-lane
			// guard: the stored lane value is delivered untouched, with no fallback note.
			if got := derefOr(p.Config.DefaultModel, ""); got != roleToken {
				t.Errorf("checker default_model = %q, want the stored lane value %q (guard must not run for a checker)", got, roleToken)
			}
			requireNoLeadSourceKeys(t, "checker", p)
			for _, run := range []uuid.UUID{f.runID, f.lead} {
				if n := f.env.countHarnessModelFallbackNotes(t, run); n != 0 {
					t.Errorf("run %s has %d harness_model_fallback notes, want none for a checker claim", run, n)
				}
			}
		})
	}
}

// TestLeadSourceKeysAbsentForReviewClaimsLiveDB: Claude and Codex task-REVIEW claims (real
// assembleClaim) carry neither lead-source key, while an ordinary lead-lane claim of the same user
// carries both (the positive control that keeps the absence assertions from being vacuous).
func TestLeadSourceKeysAbsentForReviewClaimsLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	resealBotPAT(t, env, userID)
	seedDefaultAnthropicToken(t, env, userID)
	env.exec(`UPDATE users SET default_claude_model = 'opus', default_codex_model = $2 WHERE id = $1`, userID, curatedCodexModel)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	wkr := store.Worker{ID: workerID, UserID: userID,
		ProtocolCapabilities: []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexCustomModelV1}}

	seedReview := func(harness string) uuid.UUID {
		target, review := uuid.New(), uuid.New()
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, issue_title, issue_description, status, harness)
		          VALUES ($1, $2, $3, 'task', $4, 'main', 't', 'd', 'completed', $5)`, target, userID, repoID, "uzi/task/"+target.String(), harness)
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at, issue_title, issue_description, status, worker_id, harness)
		          VALUES ($1, $2, $3, 'task', $4, 'main', $5, now(), 't', 'd', 'claimed', $6, $7)`,
			review, userID, repoID, "uzi/task/"+review.String(), target, workerID, harness)
		return review
	}

	t.Run("claude review", func(t *testing.T) {
		review := seedReview("claude")
		p, err := svc.assembleClaim(env.ctx, wkr, mustRun(t, env, review))
		if err != nil {
			t.Fatalf("assembleClaim: %v", err)
		}
		requireNoLeadSourceKeys(t, "claude review", p)
	})

	t.Run("codex review", func(t *testing.T) {
		review := seedReview("codex")
		aliasID := env.seedLinkedSubscription(t, userID, "codex-lead-src-"+uuid.NewString(), codexToken("access"), codexToken("refresh"))
		if err := svc.FreezeCodexBinding(env.ctx, userID, review, aliasID, codexAuthModeSubscription); err != nil {
			t.Fatalf("FreezeCodexBinding: %v", err)
		}
		p, err := svc.assembleClaim(env.ctx, wkr, mustRun(t, env, review))
		if err != nil {
			t.Fatalf("assembleClaim: %v", err)
		}
		requireNoLeadSourceKeys(t, "codex review", p)
		if p.Config.DefaultModel != nil {
			t.Errorf("a Codex review ships no default_model, got %q", *p.Config.DefaultModel)
		}
	})

	t.Run("positive control: an issue claim carries both keys", func(t *testing.T) {
		run := uuid.New()
		env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		          VALUES ($1, $2, $3, 'issue', 4343, 't', 'd', 'claimed', $4)`, run, userID, repoID, workerID)
		p, err := svc.assembleClaim(env.ctx, wkr, mustRun(t, env, run))
		if err != nil {
			t.Fatalf("assembleClaim: %v", err)
		}
		cfg := claimConfigKeys(t, p)
		if _, ok := cfg["default_model_source"]; !ok {
			t.Error("lead-lane claim lacks default_model_source")
		}
		if _, ok := cfg["default_effort_source"]; !ok {
			t.Error("lead-lane claim lacks default_effort_source")
		}
	})
}

// TestIsolatedClaimCarriesNoLeadSourcesLiveDB: a profile-bound (isolated-lane) run claimed through
// the real Service.Claim carries neither lead-source key and its lead-config write is refused by the
// fence, while the same user's unbound run claim carries both (control). The isolated lane runs the
// IsolatedRunner, which has no lead executor (D1).
func TestIsolatedClaimCarriesNoLeadSourcesLiveDB(t *testing.T) {
	f := newIsoFix(t)
	bound := f.queuedRun(t, 311, true)
	lane := f.laneWorker(t, bound)
	p := f.claimAs(t, lane, bound)
	requireNoLeadSourceKeys(t, "isolated lane", p)

	rep := leadReport(p.ClaimGeneration, leadcfg.PhasePlan, 1)
	if _, err := f.svc.RecordLeadConfig(f.env.ctx, lane, bound, rep); !errors.Is(err, store.ErrLeadConfigClaimNotCurrent) {
		t.Fatalf("lead-config write on an isolated-lane run = %v, want ErrLeadConfigClaimNotCurrent", err)
	}
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_lead_configs WHERE run_id = $1`, bound).Scan(&n); err != nil || n != 0 {
		t.Fatalf("isolated-lane run stored %d lead-config rows (%v), want 0", n, err)
	}

	// Control: an unbound issue run for the same user is a lead-lane claim.
	unbound := f.queuedRun(t, 312, false)
	cp := f.claimAs(t, f.wkr, unbound)
	cfg := claimConfigKeys(t, cp)
	if _, ok := cfg["default_effort_source"]; !ok {
		t.Fatal("control: the unbound lead-lane claim lacks default_effort_source")
	}
}
