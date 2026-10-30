package poller

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
)

// newCrossRunDiscardEnv uses only the original merged-predecessor fixture/API.
func newCrossRunDiscardEnv(t *testing.T) (*mergedPollEnv, uuid.UUID) {
	t.Helper()
	e := newMergedPollEnv(t)
	witness := uuid.New()
	e.exec(`UPDATE runs SET kind='issue',issue_iid=101,status='cancelled',
		claim_generation=1,mr_iid=NULL,mr_state=NULL,
		finished_at='2020-01-02T00:00:00Z' WHERE id=$1`, e.run)
	e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,
		issue_description,status,worker_id,claim_generation,mr_iid,mr_state,
		created_at,finished_at)
		VALUES($1,$2,$3,'issue',101,'witness','d','completed',$4,1,7,'opened',
		'2020-01-03T00:00:00Z','2020-01-04T00:00:00Z')`,
		witness, e.worker.UserID, e.repo, e.worker.ID)

	var eligible bool
	if err := e.pool.QueryRow(e.ctx, `SELECT
		a.kind='issue' AND a.issue_iid>0 AND a.status='cancelled'
		AND a.claim_generation=1 AND a.mr_iid IS NULL
		AND isfinite(a.finished_at) AND isfinite(b.created_at) AND a.finished_at<b.created_at
		AND a.issue_iid=b.issue_iid
		AND a.fail_origin IS DISTINCT FROM 'run_timeout'
		AND b.id<>a.id AND b.kind='issue' AND b.issue_iid>0
		AND b.status='completed' AND b.claim_generation=1 AND b.mr_iid=7
		AND a.user_id=b.user_id AND a.repo_id=b.repo_id
		AND NOT EXISTS (SELECT 1 FROM worker_active_runs w WHERE w.run_id=a.id)
		AND NOT EXISTS (SELECT 1 FROM recovery_custody_holds h WHERE h.run_id=b.id)
		FROM runs a JOIN runs b ON b.id=$2 WHERE a.id=$1`,
		e.run, witness).Scan(&eligible); err != nil {
		t.Fatal(err)
	}
	if !eligible {
		t.Fatal("cross-run fixture does not satisfy target/witness preconditions")
	}
	if got := e.state(e.hold); got != "open" {
		t.Fatalf("target hold before reconciliation = %s, want open", got)
	}

	// Check the witness's exact recorded PR through the real connection builder.
	var forgeType, baseURL string
	var token []byte
	var project, iid int64
	if err := e.pool.QueryRow(e.ctx, `SELECT c.forge_type,c.base_url,
		c.token_ciphertext,p.forge_project_id,r.mr_iid
		FROM runs r JOIN repos p ON p.id=r.repo_id
		JOIN forge_connections c ON c.id=p.connection_id AND c.user_id=r.user_id
		WHERE r.id=$1`, witness).Scan(&forgeType, &baseURL, &token, &project, &iid); err != nil {
		t.Fatal(err)
	}
	f, err := e.svc.ForgeForConnection(forgeType, baseURL, token)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := f.GetMergeRequest(e.ctx, project, iid)
	if err != nil {
		t.Fatal(err)
	}
	if observed.IID != iid || observed.State != forge.MRStateMerged || e.forge.reads != 1 {
		t.Fatalf("fresh witness observation = %+v, reads=%d", observed, e.forge.reads)
	}
	e.svc.SetMergedPredecessorRecovery(e.q, e.recovery)
	return e, witness
}

func TestCrossRunDiscardBaselineLiveDB(t *testing.T) {
	t.Run("same_run_control", func(t *testing.T) {
		e := newMergedPollEnv(t)
		e.exec("UPDATE runs SET status='completed',claim_generation=2 WHERE id=$1", e.run)
		e.svc.SetMergedPredecessorRecovery(e.q, e.recovery)
		e.svc.ReconcileMergedPredecessors(e.ctx, []uuid.UUID{e.repo})
		if got := e.state(e.hold); got != "discarded" || e.forge.reads != 1 {
			t.Fatalf("same-run control: hold=%s, reads=%d; want discarded and 1", got, e.forge.reads)
		}
	})

	t.Run("cross_run_gap", func(t *testing.T) {
		e, _ := newCrossRunDiscardEnv(t)
		e.svc.ReconcileMergedPredecessors(e.ctx, []uuid.UUID{e.repo})

		if got := e.state(e.hold); got != "discarded" {
			t.Fatalf("target hold should be discarded: got %s", got)
		}
	})
}
