package handler

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Shared only by the bounded handler proofs. Claims and enrollment use the public
// service; SQL below sets lifecycle phases, never synthesizes frozen descriptors.
type taskCheckpointHandlerFixture struct {
	*liveEnv
	root, rootHold    uuid.UUID
	rootKey, childKey string
	firstGeneration   int64
	coordinates       []byte
}

type taskCheckpointSchedulingOff struct{}

func (taskCheckpointSchedulingOff) CapabilityAwareScheduling(context.Context) (bool, error) {
	return false, nil
}

func newTaskCheckpointHandlerFixture(t *testing.T) *taskCheckpointHandlerFixture {
	t.Helper()
	e := newLiveEnv(t)
	// This claim fixture deliberately exercises legacy master-sealed credentials.
	e.exec("UPDATE users SET vault_mode='unprotected' WHERE id=$1", e.user)
	// The inherited issue fixture must not occupy the worker's execution slot.
	e.exec("UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", e.run)
	box := newHandlerTestBox(t)
	for _, secret := range []struct{ kind, value string }{
		{"forge", "handler-checkpoint-bot-fixture"},
		{"anthropic_token", "handler-checkpoint-model-fixture"},
	} {
		sealed, err := box.Seal([]byte(secret.value))
		if err != nil {
			t.Fatal(err)
		}
		if secret.kind == "forge" {
			e.exec("UPDATE forge_connections SET token_ciphertext=$1 WHERE user_id=$2", sealed, e.user)
		} else {
			e.exec(`INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with)
				VALUES($1,$2,$3,$4,true,$5,'master')`,
				uuid.New(), e.user, secret.kind, "checkpoint-"+uuid.NewString(), sealed)
		}
	}
	f := &taskCheckpointHandlerFixture{liveEnv: e, root: uuid.New()}
	f.wire()
	caps := []string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1}
	e.exec(`UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now(),status='online'
		WHERE id=ANY($1::uuid[])`, []uuid.UUID{e.workerA, e.workerB}, caps)
	f.rootKey = "uzi/task/" + f.root.String()
	e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,dispatched_at,issue_title,issue_description,status,harness)
		VALUES($1,$2,$3,'task',$4,now(),'root','handler proof','queued','claude')`,
		f.root, e.user, e.repo, f.rootKey)
	root := f.claim(f.root)
	f.rootHold = f.holdID(f.root, root.ClaimGeneration)
	e.exec("UPDATE runs SET status='failed',finished_at=now() WHERE id=$1", f.root)
	e.run = uuid.New()
	f.childKey = "uzi/task/" + e.run.String()
	e.branch = f.rootKey
	e.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,review_target_run_id,dispatched_at,
		issue_title,issue_description,status,harness,required_capabilities)
		VALUES($1,$2,$3,'task',$4,$5,now(),'review child','handler proof','queued','claude','{}')`,
		e.run, e.user, e.repo, f.rootKey, f.root)
	first := f.claim(e.run)
	f.firstGeneration = first.ClaimGeneration
	e.pred = f.holdID(e.run, first.ClaimGeneration)
	f.coordinates = append([]byte(nil), f.runRow().CheckpointCoordinates...)
	f.assertCoordinates(first)
	// A sibling hold on the child has a different original worker.
	e.sibWork = e.insertHold(e.run, first.ClaimGeneration, e.workerB)
	return f
}

// A restart uses the SAME test-box key and explicitly wires the transaction seam.
func (f *taskCheckpointHandlerFixture) wire() {
	e := f.liveEnv
	q := store.New(e.pool)
	box := newHandlerTestBox(e.t)
	e.wsvc = workersvc.New(q, box, workersvc.Params{
		CustodyHoldLimit: defaultCustodyLimit(), WorkerHeartbeatStale: time.Minute,
		WorkerHeartbeatInterval: 10 * time.Second, WorkerAffinityCeiling: 2 * time.Hour, RunTimeout: time.Hour,
	})
	e.wsvc.SetVault(vault.NewPostgres(box, e.pool))
	e.wsvc.SetTxBeginner(e.pool)
	e.wsvc.SetCapabilitySettings(taskCheckpointSchedulingOff{})
	e.wsvc.SetForges(settleForgeBuilder{f: e.lf})
	h := &Handler{pool: e.pool, q: q, box: box, wsvc: e.wsvc, settings: settings.New(q, time.Minute),
		cfg: config.Config{JWTSecret: cliTestSecret, AuthTokenTTL: time.Hour, RunTimeout: time.Hour, WorkerHeartbeatStale: time.Minute}}
	lim := mw.NewLimiter(100000, time.Minute, nil)
	e.router = h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
}

