package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

// Issue #4616 publication backstop: a ci_fix / mr_rework run whose bound MR is merged or closed
// is stopped (stop_kind=cancelled, server reason, no cancel input) when it asks for its
// publication context, and its later failed report finalizes as cancelled rather than as an
// agent failure.

const (
	termMergedReason = "target MR merged; further publication stopped."
	termClosedReason = "target MR closed; further publication stopped."
)

type termErrForge struct {
	forgetest.BaseFake
	err error
}

func (f *termErrForge) GetMergeRequestSummary(context.Context, int64, int64) (forge.MergeRequestSummary, error) {
	return forge.MergeRequestSummary{}, f.err
}

type termPub struct {
	f      sourcePublicationFixture
	kind   string
	id     uuid.UUID
	branch string
	proof  *contextPublicationForge
}

// newTermPub seeds a completed source publication for MR 7 and a RUNNING request run of the
// given kind (bound to that MR) owned by the fixture's worker at generation 1, with the forge
// reporting the MR in the given state. The request carries a stale failure_reason and fail_origin
// so a cancel's clearing of both is observable.
func newTermPub(t *testing.T, kind, state string) *termPub {
	t.Helper()
	f := seedSourcePublication(t, false)
	if r, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req); err != nil || !r.Applied {
		t.Fatalf("source completion: %+v %v", r, err)
	}
	branch := *f.req.Branch
	id := uuid.New()
	if kind == "ci_fix" {
		f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,pipeline_id,ci_fix_mr_iid,issue_title,issue_description,failure_reason,fail_origin)
 VALUES($1,$2,$3,'ci_fix','running',$4,1,$5,123,7,'term','','stale failure','agent_failure')`, id, f.w.UserID, f.e.repoID, f.w.ID, branch)
	} else {
		f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description,failure_reason,fail_origin)
 VALUES($1,$2,$3,'mr_rework','running',$4,1,$5,7,$6,'term','','stale failure','agent_failure')`, id, f.w.UserID, f.e.repoID, f.w.ID, branch, f.id)
	}
	p := &termPub{f: f, kind: kind, id: id, branch: branch}
	p.proof = &contextPublicationForge{summary: forge.MergeRequestSummary{IID: 7, State: state, SourceBranch: branch, HeadSHA: strings.Repeat("b", 40)}}
	f.svc.SetForges(settleUnitBuilder{f: p.proof})
	return p
}

func (p *termPub) resolve() PublicationContextResponse {
	return p.f.svc.ResolvePublicationContext(p.f.e.ctx, p.f.w, p.id,
		PublicationContextRequest{ClaimGeneration: 1, Branch: p.branch, CandidateHead: strings.Repeat("c", 40)})
}

type termRow struct {
	Status        string
	StopKind      string
	StopReason    string
	FailureReason string
	FailOrigin    string
	Inputs        int
}

