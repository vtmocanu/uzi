package workersvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

// ciBindingEnv is a branch whose history holds an old merged PR (oldMR) and the current
// open PR (currentMR, the fixture's completed source), plus a running ci_fix request.
type ciBindingEnv struct {
	f         sourcePublicationFixture
	branch    string
	requestID uuid.UUID
	oldID     uuid.UUID
	proof     *contextPublicationForge
}

const (
	ciBindingOldMR     = int64(5)
	ciBindingCurrentMR = int64(7) // the mr_iid seedSourcePublication completes the source with
)

func (c *ciBindingEnv) insertRoot(t *testing.T, kind, status string, mr *int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	iid := *c.f.e.nextIID
	*c.f.e.nextIID++
	c.f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,branch,mr_iid,issue_iid,issue_title,issue_description)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,'old','')`, id, c.f.w.UserID, c.f.e.repoID, kind, status, c.branch, mr, iid)
	return id
}

func (c *ciBindingEnv) insertRework(t *testing.T, target uuid.UUID, mr *int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	c.f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description)
 VALUES($1,$2,$3,'mr_rework','completed',$4,$5,$6,'rework','')`, id, c.f.w.UserID, c.f.e.repoID, c.branch, mr, target)
	return id
}

func seedCIBinding(t *testing.T, bound *int64) *ciBindingEnv {
	t.Helper()
	f := seedSourcePublication(t, false)
	if r, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req); err != nil || !r.Applied {
		t.Fatalf("source completion: %+v %v", r, err)
	}
	c := &ciBindingEnv{f: f, branch: *f.req.Branch, requestID: uuid.New()}
	old := ciBindingOldMR
	c.oldID = c.insertRoot(t, "issue", "completed", &old)
	f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,pipeline_id,issue_title,issue_description)
 VALUES($1,$2,$3,'ci_fix','running',$4,1,$5,123,'refresh','')`, c.requestID, f.w.UserID, f.e.repoID, f.w.ID, c.branch)
	if bound != nil {
		f.e.exec(t, "UPDATE runs SET ci_fix_mr_iid=$2 WHERE id=$1", c.requestID, *bound)
	}
	c.proof = &contextPublicationForge{summary: forge.MergeRequestSummary{IID: ciBindingCurrentMR, State: forge.MRStateOpened,
		SourceBranch: c.branch, HeadSHA: strings.Repeat("b", 40)}}
	f.svc.SetForges(settleUnitBuilder{f: c.proof})
	return c
}

func (c *ciBindingEnv) resolve() PublicationContextResponse {
	return c.f.svc.ResolvePublicationContext(c.f.e.ctx, c.f.w, c.requestID,
		PublicationContextRequest{ClaimGeneration: 1, Branch: c.branch, CandidateHead: strings.Repeat("c", 40)})
}

func (c *ciBindingEnv) wantResolved(t *testing.T, got PublicationContextResponse) {
	t.Helper()
	if got.Status != "resolved" || got.Binding == nil || got.Binding.SourceRunID != c.f.id ||
		got.Binding.MRIID != ciBindingCurrentMR || got.Binding.Intent != "closing" {
		t.Fatalf("want resolved to the current source: %+v", got)
	}
}

func wantUnknown(t *testing.T, got PublicationContextResponse, reason string) {
	t.Helper()
	if got.Status != "unknown" || got.Reason != reason {
		t.Fatalf("want unknown/%s: %+v", reason, got)
	}
}

func ptr64(v int64) *int64 { return &v }

func TestPublicationContextCIFixBindingResolvesCurrentPRLiveDB(t *testing.T) {
	c := seedCIBinding(t, ptr64(ciBindingCurrentMR))
	c.wantResolved(t, c.resolve())
}

func TestPublicationContextCIFixBindingIgnoresNullMRRowsLiveDB(t *testing.T) {
	c := seedCIBinding(t, ptr64(ciBindingCurrentMR))
	c.insertRoot(t, "issue", "failed", nil)
	// runs_kind_shape forbids a NULL-mr_iid mr_rework, so the stray rework carries the old PR.
	c.insertRework(t, c.oldID, ptr64(ciBindingOldMR))
	c.wantResolved(t, c.resolve())
}

func TestPublicationContextCIFixBindingToOldPRLiveDB(t *testing.T) {
	c := seedCIBinding(t, ptr64(ciBindingOldMR))
	// The old PR's root is the only candidate; the forge reports that old bound PR merged.
	c.proof.summary = forge.MergeRequestSummary{IID: ciBindingOldMR, State: forge.MRStateMerged,
		SourceBranch: c.branch, HeadSHA: strings.Repeat("b", 40)}
	wantUnknown(t, c.resolve(), "forge_mismatch")
}

func TestPublicationContextCIFixBindingWithoutRowsLiveDB(t *testing.T) {
	c := seedCIBinding(t, ptr64(99))
	wantUnknown(t, c.resolve(), "association_unknown")
}

func TestPublicationContextCIFixBindingTwoRootsLiveDB(t *testing.T) {
	c := seedCIBinding(t, ptr64(ciBindingCurrentMR))
	c.insertRoot(t, "issue", "completed", ptr64(ciBindingCurrentMR))
	wantUnknown(t, c.resolve(), "ambiguous_source")
}

func TestPublicationContextCIFixUnboundStaysUnknownLiveDB(t *testing.T) {
	c := seedCIBinding(t, nil)
	wantUnknown(t, c.resolve(), "association_unknown")
}

func TestPublicationContextCIFixBindingSourceChangesLiveDB(t *testing.T) {
	t.Run("source moved to another PR before publication", func(t *testing.T) {
		c := seedCIBinding(t, ptr64(ciBindingCurrentMR))
		c.f.e.exec(t, "UPDATE runs SET mr_iid=11 WHERE id=$1", c.f.id)
		wantUnknown(t, c.resolve(), "association_unknown")
	})
	t.Run("claim moves at the barrier", func(t *testing.T) {
		c := seedCIBinding(t, ptr64(ciBindingCurrentMR))
		c.proof.barrier = func() { c.f.e.exec(t, "UPDATE runs SET claim_generation=2 WHERE id=$1", c.requestID) }
		wantUnknown(t, c.resolve(), "claim_changed")
	})
	t.Run("second root appears at the barrier", func(t *testing.T) {
		c := seedCIBinding(t, ptr64(ciBindingCurrentMR))
		c.proof.barrier = func() { c.insertRoot(t, "issue", "completed", ptr64(ciBindingCurrentMR)) }
		wantUnknown(t, c.resolve(), "authority_changed")
	})
	t.Run("rework ancestor re-pointed at the barrier", func(t *testing.T) {
		c := seedCIBinding(t, ptr64(ciBindingCurrentMR))
		rework := c.insertRework(t, c.f.id, ptr64(ciBindingCurrentMR))
		c.proof.barrier = func() {
			c.f.e.exec(t, "UPDATE runs SET target_run_id=$2 WHERE id=$1", rework, c.oldID)
		}
		wantUnknown(t, c.resolve(), "authority_changed")
	})
}
