package workersvc

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Only the remote operations are simulated. Claims, enrollment, custody,
// publication bookkeeping and every lifecycle consumer use the real database.
type taskLifecycleFixture struct {
	f                                    *custodyContinuationFixture
	forge                                *memForge
	root, child                          store.Run
	rootKey, childKey, rootRef, childRef string
}

func newTaskLifecycleFixture(t *testing.T) *taskLifecycleFixture {
	t.Helper()
	return newTaskLifecycleFixtureWithChild(t, false)
}

// thenFix selects the writable descendant; the default remains an advisory review.
func newTaskLifecycleFixtureWithChild(t *testing.T, thenFix bool) *taskLifecycleFixture {
	t.Helper()
	f := newCustodyContinuationFixture(t, setupCodexLiveDB(t))
	root := claimCoordinateTask(t, f)
	// A terminal parent frees the worker's slot before the child's fresh claim.
	f.env.exec("UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", root.ID)
	id := uuid.New()
	if thenFix {
		f.env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,then_fix_of_run_id,
			dispatched_at,issue_title,issue_description,status)
			VALUES($1,$2,$3,'task',$4,$5,now(),'child','lifecycle fixture','queued')`,
			id, f.userID, f.repoID, "uzi/task/"+root.ID.String(), root.ID)
	} else {
		f.env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,review_target_run_id,
			dispatched_at,issue_title,issue_description,status)
			VALUES($1,$2,$3,'task',$4,$5,now(),'child','lifecycle fixture','queued')`,
			id, f.userID, f.repoID, "uzi/task/"+root.ID.String(), root.ID)
	}
	x := &taskLifecycleFixture{f: f, forge: newMemForge(), root: root,
		rootKey: "uzi/task/" + root.ID.String(), childKey: "uzi/task/" + id.String(),
		rootRef:  "refs/uzi-checkpoints/uzi/task/" + root.ID.String(),
		childRef: "refs/uzi-checkpoints/uzi/task/" + id.String()}
	if thenFix {
		f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
			[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1, capability.RecoveryInventoryV1})
	}
	x.child = x.claim(t, id)
	f.wkr.ProtocolCapabilities = []string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1}
	f.svc.SetRetentionLockPool(f.env.pool)
	f.svc.SetBackground(func(fn func()) { fn() })
	f.svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	f.svc.SetPublishFn(x.forge.publish)
	f.svc.SetDeleteCheckpointFn(x.forge.deleteRef)
	f.svc.SetCreateRefFn(x.forge.createRef)
	f.svc.SetListRefTipsFn(x.forge.listRefTips)
	f.svc.checkpointSupersessionCooling = 0
	for _, run := range []store.Run{root, x.child} {
		var got CheckpointCoordinates
		if err := json.Unmarshal(run.CheckpointCoordinates, &got); err != nil {
			t.Fatal(err)
		}
		wantPath := []uuid.UUID{root.ID}
		if run.ID == id {
			wantPath = []uuid.UUID{id, root.ID}
		}
		if got.WorkBranch != x.rootKey || got.CheckpointKey != "uzi/task/"+run.ID.String() ||
			got.RunID != run.ID || got.UserID != f.userID || got.RepoID != f.repoID ||
			got.Version != 1 || got.Kind != "task" || got.IssueIID != nil ||
			len(got.Provenance) != len(wantPath) {
			t.Fatalf("fresh claim coordinates=%+v, want literal root branch and own task key", got)
		}
		for i := range wantPath {
			if got.Provenance[i] != wantPath[i] {
				t.Fatalf("provenance=%v want=%v", got.Provenance, wantPath)
			}
		}
		persisted := mustRun(t, f.env, run.ID)
		if string(persisted.CheckpointCoordinates) != string(run.CheckpointCoordinates) {
			t.Fatal("claim returned coordinates before persistence")
		}
	}
	return x
}

