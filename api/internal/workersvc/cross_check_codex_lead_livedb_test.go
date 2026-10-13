package workersvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #2460: a Codex lead's plan is checked by a CLAUDE child. The owner here also holds a
// usable Codex credential, so a wrongly derived (same-family) checker would succeed
// silently; every assertion is on the child's actual harness and on the absence of one.

type codexLeadFix struct {
	env    codexTestEnv
	svc    *Service
	worker store.Worker
	lead   uuid.UUID
	user   uuid.UUID
	cand   PlanCrossCheckCandidate
}

// anthropic is "usable", "none" or "disabled"; withCap grants cross_check_codex_lead_v1.
func newCodexLeadFix(t *testing.T, anthropic string, withCap bool) *codexLeadFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, worker, repo := env.seedCodexInfra(t)
	t.Cleanup(func() { env.exec(`DELETE FROM users WHERE id=$1`, user) })
	codexKey := env.seedStaticAPIKey(t, user, "codex-key", "fixture-"+uuid.NewString())
	env.exec("UPDATE user_secrets SET is_default=true WHERE id=$1", codexKey)
	switch anthropic {
	case "usable":
		seedDefaultAnthropicToken(t, env, user)
	case "disabled":
		seedDefaultAnthropicToken(t, env, user)
		env.exec(`UPDATE user_secrets SET disabled_at=now(), enablement_rev=enablement_rev+1 WHERE user_id=$1 AND kind='anthropic_token'`, user)
	}
	caps := []string{capability.CrossCheckV1, capability.CrossCheckRoundsV1}
	if withCap {
		caps = append(caps, capability.CrossCheckCodexLeadV1)
	}
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, caps)
	lead := uuid.New()
	env.exec(`INSERT INTO runs(id,user_id,repo_id,worker_id,issue_iid,issue_title,issue_description,status,harness,
		auto_approve,plan_cross_check_required,claim_generation)
		VALUES($1,$2,$3,$4,2460,'codex lead issue','body','running','codex',true,true,1)`, lead, user, repo, worker)
	p := testParams()
	p.PlanCrossCheckMaxRevisions = 2
	p.PlanCrossCheckMaxRevisionsSet = true
	svc := New(env.q, env.box, p)
	svc.SetTxBeginner(env.pool)
	c := PlanCrossCheckCandidate{PlanMd: "plan", Milestones: []byte("[]"), RequiredCapabilities: []string{},
		RequiredTools: []string{}, SizeClass: "s", BaseCommit: strings.Repeat("a", 40), PlanningDiff: "diff"}
	return &codexLeadFix{env: env, svc: svc, worker: store.Worker{ID: worker, UserID: user}, lead: lead, user: user, cand: c}
}

func (f *codexLeadFix) children(t *testing.T) (total, claude int) {
	t.Helper()
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*), count(*) FILTER (WHERE harness='claude')
		FROM runs WHERE target_run_id=$1 AND kind='cross_check'`, f.lead).Scan(&total, &claude); err != nil {
		t.Fatal(err)
	}
	return total, claude
}

// A Codex lead submits and every round's child is a Claude child, with the Codex credential
// present and usable the whole time.
func TestCodexLeadSubmitCreatesClaudeChildrenLiveDB(t *testing.T) {
	f := newCodexLeadFix(t, "usable", true)
	for round := int32(1); round <= 2; round++ {
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.worker, f.lead, 1, f.cand, round)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		child := mustRun(t, f.env, uuid.UUID(cc.CheckerRunID.Bytes))
		if cc.CheckerHarness.String != "claude" || child.Harness != "claude" || child.Kind != "cross_check" || !child.ReportOnly {
			t.Fatalf("round %d: checker_harness=%q child=%s/%s, want a claude cross_check child", round, cc.CheckerHarness.String, child.Kind, child.Harness)
		}
		f.env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child.ID, f.worker.ID)
		if round == 1 {
			if _, err := f.svc.DecidePlanCrossCheck(f.env.ctx, f.worker, child.ID, 1, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if total, claude := f.children(t); total != 2 || claude != 2 {
		t.Fatalf("children total=%d claude=%d, want two Claude children and no Codex checker", total, claude)
	}
}

// A worker without cross_check_codex_lead_v1 cannot submit for a Codex lead: nothing is created.
func TestCodexLeadSubmitRefusedWithoutCapabilityLiveDB(t *testing.T) {
	f := newCodexLeadFix(t, "usable", false)
	if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.worker, f.lead, 1, f.cand, 1); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("submit from an older worker: %v, want ErrCrossCheckRefused", err)
	}
	var rows int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT (SELECT count(*) FROM cross_checks WHERE lead_run_id=$1)
		+ (SELECT count(*) FROM runs WHERE target_run_id=$1)`, f.lead).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("refused submit left %d rows (err=%v)", rows, err)
	}
}