func (p *termPub) row(t *testing.T) termRow {
	t.Helper()
	var r termRow
	err := p.f.e.pool.QueryRow(p.f.e.ctx, `SELECT status, coalesce(stop_kind,''), coalesce(stop_reason,''), coalesce(failure_reason,''), coalesce(fail_origin,''),
 (SELECT count(*) FROM run_user_inputs WHERE run_id=$1) FROM runs WHERE id=$1`, p.id).
		Scan(&r.Status, &r.StopKind, &r.StopReason, &r.FailureReason, &r.FailOrigin, &r.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// reportFailed sends the worker's failed report (generation 1) carrying its own failure text.
func (p *termPub) reportFailed(t *testing.T, text string) {
	t.Helper()
	gen := int64(1)
	origin := "agent_failure"
	res, err := p.f.svc.SetStateReportWithReconciliation(p.f.e.ctx, p.f.w, p.id,
		StateRequest{State: "failed", ClaimGeneration: &gen, FailureReason: &text, FailOrigin: &origin})
	if err != nil || !res.Applied {
		t.Fatalf("failed report: %+v %v", res, err)
	}
}

func TestPublicationContextTerminalTargetStopsRunLiveDB(t *testing.T) {
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		for _, state := range []string{forge.MRStateMerged, forge.MRStateClosed} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				p := newTermPub(t, kind, state)
				want := termMergedReason
				if state == forge.MRStateClosed {
					want = termClosedReason
				}
				wantUnknown(t, p.resolve(), "target_terminal")
				r := p.row(t)
				if r.Status != "running" || r.StopKind != "cancelled" || r.StopReason != want || r.FailureReason != "" || r.FailOrigin != "" || r.Inputs != 0 {
					t.Fatalf("after stamp: %+v", r)
				}
				// The same answer again, and no second write or input.
				wantUnknown(t, p.resolve(), "forge_mismatch")
				if again := p.row(t); again != r {
					t.Fatalf("second resolve changed the run: %+v -> %+v", r, again)
				}
				p.reportFailed(t, "worker says the publish failed")
				r = p.row(t)
				if r.Status != "cancelled" || r.StopKind != "cancelled" || r.StopReason != want || r.FailureReason != "" || r.FailOrigin != "" {
					t.Fatalf("after failed report: %+v", r)
				}
			})
		}
	}
}

