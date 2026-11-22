package workersvc

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4554 live-DB coverage: a checked plan whose post-scrub milestone list is not
// exactly [{id,title}] objects passing validateMilestones is refused at cross-check
// admission, before any cross_checks row or checker child exists, and a pre-fix attempt that
// already stores such a list cannot be resubmitted. The running-write proofs below are
// pre-existing fail-closed invariants kept for rollout, not proofs of the admission check.

var invalidMilestoneLists = []struct{ name, raw string }{
	{"blank id and title", `[{"id":"","title":""}]`},
	{"extra key", `[{"id":"m1","title":"t","extra":1}]`},
	{"missing title", `[{"id":"m1"}]`},
}

type ccAdmissionRow struct {
	PlanMd, Milestones, Digest, Checker string
	Round                               int32
}

func ccAdmissionSnapshot(t *testing.T, f gatedFixture, round int32) ccAdmissionRow {
	t.Helper()
	var r ccAdmissionRow
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT plan_md, milestones::text, encode(candidate_digest,'hex'), checker_run_id::text, round
		FROM cross_checks WHERE lead_run_id=$1 AND round=$2`, f.lead, round).Scan(&r.PlanMd, &r.Milestones, &r.Digest, &r.Checker, &r.Round); err != nil {
		t.Fatal(err)
	}
	return r
}

func ccRunningRequest(cc store.CrossCheck, plan string, ms []Milestone) StateRequest {
	generation := int64(1)
	caps := []string{}
	tools := []string{}
	size := "s"
	return StateRequest{State: "running", ClaimGeneration: &generation, PlanMd: &plan, Milestones: &ms,
		RequiredCapabilities: &caps, RequiredTools: &tools, SizeClass: &size, CandidateDigest: hex.EncodeToString(cc.CandidateDigest)}
}

func ccAutomaticFixture(t *testing.T, limit int32) gatedFixture {
	t.Helper()
	env, svc, w, lead, c := newAutomaticRoundsFixture(t, limit)
	t.Cleanup(func() { env.exec(`DELETE FROM users WHERE id=$1`, w.UserID) })
	env.exec(`UPDATE runs SET plan_source='agent' WHERE id=$1`, lead)
	return gatedFixture{env: env, svc: svc, w: w, lead: lead, c: c}
}

// ccValidSubmitSucceeds submits a valid milestone list on the same fixture after a refusal,
// tying the refusal to the milestones rather than to the fixture.
func ccValidSubmitSucceeds(t *testing.T, f gatedFixture) {
	t.Helper()
	c := f.c
	c.Milestones = json.RawMessage(`[{"id":"m1","title":"First"}]`)
	if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1); err != nil {
		t.Fatalf("valid milestones refused on the same fixture: %v", err)
	}
	if rows, children := f.counts(t); rows != 1 || children != 1 {
		t.Fatalf("valid submit created rows=%d children=%d", rows, children)
	}
}

// ccPreFixDigest returns the candidate digest the pre-fix service would have stored for the
// fixture's candidate carrying the given canonical milestone bytes. It rebuilds the
// service-side candidate (normalised, capability/tool sets sorted, inferred-capability
// policy applied) and is sanity-checked against the digest of the real, valid round-1 row.
func ccPreFixDigest(t *testing.T, f gatedFixture, valid store.CrossCheck, milestones []byte) []byte {
	t.Helper()
	cand, err := NormalizePlanCrossCheckCandidate(f.c)
	if err != nil {
		t.Fatal(err)
	}
	cand.RequiredCapabilities = applyInferredCapabilityPolicy(mustRun(t, f.env, f.lead), sortedSet(cand.RequiredCapabilities))
	cand.RequiredTools = sortedSet(cand.RequiredTools)
	if got, err := cand.Digest(); err != nil || hex.EncodeToString(got) != hex.EncodeToString(valid.CandidateDigest) {
		t.Fatalf("digest helper disagrees with the service: got %x err=%v want %x", got, err, valid.CandidateDigest)
	}
	cand.Milestones = milestones
	d, err := cand.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestCrossCheckMilestoneAdmissionRefusesWithoutRowOrChildLiveDB(t *testing.T) {
	for _, tc := range invalidMilestoneLists {
		t.Run("automatic "+tc.name, func(t *testing.T) {
			f := ccAutomaticFixture(t, 2)
			c := f.c
			c.Milestones = json.RawMessage(tc.raw)
			if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("invalid milestones admitted: %v", err)
			}
			if rows, children := f.counts(t); rows != 0 || children != 0 {
				t.Fatalf("refused submit created rows=%d children=%d", rows, children)
			}
			ccValidSubmitSucceeds(t, f)
		})
		t.Run("gated "+tc.name, func(t *testing.T) {
			f := newGatedFixture(t, 2)
			c := f.c
			c.Milestones = json.RawMessage(tc.raw)
			if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("invalid milestones admitted in gated mode: %v", err)
			}
			if rows, children := f.counts(t); rows != 0 || children != 0 {
				t.Fatalf("refused gated submit created rows=%d children=%d", rows, children)
			}
			ccValidSubmitSucceeds(t, f)
		})
	}
}

func TestCrossCheckMilestoneAdmissionPreFixAttemptLiveDB(t *testing.T) {
	for _, tc := range invalidMilestoneLists {
		t.Run(tc.name, func(t *testing.T) {
			f := ccAutomaticFixture(t, 2)
			cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
			if err != nil {
				t.Fatal(err)
			}
			f.decide(t, cc, "revise")
			// Simulate an attempt admitted before the fix: the row stores the invalid list in
			// canonical form (as the pre-fix normaliser produced it) and the digest of that
			// same candidate, so on the pre-fix service a same-round resubmit is a clean replay.
			var generic any
			if err := json.Unmarshal([]byte(tc.raw), &generic); err != nil {
				t.Fatal(err)
			}
			canonical, err := json.Marshal(generic)
			if err != nil {
				t.Fatal(err)
			}
			digest := ccPreFixDigest(t, f, cc, canonical)
			f.env.exec(`UPDATE cross_checks SET milestones=$2::jsonb, candidate_digest=$3 WHERE id=$1`, cc.ID, string(canonical), digest)
			before := ccAdmissionSnapshot(t, f, 1)

			c := f.c
			c.Milestones = json.RawMessage(tc.raw)
			for _, round := range []int32{1, 2} {
				_, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, round)
				if !errors.Is(err, ErrCrossCheckRefused) || errors.Is(err, ErrCrossCheckInterrupted) {
					t.Fatalf("round %d resubmission of invalid list: want admission refusal, got %v", round, err)
				}
			}
			if rows, children := f.counts(t); rows != 1 || children != 1 {
				t.Fatalf("resubmission created rows=%d children=%d", rows, children)
			}
			if after := ccAdmissionSnapshot(t, f, 1); after != before {
				t.Fatalf("immutable columns changed: before=%+v after=%+v", before, after)
			}

			// The status read still returns the attempt.
			status, _, err := f.svc.PlanCrossCheckStatus(f.env.ctx, f.w, f.lead, 1, 1)
			if err != nil || status.ID != cc.ID {
				t.Fatalf("status of pre-fix attempt: id=%v err=%v", status.ID, err)
			}
			f.latest(t, 1)

			// A running checked-plan write carrying the invalid list fails closed. This is a
			// pre-existing invariant kept for rollout, not a proof of the admission check: for
			// the extra-key case the typed decode drops the key and the refusal comes from the
			// plan-write SQL matching no row.
			var invalid []Milestone
			if err := json.Unmarshal([]byte(tc.raw), &invalid); err != nil {
				t.Fatal(err)
			}
			_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(cc, "plan", invalid))
			if applied || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("running write with invalid list: applied=%v err=%v", applied, err)
			}
			if run := mustRun(t, f.env, f.lead); run.PlanMd.Valid || len(run.MilestonesFrozen) != 0 && string(run.MilestonesFrozen) != "null" {
				t.Fatalf("refused write persisted: plan=%v milestones=%s", run.PlanMd, run.MilestonesFrozen)
			}
		})
	}
}

// An APPROVED pre-fix attempt storing an invalid list, with the worker presenting that
// attempt's own stored digest, still cannot be written as a running plan: the write fails
// closed with ErrInvalidState and persists nothing. Like the running-write block above, this
// is a pre-existing invariant kept for rollout (it also holds on the pre-fix service), not a
// proof of the admission check. The digest matches by construction; the refusing check is
// the Go milestone validation for "blank id and title" (and "missing title"), and only the
// SetRunAutopilotPlan cc.milestones equality in SQL for "extra key", whose typed decode drops
// the key.
func TestCrossCheckMilestoneAdmissionApprovedPreFixAttemptLiveDB(t *testing.T) {
	for _, tc := range invalidMilestoneLists {
		t.Run(tc.name, func(t *testing.T) {
			f := ccAutomaticFixture(t, 2)
			cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
			if err != nil {
				t.Fatal(err)
			}
			f.decide(t, cc, "approve")
			var generic any
			if err := json.Unmarshal([]byte(tc.raw), &generic); err != nil {
				t.Fatal(err)
			}
			canonical, err := json.Marshal(generic)
			if err != nil {
				t.Fatal(err)
			}
			digest := ccPreFixDigest(t, f, cc, canonical)
			f.env.exec(`UPDATE cross_checks SET milestones=$2::jsonb, candidate_digest=$3 WHERE id=$1`, cc.ID, string(canonical), digest)
			var invalid []Milestone
			if err := json.Unmarshal([]byte(tc.raw), &invalid); err != nil {
				t.Fatal(err)
			}
			stored := cc
			stored.CandidateDigest = digest
			_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(stored, "plan", invalid))
			if applied || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("running write of approved pre-fix attempt: applied=%v err=%v", applied, err)
			}
			if run := mustRun(t, f.env, f.lead); run.PlanMd.Valid || len(run.MilestonesFrozen) != 0 && string(run.MilestonesFrozen) != "null" {
				t.Fatalf("refused write persisted: plan=%v milestones=%s", run.PlanMd, run.MilestonesFrozen)
			}
			// Control: the same approved attempt restored to its valid list and digest writes.
			f.env.exec(`UPDATE cross_checks SET milestones='[]'::jsonb, candidate_digest=$2 WHERE id=$1`, cc.ID, cc.CandidateDigest)
			if _, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(cc, "plan", []Milestone{})); err != nil || !applied {
				t.Fatalf("control running write: applied=%v err=%v", applied, err)
			}
		})
	}
}

func decodeMilestonesForTest(t *testing.T, raw []byte) []Milestone {
	t.Helper()
	var out []Milestone
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

func assertStoredCheckedPlan(t *testing.T, f gatedFixture, plan string, want []Milestone, cc store.CrossCheck) {
	t.Helper()
	run := mustRun(t, f.env, f.lead)
	if run.PlanMd.String != plan {
		t.Fatalf("stored plan %q, want %q", run.PlanMd.String, plan)
	}
	got := decodeMilestonesForTest(t, run.MilestonesFrozen)
	if len(got) != len(want) {
		t.Fatalf("stored milestones %s, want %v", run.MilestonesFrozen, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("stored milestone %d = %v, want %v", i, got[i], want[i])
		}
	}
	if len(want) == 0 && string(run.MilestonesFrozen) == "null" {
		t.Fatal("empty list stored as null")
	}
	var digest string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT encode(candidate_digest,'hex') FROM cross_checks WHERE id=$1`, cc.ID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != hex.EncodeToString(cc.CandidateDigest) || digest == "" {
		t.Fatalf("bound digest %q, want %x", digest, cc.CandidateDigest)
	}
}