// An unusable Claude credential is checker_unavailable at submit; the usable Codex
// credential never produces a Codex checker for a Codex lead.
func TestCodexLeadSubmitNeverFallsBackToCodexCheckerLiveDB(t *testing.T) {
	for _, anthropic := range []string{"none", "disabled"} {
		t.Run(anthropic, func(t *testing.T) {
			f := newCodexLeadFix(t, anthropic, true)
			if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.worker, f.lead, 1, f.cand, 1); !errors.Is(err, ErrCrossCheckUnavailable) {
				t.Fatalf("submit with %s Claude credential: %v, want ErrCrossCheckUnavailable", anthropic, err)
			}
			if total, _ := f.children(t); total != 0 {
				t.Fatalf("created %d checker children, want none", total)
			}
		})
	}
}

// The latest-attempt metadata offers a Codex lead its next round only when its claiming
// worker advertises the capability.
func TestCodexLeadLatestMetadataNeedsCapabilityLiveDB(t *testing.T) {
	for _, withCap := range []bool{true, false} {
		f := newCodexLeadFix(t, "usable", withCap)
		got, err := f.svc.LatestPlanCrossCheck(f.env.ctx, f.worker, f.lead, 1)
		if err != nil {
			t.Fatal(err)
		}
		if got.NextRoundEligible != withCap || (got.NextRound != nil) != withCap {
			t.Fatalf("capability=%v: next_round=%v eligible=%v", withCap, got.NextRound, got.NextRoundEligible)
		}
	}
}

// The checker_unavailable attestation is judged against the lead's OPPOSITE family, and
// codex_lead_unsupported stays valid for a Codex lead (an older worker still declares it).
func TestPlanCrossCheckCheckerUnavailableAttestationFamilyLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, leadHarness, anthropic string
		codexUsable                  bool
		reason                       string
		valid                        bool
	}{
		{"codex lead, Claude credential missing", "codex", "none", true, "checker_unavailable", true},
		{"codex lead, Claude credential disabled", "codex", "disabled", true, "checker_unavailable", true},
		{"codex lead, Claude credential usable", "codex", "usable", false, "checker_unavailable", false},
		{"claude lead, Codex credential missing", "claude", "usable", false, "checker_unavailable", true},
		{"claude lead, Codex credential usable", "claude", "usable", true, "checker_unavailable", false},
		{"codex lead keeps the legacy unsupported declaration", "codex", "usable", true, "codex_lead_unsupported", true},
		{"claude lead cannot declare codex_lead_unsupported", "claude", "usable", true, "codex_lead_unsupported", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCodexLeadFix(t, tc.anthropic, true)
			f.env.exec(`UPDATE runs SET harness=$2 WHERE id=$1`, f.lead, tc.leadHarness)
			if !tc.codexUsable {
				f.env.exec(`DELETE FROM user_secrets WHERE user_id=$1 AND kind='openai_api_key'`, f.user)
			}
			plan, size := "A candidate for human review", "s"
			caps, tools, ms := []string{}, []string{}, []Milestone{}
			gen := int64(1)
			req := StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &ms,
				RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, PlanCrossCheckGateReason: &tc.reason}
			_, applied, err := f.svc.SetState(f.env.ctx, f.worker, f.lead, req)
			if tc.valid {
				if err != nil || !applied {
					t.Fatalf("valid declaration refused: applied=%v err=%v", applied, err)
				}
				if got := mustRun(t, f.env, f.lead).PlanCrossCheckGateReason.String; got != tc.reason {
					t.Fatalf("persisted reason=%q, want %q", got, tc.reason)
				}
				return
			}
			if applied || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("false attestation accepted: applied=%v err=%v", applied, err)
			}
		})
	}
}