func TestPublicationContextTerminalTargetNegativeControlsLiveDB(t *testing.T) {
	type tc struct {
		name   string
		mutate func(t *testing.T, p *termPub)
		reason string
	}
	cases := []tc{
		{"wrong iid", func(_ *testing.T, p *termPub) { p.proof.summary.IID = 8 }, "forge_mismatch"},
		{"wrong branch", func(_ *testing.T, p *termPub) { p.proof.summary.SourceBranch = "agent/other" }, "forge_mismatch"},
		{"unsafe branch", func(_ *testing.T, p *termPub) { p.proof.summary.SourceBranch = "bad\nbranch" }, "forge_mismatch"},
		{"malformed sha", func(_ *testing.T, p *termPub) { p.proof.summary.HeadSHA = "not-a-sha" }, "forge_mismatch"},
		{"locked", func(_ *testing.T, p *termPub) { p.proof.summary.State = forge.MRStateLocked }, "forge_mismatch"},
		{"unknown state", func(_ *testing.T, p *termPub) { p.proof.summary.State = "weird" }, "forge_mismatch"},
		{"empty state", func(_ *testing.T, p *termPub) { p.proof.summary.State = "" }, "forge_mismatch"},
		{"forge error", func(_ *testing.T, p *termPub) {
			p.f.svc.SetForges(settleUnitBuilder{f: &termErrForge{err: errors.New("dial tcp: refused")}})
		}, "forge_unknown"},
		{"forge 404", func(_ *testing.T, p *termPub) {
			p.f.svc.SetForges(settleUnitBuilder{f: &termErrForge{err: errors.New("404 merge request not found")}})
		}, "forge_unknown"},
		{"generation changes at the barrier", func(t *testing.T, p *termPub) {
			p.proof.barrier = func() { p.f.e.exec(t, "UPDATE runs SET claim_generation=2 WHERE id=$1", p.id) }
		}, "claim_changed"},
		{"claim released at the barrier", func(t *testing.T, p *termPub) {
			p.proof.barrier = func() { p.f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", p.id) }
		}, "claim_changed"},
		{"already terminal", func(t *testing.T, p *termPub) {
			p.f.e.exec(t, "UPDATE runs SET status='failed' WHERE id=$1", p.id)
		}, "claim_changed"},
		{"source changes at the barrier", func(t *testing.T, p *termPub) {
			p.proof.barrier = func() { p.f.e.exec(t, "UPDATE runs SET completion_non_closing_reason='partial' WHERE id=$1", p.f.id) }
		}, "authority_changed"},
		{"operator stop already set", func(t *testing.T, p *termPub) {
			p.f.e.exec(t, "UPDATE runs SET stop_kind='stopped', stop_reason='operator wind-down' WHERE id=$1", p.id)
		}, "forge_mismatch"},
		{"operator cancel with its own reason", func(t *testing.T, p *termPub) {
			p.f.e.exec(t, "UPDATE runs SET stop_kind='cancelled', stop_reason='operator reason' WHERE id=$1", p.id)
		}, "forge_mismatch"},
	}
	for _, kind := range []string{"ci_fix", "mr_rework"} {
		for _, c := range cases {
			t.Run(kind+"/"+c.name, func(t *testing.T) {
				p := newTermPub(t, kind, forge.MRStateMerged)
				c.mutate(t, p)
				before := p.row(t)
				wantUnknown(t, p.resolve(), c.reason)
				after := p.row(t)
				if after != before {
					t.Fatalf("a refused resolve changed the run: %+v -> %+v", before, after)
				}
			})
		}
	}
}

// An operator's own stop keeps its reason and its arm after the refused backstop: the failed
// report finalizes the run as the operator asked, never as agent_failure and never with the
// server's reason.
func TestPublicationContextTerminalTargetKeepsOperatorOutcomeLiveDB(t *testing.T) {
	for _, stopKind := range []string{"stopped", "cancelled"} {
		t.Run(stopKind, func(t *testing.T) {
			p := newTermPub(t, "ci_fix", forge.MRStateClosed)
			p.f.e.exec(t, "UPDATE runs SET stop_kind=$2, stop_reason='operator reason' WHERE id=$1", p.id, stopKind)
			wantUnknown(t, p.resolve(), "forge_mismatch")
			p.reportFailed(t, "worker text")
			r := p.row(t)
			if r.Status != "cancelled" || r.StopKind != stopKind || r.StopReason != "operator reason" || r.FailOrigin == "agent_failure" {
				t.Fatalf("operator outcome lost: %+v", r)
			}
		})
	}
}

func TestPublicationContextTerminalStopReconciliationLiveDB(t *testing.T) {
	faultAt := func(p *termPub, phase string, attempts ...int) {
		p.f.svc.terminalStopFault = func(got string, attempt int) error {
			if got != phase {
				return nil
			}
			for _, a := range attempts {
				if a == attempt {
					return errors.New("injected " + phase)
				}
			}
			return nil
		}
	}
	t.Run("lost commit response", func(t *testing.T) {
		p := newTermPub(t, "ci_fix", forge.MRStateMerged)
		faultAt(p, terminalStopFaultAfterCommit, 0)
		wantUnknown(t, p.resolve(), "target_terminal")
		if r := p.row(t); r.StopKind != "cancelled" || r.StopReason != termMergedReason || r.Inputs != 0 {
			t.Fatalf("stamp: %+v", r)
		}
		p.reportFailed(t, "worker text")
		if r := p.row(t); r.Status != "cancelled" || r.FailureReason != "" || r.FailOrigin != "" {
			t.Fatalf("finalize: %+v", r)
		}
	})
	t.Run("definite rollback then restamp", func(t *testing.T) {
		p := newTermPub(t, "mr_rework", forge.MRStateClosed)
		faultAt(p, terminalStopFaultBeforeCommit, 0)
		wantUnknown(t, p.resolve(), "target_terminal")
		if r := p.row(t); r.StopKind != "cancelled" || r.StopReason != termClosedReason {
			t.Fatalf("restamp: %+v", r)
		}
		p.reportFailed(t, "worker text")
		if r := p.row(t); r.Status != "cancelled" {
			t.Fatalf("finalize: %+v", r)
		}
	})
	t.Run("transient failure until the third reconciliation attempt", func(t *testing.T) {
		p := newTermPub(t, "ci_fix", forge.MRStateMerged)
		faultAt(p, terminalStopFaultStamp, 0, 1, 2)
		wantUnknown(t, p.resolve(), "target_terminal")
		if r := p.row(t); r.StopKind != "cancelled" || r.StopReason != termMergedReason {
			t.Fatalf("stamp: %+v", r)
		}
		p.reportFailed(t, "worker text")
		if r := p.row(t); r.Status != "cancelled" {
			t.Fatalf("finalize: %+v", r)
		}
	})
	t.Run("every attempt fails", func(t *testing.T) {
		p := newTermPub(t, "ci_fix", forge.MRStateMerged)
		faultAt(p, terminalStopFaultStamp, 0, 1, 2, 3)
		before := p.row(t)
		wantUnknown(t, p.resolve(), "authority_unknown")
		if after := p.row(t); after != before {
			t.Fatalf("an unreconciled stop wrote: %+v -> %+v", before, after)
		}
	})
	t.Run("operator cancel lands during reconciliation", func(t *testing.T) {
		p := newTermPub(t, "ci_fix", forge.MRStateMerged)
		p.f.svc.terminalStopFault = func(phase string, attempt int) error {
			switch {
			case phase == terminalStopFaultBeforeCommit && attempt == 0:
				return errors.New("injected rollback")
			case phase == terminalStopFaultReconcileBegin && attempt == 1:
				p.f.e.exec(t, "UPDATE runs SET stop_kind='cancelled', stop_reason='operator reason' WHERE id=$1", p.id)
			}
			return nil
		}
		wantUnknown(t, p.resolve(), "forge_mismatch")
		if r := p.row(t); r.StopKind != "cancelled" || r.StopReason != "operator reason" || r.Inputs != 0 {
			t.Fatalf("operator verdict overwritten: %+v", r)
		}
	})
	t.Run("operator stop lands during reconciliation", func(t *testing.T) {
		p := newTermPub(t, "mr_rework", forge.MRStateClosed)
		p.f.svc.terminalStopFault = func(phase string, attempt int) error {
			switch {
			case phase == terminalStopFaultBeforeCommit && attempt == 0:
				return errors.New("injected rollback")
			case phase == terminalStopFaultReconcileBegin && attempt == 1:
				p.f.e.exec(t, "UPDATE runs SET stop_kind='stopped', stop_reason='operator wind-down' WHERE id=$1", p.id)
			}
			return nil
		}
		wantUnknown(t, p.resolve(), "forge_mismatch")
		if r := p.row(t); r.StopKind != "stopped" || r.StopReason != "operator wind-down" {
			t.Fatalf("operator stop overwritten: %+v", r)
		}
	})
	t.Run("source changes during reconciliation", func(t *testing.T) {
		p := newTermPub(t, "ci_fix", forge.MRStateMerged)
		p.f.svc.terminalStopFault = func(phase string, attempt int) error {
			switch {
			case phase == terminalStopFaultBeforeCommit && attempt == 0:
				return errors.New("injected rollback")
			case phase == terminalStopFaultReconcileBegin && attempt == 1:
				p.f.e.exec(t, "UPDATE runs SET completion_non_closing_reason='partial' WHERE id=$1", p.f.id)
			}
			return nil
		}
		wantUnknown(t, p.resolve(), "authority_changed")
		if r := p.row(t); r.StopKind != "" || r.StopReason != "" || r.Inputs != 0 {
			t.Fatalf("stamped after the source changed: %+v", r)
		}
	})
	t.Run("generation changes during reconciliation", func(t *testing.T) {
		p := newTermPub(t, "mr_rework", forge.MRStateMerged)
		p.f.svc.terminalStopFault = func(phase string, attempt int) error {
			switch {
			case phase == terminalStopFaultBeforeCommit && attempt == 0:
				return errors.New("injected rollback")
			case phase == terminalStopFaultReconcileBegin && attempt == 1:
				p.f.e.exec(t, "UPDATE runs SET claim_generation=2 WHERE id=$1", p.id)
			}
			return nil
		}
		wantUnknown(t, p.resolve(), "claim_changed")
		if r := p.row(t); r.StopKind != "" {
			t.Fatalf("stamped a reclaimed run: %+v", r)
		}
	})
}
