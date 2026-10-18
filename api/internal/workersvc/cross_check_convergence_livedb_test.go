package workersvc

import (
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each subtest has at most five serial candidates, with no retries or timer waits.
// A failed candidate stops that subtest; sibling outcomes remain independent.
func TestPlanCrossCheckAutomaticRoundsConvergenceLiveDB(t *testing.T) {
	type convergenceOutcome struct {
		name, verdict, reason string
		last                  int32
		defaultLimit          bool
	}
	outcomes := []convergenceOutcome{}
	for candidate := int32(1); candidate <= 5; candidate++ {
		outcomes = append(outcomes, convergenceOutcome{fmt.Sprintf("approve%d", candidate), "approve", "approve", candidate, false})
	}
	for _, pair := range [][2]string{
		{"revise", "revise"},
		{"block", "block"},
		{"failed", "timed_out"},
		{"failed", "malformed"},
		{"failed", "model_error"},
		{"failed", "model_timeout"},
		{"failed", "checker_unavailable"},
		{"failed", "confinement_failed"},
	} {
		outcomes = append(outcomes, convergenceOutcome{pair[1], pair[0], pair[1], 5, false})
	}
	outcomes = append(outcomes,
		convergenceOutcome{"default/approve5", "approve", "approve", 5, true},
		convergenceOutcome{"default/revise", "revise", "revise", 5, true},
	)
	for _, direction := range []struct{ lead, checker string }{{"claude", "codex"}, {"codex", "claude"}} {
		for _, outcome := range outcomes {
			t.Run(direction.lead+"/"+outcome.name, func(t *testing.T) {
				var (
					env       codexTestEnv
					svc       *Service
					w         store.Worker
					lead      uuid.UUID
					candidate PlanCrossCheckCandidate
				)
				if outcome.defaultLimit {
					p := testParams()
					p.PlanCrossCheckMaxRevisions = 0
					p.PlanCrossCheckMaxRevisionsSet = false
					if p.PlanCrossCheckMaxRevisions != 0 || p.PlanCrossCheckMaxRevisionsSet {
						t.Fatal("default revision limit must be unset before New")
					}
					env, svc, w, lead, candidate = newAutomaticRoundsHarnessFixtureWithParams(t, p, direction.lead)
				} else {
					env, svc, w, lead, candidate = newAutomaticRoundsHarnessFixture(t, 4, direction.lead)
				}
				for round := int32(1); round <= outcome.last; round++ {
					candidate.PlanMd = fmt.Sprintf("Candidate %d\n\n## Cross-check dispositions\n- Prior finding: addressed in this candidate.", round)
					cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, candidate, round)
					if err != nil {
						t.Fatalf("candidate %d: %v", round, err)
					}
					child := mustRun(t, env, uuid.UUID(cc.CheckerRunID.Bytes))
					if cc.Round != round || !cc.AutomaticRoundsEnabled || cc.AutomaticRevisionLimit != 4 ||
						cc.CheckerHarness.String != direction.checker || child.Harness != direction.checker ||
						child.Kind != "cross_check" || !child.ReportOnly || !child.TargetRunID.Valid || child.TargetRunID.Bytes != lead {
						t.Fatalf("candidate %d: snapshot=%+v child=%+v", round, cc, child)
					}
					verdict, reason := "revise", "revise"
					if round == outcome.last {
						verdict, reason = outcome.verdict, outcome.reason
					}
					if reason == "timed_out" {
						// Timeout is a server settlement, never a model verdict/reason pair.
						env.exec("UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE id=$1", cc.ID)
						settled, _, err := svc.PlanCrossCheckStatus(env.ctx, w, lead, 1, round)
						if err != nil || settled.Verdict != "failed" || settled.ReasonClass.String != "timed_out" {
							t.Fatalf("timeout settlement: %+v err=%v", settled, err)
						}
					} else {
						env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child.ID, w.ID)
						if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child.ID, 1, verdict, reason,
							[]byte(`{"summary":"Checker disposition","items":[]}`)); err != nil {
							t.Fatal(err)
						}
					}
				}

				normalized, err := NormalizePlanCrossCheckCandidate(candidate)
				if err != nil {
					t.Fatal(err)
				}
				digest, err := normalized.Digest()
				if err != nil {
					t.Fatal(err)
				}
				generation := int64(1)
				milestones := []Milestone{}
				presentation := uuid.New()
				req := StateRequest{State: "running", ClaimGeneration: &generation,
					PlanMd: &normalized.PlanMd, Milestones: &milestones,
					RequiredCapabilities: &normalized.RequiredCapabilities, RequiredTools: &normalized.RequiredTools,
					SizeClass: &normalized.SizeClass, CandidateDigest: hex.EncodeToString(digest)}
				if outcome.verdict != "approve" {
					want := outcome.reason
					if outcome.verdict == "revise" {
						want = "revisions_exhausted"
					}
					metadata, err := svc.LatestPlanCrossCheck(env.ctx, w, lead, 1)
					if err != nil || metadata.Round != 5 || metadata.NextRoundEligible ||
						metadata.NextRound != nil || metadata.FallbackReason != want {
						t.Fatalf("final metadata: %+v err=%v want=%s", metadata, err, want)
					}
					// Round 6 checks the protocol bound, not the revision budget independently.
					// Metadata fallback above and lower-limit budget tests prove accounting.
					if _, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, candidate, 6); !errors.Is(err, ErrCrossCheckRefused) {
						t.Fatalf("round 6 protocol-bound refusal: got %v, want %v", err, ErrCrossCheckRefused)
					}
					_, applied, err := svc.SetState(env.ctx, w, lead, req)
					if applied || !errors.Is(err, ErrInvalidState) {
						t.Fatalf("nonapproval entered implementation: applied=%v err=%v", applied, err)
					}
					req.State = "awaiting_approval"
					req.CandidateDigest = ""
					req.PresentationID = &presentation
					// The server derives the reason from the actual final attempt.
					_, applied, err = svc.SetState(env.ctx, w, lead, req)
					run := mustRun(t, env, lead)
					if err != nil || !applied || run.Status != "awaiting_approval" || run.AutoApprove ||
						run.PlanCrossCheckGateReason.String != want {
						t.Fatalf("human gate: applied=%v status=%s auto=%v reason=%s err=%v",
							applied, run.Status, run.AutoApprove, run.PlanCrossCheckGateReason.String, err)
					}
				} else {
					_, applied, err := svc.SetState(env.ctx, w, lead, req)
					run := mustRun(t, env, lead)
					if err != nil || !applied || run.Status != "running" || !run.AutoApprove ||
						run.PlanMd.String != normalized.PlanMd || run.PlanCrossCheckGateReason.Valid {
						t.Fatalf("approved canonical plan not acknowledged: applied=%v status=%s plan=%q err=%v",
							applied, run.Status, run.PlanMd.String, err)
					}
				}
				var attempts, children, humanRevisions int
				if err := env.pool.QueryRow(env.ctx, `SELECT
					(SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
					(SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check'),
					revise_count FROM runs WHERE id=$1`, lead).Scan(&attempts, &children, &humanRevisions); err != nil {
					t.Fatal(err)
				}
				if attempts != int(outcome.last) || children != int(outcome.last) || humanRevisions != 0 {
					t.Fatalf("attempts=%d children=%d human revisions=%d", attempts, children, humanRevisions)
				}
			})
		}
	}
}
