package workersvc

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3708 live-DB coverage of the gated plan cross-check server side: submit, the gated
// recovery policy, the LatestPlanCrossCheck / SetRunAwaitingApproval parity, the digest-bound
// "passed" gate and the trigger-error mapping. Every transition goes through the real Service.

const gatedProtocolCaps = "ARRAY['cross_check_v1','cross_check_rounds_v1','cross_check_gated_v1']"

type gatedFixture struct {
	env codexTestEnv
	svc *Service
	w   store.Worker
	// lead is a gated lead run at claim generation 1.
	lead uuid.UUID
	c    PlanCrossCheckCandidate
}

// newGatedFixture is the rounds fixture with the lead switched to gated mode: not auto-approved,
// not required, and a worker that advertises cross_check_gated_v1 (in the row and on the struct).
func newGatedFixture(t *testing.T, limit int32) gatedFixture {
	t.Helper()
	env, svc, w, lead, c := newAutomaticRoundsFixture(t, limit)
	t.Cleanup(func() { env.exec(`DELETE FROM users WHERE id=$1`, w.UserID) })
	env.exec(`UPDATE runs SET auto_approve=false, plan_cross_check_required=false, plan_cross_check_gated=true, plan_source='agent' WHERE id=$1`, lead)
	env.exec("UPDATE workers SET protocol_capabilities="+gatedProtocolCaps+" WHERE id=$1", w.ID)
	w.ProtocolCapabilities = []string{capability.CrossCheckV1, capability.CrossCheckRoundsV1, capability.CrossCheckGatedV1}
	return gatedFixture{env: env, svc: svc, w: w, lead: lead, c: c}
}