func (f *taskCheckpointHandlerFixture) worker() store.Worker {
	f.t.Helper()
	w, err := store.New(f.pool).GetWorkerByID(f.ctx, f.workerA)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

func (f *taskCheckpointHandlerFixture) claim(id uuid.UUID) *workersvc.ClaimPayload {
	f.t.Helper()
	p, err := f.wsvc.Claim(f.ctx, f.worker(), nil)
	if err != nil || p == nil || p.RunID != id.String() {
		f.t.Fatalf("public Claim target=%s delivered=%v err=%v", id, p != nil, err)
	}
	return p
}

func (f *taskCheckpointHandlerFixture) runRow() store.Run {
	f.t.Helper()
	r, err := store.New(f.pool).GetRunByID(f.ctx, f.run)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *taskCheckpointHandlerFixture) holdID(run uuid.UUID, generation int64) uuid.UUID {
	f.t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM recovery_custody_holds
		WHERE run_id=$1 AND generation=$2 AND original_worker_id=$3 AND state='open'`,
		run, generation, f.workerA).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *taskCheckpointHandlerFixture) assertCoordinates(p *workersvc.ClaimPayload) {
	f.t.Helper()
	want := workersvc.CheckpointCoordinates{Version: 1, RunID: f.run, UserID: f.user, RepoID: f.repo,
		Kind: "task", IssueIID: nil, WorkBranch: f.rootKey, CheckpointKey: f.childKey,
		Provenance: []uuid.UUID{f.run, f.root}}
	var got workersvc.CheckpointCoordinates
	if err := json.Unmarshal(f.runRow().CheckpointCoordinates, &got); err != nil {
		f.t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(p.CheckpointCoordinates, &want) {
		f.t.Fatalf("frozen / delivered coordinates=%+v / %+v want=%+v", got, p.CheckpointCoordinates, want)
	}
}

// Snapshot complete custody audit rows, with deterministic ordering. Equality
// detects any release, new hold, changed live FK or audit/timestamp write.
func (f *taskCheckpointHandlerFixture) custodySnapshot() string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT coalesce(jsonb_agg(to_jsonb(h) ORDER BY id),'[]'::jsonb)::text
		FROM recovery_custody_holds h WHERE user_id=$1`, f.user).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *taskCheckpointHandlerFixture) authoritySnapshot() string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT jsonb_build_object(
		'run',(SELECT to_jsonb(r) FROM runs r WHERE id=$1),
		'holds',(SELECT coalesce(jsonb_agg(to_jsonb(h) ORDER BY id),'[]'::jsonb)
			FROM recovery_custody_holds h WHERE user_id=$2),
		'attempts',(SELECT coalesce(jsonb_agg(to_jsonb(a) ORDER BY id),'[]'::jsonb)
			FROM checkpoint_publish_attempts a WHERE run_id=$1),
		'inputs',(SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY id),'[]'::jsonb)
			FROM run_user_inputs i WHERE run_id=$1))::text`, f.run, f.user).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *taskCheckpointHandlerFixture) successor() int64 {
	f.t.Helper()
	f.exec("UPDATE runs SET status='queued',worker_id=NULL WHERE id=$1", f.run)
	p := f.claim(f.run)
	if p.ClaimGeneration != f.firstGeneration+1 {
		f.t.Fatalf("successor generation=%d predecessor=%d", p.ClaimGeneration, f.firstGeneration)
	}
	f.assertCoordinates(p)
	f.sibGen = f.holdID(f.run, p.ClaimGeneration)
	f.exec("UPDATE runs SET status='running' WHERE id=$1", f.run)
	// Server-held predecessor source predates the new generation's custody.
	f.insertCapture(f.pred, settleSource)
	return p.ClaimGeneration
}
