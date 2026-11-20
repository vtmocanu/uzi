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
// admission, before any cross_checks row or checker child exists, and a pre-fix attempt
// that already stores such a list can neither be resubmitted nor written as a running plan.

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

// ccApprove decides the round's checker child with the given verdict through the real service.
func ccApprove(t *testing.T, f gatedFixture, cc store.CrossCheck, verdict string) {
	t.Helper()
	f.decide(t, cc, verdict)
}

func ccRunningRequest(t *testing.T, cc store.CrossCheck, plan string, ms []Milestone) StateRequest {
	t.Helper()
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
			ccApprove(t, f, cc, "approve")
			// Simulate an attempt admitted before the fix: the stored milestones are invalid.
			f.env.exec(`UPDATE cross_checks SET milestones=$2::jsonb WHERE id=$1`, cc.ID, tc.raw)
			before := ccAdmissionSnapshot(t, f, 1)

			c := f.c
			c.Milestones = json.RawMessage(tc.raw)
			if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 1); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("same-round resubmission of invalid list: %v", err)
			}
			if _, err := f.svc.SubmitPlanCrossCheck(f.env.ctx, f.w, f.lead, 1, c, 2); !errors.Is(err, ErrCrossCheckRefused) {
				t.Fatalf("round 2 with invalid list: %v", err)
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

			// A running checked-plan write carrying the invalid list fails closed.
			var invalid []Milestone
			if err := json.Unmarshal([]byte(tc.raw), &invalid); err != nil {
				t.Fatal(err)
			}
			_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(t, cc, "plan", invalid))
			if applied || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("running write with invalid list: applied=%v err=%v", applied, err)
			}
			if run := mustRun(t, f.env, f.lead); run.PlanMd.Valid || len(run.MilestonesFrozen) != 0 && string(run.MilestonesFrozen) != "null" {
				t.Fatalf("refused write persisted: plan=%v milestones=%s", run.PlanMd, run.MilestonesFrozen)
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
		ccApprove(t, f, cc, "approve")
		_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(t, cc, "plan", []Milestone{}))
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
		ccApprove(t, f, cc, "approve")
		_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(t, cc, "plan", want))
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
		ccApprove(t, f, cc1, "revise")

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
		ccApprove(t, f, cc2, "approve")
		_, applied, err := f.svc.SetState(f.env.ctx, f.w, f.lead, ccRunningRequest(t, cc2, "revised plan", final))
		if err != nil || !applied {
			t.Fatalf("running write: applied=%v err=%v", applied, err)
		}
		assertStoredCheckedPlan(t, f, "revised plan", final, cc2)
		if uuid.UUID(cc1.CheckerRunID.Bytes) == uuid.UUID(cc2.CheckerRunID.Bytes) {
			t.Fatal("round 2 reused round 1 checker")
		}
	})
}