func TestCrossCheckMilestoneAdmissionValidControlsLiveDB(t *testing.T) {
	t.Run("milestone-free mr_rework run", func(t *testing.T) {
		f := ccAutomaticFixture(t, 2)
		// runs_kind_shape: an mr_rework row needs a pipeline_ref, mr_iid and a source run.
		f.env.exec(`UPDATE runs SET kind='mr_rework', pipeline_ref='agent/issue-2150', mr_iid=7, target_run_id=id WHERE id=$1`, f.lead)
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, f.c, 1)
		if err != nil {
			t.Fatal(err)
		}
		f.decide(t, cc, "approve")
		_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(cc, "plan", []Milestone{}))
		if err != nil || !applied {
			t.Fatalf("running write: applied=%v err=%v", applied, err)
		}
		assertStoredCheckedPlan(t, f, "plan", nil, cc)
	})

	t.Run("issue run with a nonempty list", func(t *testing.T) {
		f := ccAutomaticFixture(t, 2)
		want := []Milestone{{ID: "m1", Title: "First"}, {ID: "m2", Title: "Second"}}
		c := f.c
		c.Milestones = json.RawMessage(`[{"id":"m1","title":"First"},{"id":"m2","title":"Second"}]`)
		cc, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1)
		if err != nil {
			t.Fatal(err)
		}
		f.decide(t, cc, "approve")
		_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(cc, "plan", want))
		if err != nil || !applied {
			t.Fatalf("running write: applied=%v err=%v", applied, err)
		}
		assertStoredCheckedPlan(t, f, "plan", want, cc)
	})

	t.Run("later allowed round", func(t *testing.T) {
		f := ccAutomaticFixture(t, 2)
		final := []Milestone{{ID: "m1", Title: "First"}, {ID: "m2", Title: "Second"}}
		c1 := f.c
		c1.Milestones = json.RawMessage(`[{"id":"m1","title":"First"}]`)
		cc1, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c1, 1)
		if err != nil {
			t.Fatal(err)
		}
		f.decide(t, cc1, "revise")

		// An invalid round-2 candidate is refused and leaves round 1 alone.
		bad := f.c
		bad.Milestones = json.RawMessage(`[{"id":"","title":""}]`)
		if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, bad, 2); !errors.Is(err, ErrCrossCheckRefused) {
			t.Fatalf("invalid round 2: %v", err)
		}
		if rows, children := f.counts(t); rows != 1 || children != 1 {
			t.Fatalf("invalid round 2 created rows=%d children=%d", rows, children)
		}

		c2 := f.c
		c2.PlanMd = "revised plan"
		c2.Milestones = json.RawMessage(`[{"id":"m1","title":"First"},{"id":"m2","title":"Second"}]`)
		cc2, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c2, 2)
		if err != nil || cc2.Round != 2 {
			t.Fatalf("round 2: round=%d err=%v", cc2.Round, err)
		}
		f.decide(t, cc2, "approve")
		_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(cc2, "revised plan", final))
		if err != nil || !applied {
			t.Fatalf("running write: applied=%v err=%v", applied, err)
		}
		assertStoredCheckedPlan(t, f, "revised plan", final, cc2)
		if uuid.UUID(cc1.CheckerRunID.Bytes) == uuid.UUID(cc2.CheckerRunID.Bytes) {
			t.Fatal("round 2 reused round 1 checker")
		}
	})
}