func (f gatedFixture) counts(t *testing.T) (rows, children int) {
	t.Helper()
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT (SELECT count(*) FROM cross_checks WHERE lead_run_id=$1),
		(SELECT count(*) FROM runs WHERE target_run_id=$1 AND kind='cross_check')`, f.lead).Scan(&rows, &children); err != nil {
		t.Fatal(err)
	}
	return rows, children
}

func (f gatedFixture) setGeneration(gen int64) {
	f.env.exec("UPDATE runs SET claim_generation=$2 WHERE id=$1", f.lead, gen)
}

func (f gatedFixture) latest(t *testing.T, gen int64) PlanCrossCheckLatestMetadata {
	t.Helper()
	got, err := f.svc.LatestPlanCrossCheck(f.env.ctx, f.w, f.lead, gen)
	if err != nil {
		t.Fatalf("LatestPlanCrossCheck: %v", err)
	}
	return got
}

// decide moves the round's checker child onto the worker and records its verdict.
func (f gatedFixture) decide(t *testing.T, cc store.CrossCheck, verdict string) {
	t.Helper()
	child := uuid.UUID(cc.CheckerRunID.Bytes)
	f.env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, f.w.ID)
	if _, err := f.svc.DecidePlanCrossCheck(f.env.ctx, f.w, child, 1, verdict, verdict, []byte(`{"summary":"`+verdict+`","items":[]}`)); err != nil {
		t.Fatalf("decide %s: %v", verdict, err)
	}
}

func TestGatedPlanCrossCheckSubmitLiveDB(t *testing.T) {
	t.Run("a gated lead on a capable worker gets a row and a child, and a same-generation retry reuses them", func(t *testing.T) {
		f := newGatedFixture(t, 2)
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rows, children := f.counts(t); rows != 1 || children != 1 || cc.LeadClaimGeneration != 1 || !cc.DeadlineAt.Valid {
			t.Fatalf("rows=%d children=%d cc=%+v", rows, children, cc)
		}
		// Case (a): a lost submit ACK resubmits the same round and gets the live attempt back.
		retry, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
		if err != nil || retry.ID != cc.ID || retry.CheckerRunID != cc.CheckerRunID || !retry.DeadlineAt.Time.Equal(cc.DeadlineAt.Time) {
			t.Fatalf("retry did not reuse the attempt: %v", err)
		}
		if rows, children := f.counts(t); rows != 1 || children != 1 {
			t.Fatalf("retry created rows=%d children=%d", rows, children)
		}
		status, _, err := f.svc.PlanCrossCheckStatus(f.env.ctx, f.w, f.lead, 1, 1)
		if err != nil || status.ID != cc.ID {
			t.Fatalf("status of a gated lead: %v", err)
		}
		if latest := f.latest(t, 1); latest.Round != 1 || latest.NextRoundEligible || latest.FallbackReason != "" {
			t.Fatalf("a live same-generation attempt must keep today's metadata: %+v", latest)
		}
	})

	t.Run("a worker that does not advertise cross_check_gated_v1 is refused and nothing is created", func(t *testing.T) {
		f := newGatedFixture(t, 2)
		f.env.exec("UPDATE workers SET protocol_capabilities=ARRAY['cross_check_v1','cross_check_rounds_v1'] WHERE id=$1", f.w.ID)
		if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("incapable worker: %v", err)
		}
		if rows, children := f.counts(t); rows != 0 || children != 0 {
			t.Fatalf("refused submit created rows=%d children=%d", rows, children)
		}
	})

	t.Run("a run with neither mode, or required without auto-approval, is refused", func(t *testing.T) {
		for _, tc := range []struct{ name, set string }{
			{"neither", "plan_cross_check_gated=false"},
			{"required fell back to a human gate", "plan_cross_check_gated=false, plan_cross_check_required=true"},
		} {
			f := newGatedFixture(t, 2)
			f.env.exec("UPDATE runs SET "+tc.set+" WHERE id=$1", f.lead)
			if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if rows, children := f.counts(t); rows != 0 || children != 0 {
				t.Fatalf("%s created rows=%d children=%d", tc.name, rows, children)
			}
		}
	})

	t.Run("a gated Codex lead needs the Codex-lead capability like a required one", func(t *testing.T) {
		f := newGatedFixture(t, 2)
		f.env.exec(`UPDATE runs SET harness='codex' WHERE id=$1`, f.lead)
		seedDefaultAnthropicToken(t, f.env, f.w.UserID)
		if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("Codex lead without cross_check_codex_lead_v1: %v", err)
		}
		f.env.exec("UPDATE workers SET protocol_capabilities=ARRAY['cross_check_v1','cross_check_rounds_v1','cross_check_gated_v1','cross_check_codex_lead_v1'] WHERE id=$1", f.w.ID)
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
		if err != nil || cc.CheckerHarness.String != "claude" {
			t.Fatalf("Codex lead with the capability: err=%v checker=%q", err, cc.CheckerHarness.String)
		}
	})
}

func TestGatedPlanCrossCheckCustodyLossLiveDB(t *testing.T) {
	t.Run("custody loss while pending ends the check: one cancel, one wait credit, not eligible, no new row or child", func(t *testing.T) {
		f := newGatedFixture(t, 2)
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
		if err != nil {
			t.Fatal(err)
		}
		child := uuid.UUID(cc.CheckerRunID.Bytes)
		f.env.exec("UPDATE runs SET worker_id=$2,status='running',claim_generation=1 WHERE id=$1", child, f.w.ID)
		var pausedBefore int32
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT budget_paused_seconds FROM runs WHERE id=$1`, f.lead).Scan(&pausedBefore); err != nil {
			t.Fatal(err)
		}
		f.setGeneration(2)
		settled, err := f.env.q.GetExactPlanCrossCheck(f.env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.lead, Round: 1})
		if err != nil || settled.Verdict != "failed" || settled.ReasonClass.String != "superseded" || !settled.InterruptedAt.Valid || !settled.WaitCredited {
			t.Fatalf("settled round: %+v err=%v", settled, err)
		}
		if got := mustRun(t, f.env, child); got.Status != "cancelled" {
			t.Fatalf("child status %q, want cancelled", got.Status)
		}
		var pausedAfter int32
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT budget_paused_seconds FROM runs WHERE id=$1`, f.lead).Scan(&pausedAfter); err != nil {
			t.Fatal(err)
		}
		// A second custody loss must not credit or cancel again.
		assertEnded := func(gen int64) {
			t.Helper()
			latest := f.latest(t, gen)
			if latest.NextRoundEligible || latest.NextRound != nil || latest.FallbackReason != "interrupted" {
				t.Fatalf("generation %d: custody loss must end the check as interrupted: %+v", gen, latest)
			}
		}
		assertEnded(2)
		f.setGeneration(3)
		var pausedThird int32
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT budget_paused_seconds FROM runs WHERE id=$1`, f.lead).Scan(&pausedThird); err != nil || pausedThird != pausedAfter {
			t.Fatalf("wait credited again: after=%d third=%d err=%v", pausedAfter, pausedThird, err)
		}
		if pausedAfter < pausedBefore {
			t.Fatalf("wait credit went backwards: %d -> %d", pausedBefore, pausedAfter)
		}
		assertEnded(3)
		// The forged next round is refused and creates nothing.
		if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 3, f.c, 2); !errors.Is(err, ErrCrossCheckInterrupted) {
			t.Fatalf("forged round 2 after custody loss: %v", err)
		}
		if rows, children := f.counts(t); rows != 1 || children != 1 {
			t.Fatalf("rows=%d children=%d", rows, children)
		}
	})

	t.Run("custody loss past the deadline reads timed_out", func(t *testing.T) {
		f := newGatedFixture(t, 2)
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
		if err != nil {
			t.Fatal(err)
		}
		f.env.exec("UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE id=$1", cc.ID)
		f.setGeneration(2)
		latest := f.latest(t, 2)
		if latest.NextRoundEligible || latest.FallbackReason != "timed_out" {
			t.Fatalf("past the deadline: %+v", latest)
		}
		if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 2, f.c, 2); !errors.Is(err, ErrCrossCheckInterrupted) {
			t.Fatalf("forged round 2: %v", err)
		}
	})

	t.Run("custody loss on the last allowed round is interrupted, not revisions_exhausted", func(t *testing.T) {
		for _, mode := range []string{"gated", "required"} {
			f := newGatedFixture(t, 0)
			if mode == "required" {
				f.env.exec(`UPDATE runs SET plan_cross_check_gated=false, plan_cross_check_required=true, auto_approve=true WHERE id=$1`, f.lead)
			}
			if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1); err != nil {
				t.Fatal(err)
			}
			f.setGeneration(2)
			latest := f.latest(t, 2)
			want := "interrupted"
			if mode == "required" {
				// Today's autopilot behaviour, which this change must not touch.
				want = "revisions_exhausted"
			}
			if latest.NextRoundEligible || latest.FallbackReason != want {
				t.Fatalf("%s: %+v, want fallback %q", mode, latest, want)
			}
		}
	})

	t.Run("an unpresented approval is never re-checked and is not superseded by a forged round", func(t *testing.T) {
		f := newGatedFixture(t, 2)
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
		if err != nil {
			t.Fatal(err)
		}
		f.decide(t, cc, "approve")
		f.setGeneration(2)
		latest := f.latest(t, 2)
		if latest.NextRoundEligible || latest.NextRound != nil || latest.FallbackReason != "interrupted" {
			t.Fatalf("unpresented approval after reclaim: %+v", latest)
		}
		if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 2, f.c, 2); !errors.Is(err, ErrCrossCheckInterrupted) {
			t.Fatalf("forged round 2: %v", err)
		}
		// The recorded evidence is untouched: no SupersedeUnstoredPlanApproval on a gated lead.
		after, err := f.env.q.GetExactPlanCrossCheck(f.env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.lead, Round: 1})
		if err != nil || after.Verdict != "approve" {
			t.Fatalf("gated approve row was rewritten: %+v err=%v", after, err)
		}
		if rows, children := f.counts(t); rows != 1 || children != 1 {
			t.Fatalf("rows=%d children=%d", rows, children)
		}
	})

	t.Run("a decided REVISE followed by a reclaim starts round N+1 with a new deadline", func(t *testing.T) {
		f := newGatedFixture(t, 2)
		first, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
		if err != nil {
			t.Fatal(err)
		}
		f.decide(t, first, "revise")
		f.setGeneration(2)
		latest := f.latest(t, 2)
		if !latest.NextRoundEligible || latest.NextRound == nil || *latest.NextRound != 2 || latest.FallbackReason != "" {
			t.Fatalf("decided REVISE must allow round 2 after a reclaim: %+v", latest)
		}
		time.Sleep(20 * time.Millisecond)
		next := f.c
		next.PlanMd += "\n\n## Cross-check dispositions\nR1: fixed"
		second, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 2, next, 2)
		if err != nil {
			t.Fatal(err)
		}
		if second.Round != 2 || second.LeadClaimGeneration != 2 || second.CheckerRunID == first.CheckerRunID || !second.DeadlineAt.Time.After(first.DeadlineAt.Time) {
			t.Fatalf("round 2: %+v (round 1 deadline %v)", second, first.DeadlineAt.Time)
		}
		if rows, children := f.counts(t); rows != 2 || children != 2 {
			t.Fatalf("rows=%d children=%d", rows, children)
		}
	})

	t.Run("a presented gated gate never starts a first round again", func(t *testing.T) {
		f := newGatedFixture(t, 2)
		f.env.exec(`UPDATE runs SET plan_md='presented plan', gate_revision=1 WHERE id=$1`, f.lead)
		if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("round 1 after the gate was presented: %v", err)
		}
		if latest := f.latest(t, 1); latest.NextRoundEligible {
			t.Fatalf("latest offered a round after presentation: %+v", latest)
		}
		if rows, children := f.counts(t); rows != 0 || children != 0 {
			t.Fatalf("rows=%d children=%d", rows, children)
		}
	})
}