func (x *taskLifecycleFixture) claim(t *testing.T, id uuid.UUID) store.Run {
	t.Helper()
	f := x.f
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.env.ctx) }()
	worker, err := store.New(tx).GetWorkerForUpdate(f.env.ctx, f.workerID)
	if err != nil {
		t.Fatal(err)
	}
	params := coordinateClaimParams(f)
	params.WorkerProtocolCaps = worker.ProtocolCapabilities
	run, err := f.svc.claimCheckpointRun(f.env.ctx, tx, params)
	if err != nil || run.ID != id || len(run.CheckpointCoordinates) == 0 {
		t.Fatalf("fresh claim target=%s got=%s coordinates=%s err=%v", id, run.ID, run.CheckpointCoordinates, err)
	}
	if err := tx.Commit(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	return run
}

func (x *taskLifecycleFixture) publish(t *testing.T, run store.Run, tip string) {
	t.Helper()
	res, err := x.f.svc.Publish(x.f.env.ctx, x.f.wkr, run.ID, tip, []byte("pack"), &run.ClaimGeneration)
	wantRef := "refs/uzi-checkpoints/uzi/task/" + run.ID.String()
	if err != nil || !res.Published || res.Ref != wantRef {
		t.Fatalf("Publish(%s)=%+v err=%v want=%s", run.ID, res, err, wantRef)
	}
	x.ref(t, wantRef, tip)
}

func (x *taskLifecycleFixture) ref(t *testing.T, ref, want string) {
	t.Helper()
	got, ok := x.forge.ref(ref)
	if (want != "" && (!ok || got != want)) || (want == "" && ok) {
		t.Fatalf("remote %s=(%q,%v), want %q", ref, got, ok, want)
	}
}

func (x *taskLifecycleFixture) retention(t *testing.T, run uuid.UUID, key, ref, tip, state string) store.CheckpointRetention {
	t.Helper()
	r, err := x.f.env.q.GetCheckpointRetention(x.f.env.ctx, run)
	if err != nil || r.RunID != run || r.Branch != key || r.Ref != ref || r.Tip != tip || r.State != state {
		t.Fatalf("retention(%s)=%+v err=%v want key=%s ref=%s tip=%s state=%s", run, r, err, key, ref, tip, state)
	}
	return r
}

func (x *taskLifecycleFixture) release(t *testing.T, generation int64) {
	t.Helper()
	f := x.f
	rs := recovery.New(f.env.q, f.env.pool, nil, recovery.Limits{CustodyHoldLimit: fixedCustodyLimit(testCustodyLimit)}, nil)
	rs.SetCustodySettledHook(f.svc.SettleRetainedCheckpoint)
	w := f.wkr
	w.ProtocolCapabilities = append([]string{capability.RecoveryArchiveV2}, w.ProtocolCapabilities...)
	res, err := rs.Release(f.env.ctx, w, x.child.ID, apitypes.RecoveryReleaseRequest{Generation: &generation})
	if err != nil || !res.Released {
		t.Fatalf("Release(child,generation=%d)=%+v err=%v", generation, res, err)
	}
}

// Snapshot all root evidence, including timestamps and outstanding attempts,
// so a child consumer cannot silently consume or rewrite another key's evidence.
func (x *taskLifecycleFixture) rootEvidence(t *testing.T) string {
	t.Helper()
	var s string
	err := x.f.env.pool.QueryRow(x.f.env.ctx, `SELECT jsonb_build_object(
		'run',(SELECT to_jsonb(r) FROM runs r WHERE id=$1),
		'retention',(SELECT to_jsonb(c) FROM checkpoint_retentions c WHERE run_id=$1),
		'custody',(SELECT coalesce(jsonb_agg(to_jsonb(h) ORDER BY id),'[]'::jsonb)
			FROM recovery_custody_holds h WHERE run_id=$1),
		'attempts',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY id),'[]'::jsonb)
			FROM checkpoint_publish_attempts a WHERE run_id=$1))::text`, x.root.ID).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