// gatedRowShape describes one settled cross_checks row to compare the two derivations on.
type gatedRowShape struct {
	name            string
	limit           int32
	round           int32
	rowGen, leadGen int64
	verdict, reason string
	// Offsets from now(); decided and interrupted may be absent.
	deadline          string
	decided, interrup string
}

func insertGatedShape(t *testing.T, f gatedFixture, s gatedRowShape) {
	t.Helper()
	digest, err := f.c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	insert := func(round int32, verdict string, reason any, decided, interrupted, deadline string, gen int64) {
		nullable := func(offset string) string {
			if offset == "" {
				return "NULL"
			}
			return "now()+interval '" + offset + "'"
		}
		f.env.exec(fmt.Sprintf(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
			required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,
			checker_harness,verdict,reason_class,findings,automatic_rounds_enabled,automatic_revision_limit,
			decided_at,interrupted_at,deadline_at)
			VALUES ($1,'plan',$2,$3,$4,$5,'{}','{}','s',$6,'',$7,'codex',$8,$9,
			'{"summary":"outcome","items":[]}',true,$10,%s,%s,%s)`, nullable(decided), nullable(interrupted), nullable(deadline)),
			f.lead, round, gen, f.c.PlanMd, f.c.Milestones, f.c.BaseCommit, digest, verdict, reason, s.limit)
	}
	for r := int32(1); r < s.round; r++ {
		insert(r, "revise", "revise", "-20 minutes", "", "-10 minutes", 1)
	}
	var reason any
	if s.reason != "" {
		reason = s.reason
	}
	insert(s.round, s.verdict, reason, s.decided, s.interrup, s.deadline, s.rowGen)
	f.setGeneration(s.leadGen)
}

// TestGatedPlanCrossCheckFallbackParityLiveDB pins the PRD #3708 R1 invariant: for every
// settled row shape, the fallback reason LatestPlanCrossCheck tells the worker equals the gate
// reason SetRunAwaitingApproval then derives, so a worker that opens the gate on the former
// never disagrees with the stored reason.
func TestGatedPlanCrossCheckFallbackParityLiveDB(t *testing.T) {
	shapes := []gatedRowShape{
		{name: "pending, lost custody before the deadline", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "pending", deadline: "10 minutes", interrup: "-1 minutes"},
		{name: "pending, lost custody after the deadline", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "pending", deadline: "-10 minutes", interrup: "-1 minutes"},
		{name: "pending, no interruption stamp", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "pending", deadline: "10 minutes"},
		{name: "approve of an older generation, decided in time", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "approve", reason: "approve", deadline: "10 minutes", decided: "-1 minutes"},
		{name: "approve of an older generation, decided after the deadline", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "approve", reason: "approve", deadline: "-10 minutes", decided: "-1 minutes"},
		{name: "superseded in time", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "failed", reason: "superseded", deadline: "10 minutes", decided: "-1 minutes", interrup: "-1 minutes"},
		{name: "superseded in time on the last round", limit: 0, round: 1, rowGen: 1, leadGen: 2, verdict: "failed", reason: "superseded", deadline: "10 minutes", decided: "-1 minutes", interrup: "-1 minutes"},
		{name: "superseded after the deadline", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "failed", reason: "superseded", deadline: "-10 minutes", decided: "-1 minutes", interrup: "-1 minutes"},
		{name: "approved_not_stored in time", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "failed", reason: "approved_not_stored", deadline: "10 minutes", decided: "-1 minutes", interrup: "-1 minutes"},
		{name: "approved_not_stored after the deadline", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "failed", reason: "approved_not_stored", deadline: "-10 minutes", decided: "-1 minutes", interrup: "-1 minutes"},
		{name: "approved_not_stored in time on the last round", limit: 1, round: 2, rowGen: 1, leadGen: 2, verdict: "failed", reason: "approved_not_stored", deadline: "10 minutes", decided: "-1 minutes", interrup: "-1 minutes"},
		{name: "decided block", limit: 2, round: 1, rowGen: 1, leadGen: 1, verdict: "block", reason: "block", deadline: "10 minutes", decided: "-1 minutes"},
		{name: "decided block of an older generation", limit: 2, round: 1, rowGen: 1, leadGen: 2, verdict: "block", reason: "block", deadline: "10 minutes", decided: "-1 minutes"},
		{name: "checker model error", limit: 2, round: 1, rowGen: 1, leadGen: 1, verdict: "failed", reason: "model_error", deadline: "10 minutes", decided: "-1 minutes"},
		{name: "checker timed out", limit: 2, round: 1, rowGen: 1, leadGen: 1, verdict: "failed", reason: "timed_out", deadline: "-1 minutes", decided: "-1 minutes"},
		{name: "decided revise on the last round", limit: 1, round: 2, rowGen: 1, leadGen: 1, verdict: "revise", reason: "revise", deadline: "10 minutes", decided: "-1 minutes"},
		{name: "decided revise on the last round of an older generation", limit: 1, round: 2, rowGen: 1, leadGen: 2, verdict: "revise", reason: "revise", deadline: "10 minutes", decided: "-1 minutes"},
	}
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			f := newGatedFixture(t, s.limit)
			insertGatedShape(t, f, s)
			latest := f.latest(t, s.leadGen)
			if latest.NextRoundEligible {
				t.Fatalf("a settled shape must not offer a round: %+v", latest)
			}
			plan, size, gen := f.c.PlanMd, "s", s.leadGen
			caps, tools, ms := []string{}, []string{}, []Milestone{}
			pid := uuid.New()
			req := StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &ms,
				RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, PresentationID: &pid}
			if _, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, req); err != nil || !applied {
				t.Fatalf("gate report: applied=%v err=%v", applied, err)
			}
			got := mustRun(t, f.env, f.lead).PlanCrossCheckGateReason.String
			if got != latest.FallbackReason || got == "" {
				t.Fatalf("LatestPlanCrossCheck.fallback_reason=%q but the gate derived %q", latest.FallbackReason, got)
			}
		})
	}
}

// TestGatedPlanCrossCheckApproveBindingLiveDB drives the digest-bound "passed" derivation
// through the report path, for an issue lead and for a prompt lead (which stores no milestones).
func TestGatedPlanCrossCheckApproveBindingLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "prompt"} {
		for _, tc := range []struct {
			name           string
			digest         func(stored []byte) string
			plan           func(stored string) string
			wantReason     string
			wantHistorical bool
		}{
			{"matching digest and plan", func(d []byte) string { return hex.EncodeToString(d) }, func(p string) string { return p }, "", false},
			{"no digest", func([]byte) string { return "" }, func(p string) string { return p }, "interrupted", false},
			{"mismatching digest", func(d []byte) string {
				wrong := append([]byte{}, d...)
				wrong[0] ^= 0xff
				return hex.EncodeToString(wrong)
			}, func(p string) string { return p }, "interrupted", false},
			{"right digest, different plan", func(d []byte) string { return hex.EncodeToString(d) }, func(p string) string { return p + "\nextra" }, "interrupted", true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				f := newGatedFixture(t, 2)
				if kind == "prompt" {
					f.env.exec(`UPDATE runs SET kind='prompt', issue_iid=NULL WHERE id=$1`, f.lead)
				}
				cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
				if err != nil {
					t.Fatal(err)
				}
				f.decide(t, cc, "approve")
				stored, err := f.env.q.GetExactPlanCrossCheck(f.env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.lead, Round: 1})
				if err != nil {
					t.Fatal(err)
				}
				plan, gen, size := tc.plan(stored.PlanMd.String), int64(1), "s"
				caps, tools, ms := []string{}, []string{}, []Milestone{}
				pid := uuid.New()
				req := StateRequest{State: "awaiting_approval", ClaimGeneration: &gen, PlanMd: &plan, Milestones: &ms,
					RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, PresentationID: &pid, CandidateDigest: tc.digest(stored.CandidateDigest)}
				if _, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, req); err != nil || !applied {
					t.Fatalf("gate report: applied=%v err=%v", applied, err)
				}
				run := mustRun(t, f.env, f.lead)
				if run.Status != "awaiting_approval" || run.AutoApprove {
					t.Fatalf("a gated approve never skips the human gate: status=%s auto=%v", run.Status, run.AutoApprove)
				}
				if run.PlanCrossCheckGateReason.String != tc.wantReason || run.PlanCrossCheckGateReason.Valid != (tc.wantReason != "") {
					t.Fatalf("gate reason %+v, want %q", run.PlanCrossCheckGateReason, tc.wantReason)
				}
				summary, err := f.svc.PlanCrossCheckSummary(f.env.ctx, f.w.UserID, f.lead)
				if err != nil || summary == nil || summary.Historical != tc.wantHistorical {
					t.Fatalf("summary=%+v err=%v, want historical=%v", summary, err, tc.wantHistorical)
				}
			})
		}
	}
}

// TestGatedPlanApprovalTriggerErrorMapsToStaleLiveDB makes the version-independent trigger
// refuse a legitimate-looking running report (the race the WHERE clause cannot see) and
// requires the same not-applied outcome as 0 rows: no error, the gate untouched.
func TestGatedPlanApprovalTriggerErrorMapsToStaleLiveDB(t *testing.T) {
	f := newGateFix(t, 3)
	run := f.newRun("issue")
	f.exec(`UPDATE runs SET plan_cross_check_gated=true WHERE id=$1`, run)
	f.mustPublish(run, gateReq(i64(1), uid()), 1)
	f.exec(`UPDATE workers SET last_heartbeat_at=now(), status='online' WHERE id=$1`, f.wkr.ID)
	if _, err := f.submit(run, "approve_plan", nil, SubmitInputOptions{ExpectedGateRevision: i64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ConsumeInputs(f.ctx, f.wkr, run); err != nil {
		t.Fatal(err)
	}
	var inputID int64
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM run_user_inputs WHERE run_id=$1 AND kind='approve_plan'`, run).Scan(&inputID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AckInputs(f.ctx, f.wkr, run, 1, []int64{inputID}); err != nil {
		t.Fatal(err)
	}
	// Simulate the race: an extra trigger raises the gated approval error for this run only.
	fn := "zz_gated_race_" + strings.ReplaceAll(run.String(), "-", "")
	f.exec(fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		RAISE EXCEPTION 'simulated approval race' USING ERRCODE = 'check_violation', CONSTRAINT = 'runs_gated_plan_approval'; END $$`, fn))
	f.exec(fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE OF status ON runs FOR EACH ROW
		WHEN (NEW.id = '%s' AND OLD.status = 'awaiting_approval' AND NEW.status = 'running') EXECUTE FUNCTION %s()`, fn, run, fn))
	t.Cleanup(func() {
		f.exec(fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON runs`, fn))
		f.exec(fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, fn))
	})
	gen := int64(1)
	got, applied, err := f.svc.SetState(f.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: &gen})
	if err != nil || applied {
		t.Fatalf("the trigger refusal must be a not-applied result, got applied=%v err=%v", applied, err)
	}
	if got.Status != "awaiting_approval" {
		t.Fatalf("run left the gate: %s", got.Status)
	}
	if row := f.row(run); row.status != "awaiting_approval" || row.revision != 1 {
		t.Fatalf("gate changed: %+v", row)
	}
}
