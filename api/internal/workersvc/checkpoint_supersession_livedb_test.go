package workersvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// checkpoint_supersession_livedb_test.go pins PRD #1810 M3 (D2, D3) against a REAL Postgres: a new
// issue run whose first checkpoint publish is refused not_descendant because an OLDER run's
// retained ref holds the branch slot supersedes it (the old tip moved to refs/uzi-recovery/<old
// run id>, the branch ref freed, the publish retried once), with the whole operation serialised
// on the old run's retention lock. The forge is an in-memory CAS ref map shared by TWO Service
// instances on the same database (two api replicas).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

const (
	supersedeNewTip   = "3333333333333333333333333333333333333333"
	supersedeOtherTip = "4444444444444444444444444444444444444444"
)

// memForge is an in-memory origin: a mutex-guarded ref -> sha map with the pushbroker CAS
// semantics the service relies on, counting every call and every actual ref mutation.
type memForge struct {
	mu       sync.Mutex
	refs     map[string]string
	descends map[string]string // child tip -> the tip it strictly descends

	publishCalls int
	createCalls  int
	// events is the ordered log of every ref write that landed ("publish <ref> <tip>",
	// "create <ref> <tip>", "delete <ref>"), so a test can assert what happened before what.
	events     []string
	creates    map[string]int // refs actually created
	deletes    map[string]int // refs actually deleted
	lastCreate pushbroker.CreateRefOptions

	// M4 seams (set before any concurrent use): deleteErr/listErr fail every delete/list;
	// beforeCreate/beforeDelete/beforeList run OUTSIDE the mutex at the start of the call (the
	// fence has already passed), so a test can hold an attempt inside its forge write.
	deleteErr    error
	listErr      error
	beforeCreate func(ref string)
	// beforeLand runs, with the mutex released, AFTER a create passed every check and BEFORE the
	// ref is written: the create is in flight and lands whatever happens meanwhile (a push the
	// remote already accepted).
	beforeLand   func(ref string)
	beforeDelete func(ref string)
	beforeList   func()
}

func newMemForge() *memForge {
	return &memForge{refs: map[string]string{}, descends: map[string]string{}, creates: map[string]int{}, deletes: map[string]int{}}
}

func (m *memForge) publish(_ context.Context, o pushbroker.Options) (pushbroker.Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishCalls++
	ref := checkpointRefPrefix + o.Branch
	if cur, ok := m.refs[ref]; ok && cur != o.DeclaredTip && m.descends[o.DeclaredTip] != cur {
		return pushbroker.Result{}, pushbroker.ErrNotDescendant
	}
	m.refs[ref] = o.DeclaredTip
	m.events = append(m.events, "publish "+ref+" "+o.DeclaredTip)
	return pushbroker.Result{Ref: ref}, nil
}

// land applies a push LATE, as a forge applying a receive-pack request the api's client already
// gave up on: the real pushbroker binds the request's old value to the branch tip as it FETCHED it
// (old; "" = the ref was absent), so the update lands only if the ref is still exactly there. It
// reports whether it landed.
func (m *memForge) land(ref, old, tip string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.refs[ref]; (ok && cur != old) || (!ok && old != "") {
		return false
	}
	m.refs[ref] = tip
	m.events = append(m.events, "publish "+ref+" "+tip)
	return true
}

func (m *memForge) createRef(_ context.Context, o pushbroker.CreateRefOptions) error {
	if m.beforeCreate != nil {
		m.beforeCreate(o.Ref)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCalls++
	m.lastCreate = o
	if !strings.HasPrefix(o.Ref, pushbroker.RecoveryRefPrefix) || !strings.HasPrefix(o.SourceRef, checkpointRefPrefix) {
		return pushbroker.ErrInvalidRef
	}
	if cur, ok := m.refs[o.Ref]; ok {
		if cur == o.Tip {
			return pushbroker.ErrRefExistsAtTip
		}
		return pushbroker.ErrRefExists
	}
	if m.refs[o.SourceRef] != o.Tip {
		return pushbroker.ErrSourceMissing
	}
	if m.beforeLand != nil {
		m.mu.Unlock()
		m.beforeLand(o.Ref)
		m.mu.Lock()
	}
	m.refs[o.Ref] = o.Tip
	m.creates[o.Ref]++
	m.events = append(m.events, "create "+o.Ref+" "+o.Tip)
	return nil
}

func (m *memForge) deleteRef(_ context.Context, o pushbroker.DeleteOptions) error {
	ref := o.Ref
	if ref == "" {
		ref = checkpointRefPrefix + o.Branch
	}
	if m.beforeDelete != nil {
		m.beforeDelete(ref)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteErr != nil {
		return m.deleteErr
	}
	if o.ExpectedOldTip == "" {
		return errors.New("memForge: an unconditional delete is never expected from retention")
	}
	if m.refs[ref] == o.ExpectedOldTip {
		delete(m.refs, ref)
		m.deletes[ref]++
		m.events = append(m.events, "delete "+ref)
	}
	return nil // absent or advanced: benign, like pushbroker's CAS delete
}

func (m *memForge) listRefTips(_ context.Context, _ pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
	if m.beforeList != nil {
		m.beforeList()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	out := map[string]string{}
	for _, r := range refs {
		if tip, ok := m.refs[r]; ok {
			out[r] = tip
		}
	}
	return out, nil
}

func (m *memForge) ref(name string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tip, ok := m.refs[name]
	return tip, ok
}

func (m *memForge) set(name, tip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs[name] = tip
}

func (m *memForge) counts(ref string) (creates, deletes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creates[ref], m.deletes[ref]
}

func (m *memForge) eventLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.events...)
}

func (m *memForge) calls() (publish, create int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.publishCalls, m.createCalls
}

// supersedeFix: an OLD failed issue run whose published checkpoint is retained (its custody hold
// open) and a NEW running issue run on the same issue/branch, served by two Service instances.
//
// The old run's hold is opened BEFORE its terminal UPDATE, as in production (a hold is only ever
// opened at claim): migration 00265's runs.status trigger records the retention row in the
// terminal transaction itself, and reads the open-hold state at that instant.
type supersedeFix struct {
	e          interlockLiveDB
	rf         *retentionFix // for the pg_locks helpers
	pat        string
	forge      *memForge
	svc1, svc2 *Service

	iid         int64
	branch      string
	branchRef   string
	recoveryRef string
	oldRun      uuid.UUID
	oldHold     uuid.UUID
	newRun      uuid.UUID
	newWorker   uuid.UUID
}

func newSupersedeFix(t *testing.T) *supersedeFix {
	t.Helper()
	f := newSupersedeFixWith(t, nil)
	// The post-commit Go half of the terminal path (retainOrDeleteCheckpoint): it finds the
	// trigger's row and leaves it retained.
	f.svc1.retainOrDeleteCheckpoint(f.e.ctx, f.oldRun, runkind.Issue, pgtype.Int8{Int64: f.iid, Valid: true})
	if r := f.row(t); r.State != retentionRetained {
		t.Fatalf("setup: old run's record = %q, want retained", r.State)
	}
	return f
}

// newSupersedeFixWith builds the fixture with no Go retention call at all: the old run's terminal
// status is committed by terminal (nil: one raw UPDATE to failed), so whatever records it is the
// database alone. The old run's checkpoint is published (checkpoint_tip, and the branch ref on the
// forge at retentionTestTip) and its hold open before terminal runs.
func newSupersedeFixWith(t *testing.T, terminal func(f *supersedeFix)) *supersedeFix {
	t.Helper()
	e := setupInterlockLiveDB(t)
	box := newBox(t)
	pat := strings.Join([]string{"bot", "pat", "supersede", uuid.NewString()}, "-")
	sealed, err := box.Seal([]byte(pat))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	e.exec(t, `UPDATE forge_connections SET token_ciphertext = $1
	           WHERE id = (SELECT connection_id FROM repos WHERE id = $2)`, sealed, e.repoID)
	forge := newMemForge()
	mk := func() *Service {
		svc := New(e.q, box, testParams())
		svc.SetTxBeginner(e.pool)
		svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
		svc.SetBackground(func(fn func()) { fn() })
		svc.SetRetentionLockPool(e.pool)
		svc.SetPublishFn(forge.publish)
		svc.SetDeleteCheckpointFn(forge.deleteRef)
		svc.SetCreateRefFn(forge.createRef)
		svc.SetListRefTipsFn(forge.listRefTips)
		// Supersession is driven the moment the old run is terminal: the cooling period
		// (BeginCheckpointSupersession) has its own tests in checkpoint_publish_attempt_livedb_test.go.
		svc.checkpointSupersessionCooling = 0
		return svc
	}
	f := &supersedeFix{e: e, rf: &retentionFix{e: e}, pat: pat, forge: forge, svc1: mk(), svc2: mk()}

	oldWorker := e.seedWorker(t, nil)
	f.iid = *e.nextIID
	f.oldRun = e.seedLegacyRunningRun(t, oldWorker)
	e.exec(t, `UPDATE runs SET checkpoint_tip = $2, checkpoint_tip_at = now(), claim_generation = 1
	           WHERE id = $1`, f.oldRun, retentionTestTip)
	f.oldHold = mhOpenHold(t, e, f.oldRun, 1, oldWorker)
	f.branch = agentIssueBranch(f.iid)
	f.branchRef = checkpointRefPrefix + f.branch
	f.recoveryRef = pushbroker.RecoveryRefPrefix + f.oldRun.String()
	forge.set(f.branchRef, retentionTestTip)
	if terminal == nil {
		e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, f.oldRun)
	} else {
		terminal(f)
	}

	f.newWorker = e.seedWorker(t, nil)
	f.newRun = uuid.New()
	e.exec(t, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, kind, status, worker_id)
	           VALUES ($1, $2, $3, $4, 't', 'd', 'issue', 'running', $5)`, f.newRun, e.userID, e.repoID, f.iid, f.newWorker)
	return f
}

func (f *supersedeFix) row(t *testing.T) store.CheckpointRetention {
	t.Helper()
	r, err := f.e.q.GetCheckpointRetention(f.e.ctx, f.oldRun)
	if err != nil {
		t.Fatalf("GetCheckpointRetention: %v", err)
	}
	return r
}

func (f *supersedeFix) publishNew(t *testing.T, svc *Service) PublishResult {
	t.Helper()
	res, err := svc.Publish(f.e.ctx, store.Worker{ID: f.newWorker, UserID: f.e.userID}, f.newRun, supersedeNewTip, []byte("pack"))
	if err != nil {
		t.Errorf("Publish: %v", err)
	}
	return res
}

func (f *supersedeFix) discardHold(t *testing.T) {
	t.Helper()
	f.e.exec(t, `UPDATE recovery_custody_holds SET state = 'discarded', live_worker_id = NULL, live_run_id = NULL,
	               released_at = now() WHERE id = $1`, f.oldHold)
}

// assertSuperseded: the old tip lives at the recovery ref, the record names it, and the branch
// carries the new run's tip.
func (f *supersedeFix) assertSuperseded(t *testing.T) {
	t.Helper()
	if tip, ok := f.forge.ref(f.recoveryRef); !ok || tip != retentionTestTip {
		t.Fatalf("recovery ref = %q (present %v), want the old tip %s", tip, ok, retentionTestTip)
	}
	r := f.row(t)
	if r.State != retentionSuperseded || r.Ref != f.recoveryRef || r.Tip != retentionTestTip ||
		!r.RecoveryRef.Valid || r.RecoveryRef.String != f.recoveryRef {
		t.Fatalf("record = {state %q ref %q tip %q recovery %v}, want superseded at the recovery ref", r.State, r.Ref, r.Tip, r.RecoveryRef)
	}
}

// pauser blocks the first forge write of op on a service until resumed.
type pauser struct {
	op     string
	once   sync.Once
	paused chan struct{}
	resume chan struct{}
}

func pauseAt(svc *Service, op string) *pauser {
	p := &pauser{op: op, paused: make(chan struct{}), resume: make(chan struct{})}
	svc.retentionHooks = &retentionTestHooks{beforeForgeWrite: func(_ uuid.UUID, gotOp string) {
		if gotOp == p.op {
			p.once.Do(func() {
				close(p.paused)
				<-p.resume
			})
		}
	}}
	return p
}

func (p *pauser) waitPaused(t *testing.T) {
	t.Helper()
	select {
	case <-p.paused:
	case <-time.After(20 * time.Second):
		t.Fatalf("the attempt never reached its %q write", p.op)
	}
}

// --- The happy path ------------------------------------------------------------------------------

// TestSupersessionFreesBranchForNewRunLiveDB is M3's headline: the new run is not blocked, the
// old tip survives at refs/uzi-recovery/<old run id>, the record is superseded, the publish landed
// on its first push (the slot is claimed before any push, claimCheckpointSlot), and the owner's
// recovery hold list reports the recovery ref.
func TestSupersessionFreesBranchForNewRunLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	res := f.publishNew(t, f.svc1)
	if !res.Published || res.Skipped != "" {
		t.Fatalf("Publish = %+v, want published after supersession", res)
	}
	f.assertSuperseded(t)
	if tip, _ := f.forge.ref(f.branchRef); tip != supersedeNewTip {
		t.Fatalf("branch ref = %q, want the new run's tip", tip)
	}
	if pubs, creates := f.forge.calls(); pubs != 1 || creates != 1 {
		t.Fatalf("publish calls = %d, create calls = %d; want 1 (the slot claimed before the push) and 1", pubs, creates)
	}
	c := f.forge.lastCreate
	if c.CloneURL != "https://forge.e2e/g/interlock.git" || c.Username != "bot" || c.PAT != f.pat ||
		c.SourceRef != f.branchRef || c.Tip != retentionTestTip {
		t.Fatalf("create coordinates = {clone %q user %q pat-matches %v source %q tip %q}, want server-derived",
			c.CloneURL, c.Username, c.PAT == f.pat, c.SourceRef, c.Tip)
	}
	if cr, del := f.forge.counts(f.branchRef); cr != 0 || del != 1 {
		t.Fatalf("branch ref creates/deletes = %d/%d, want 0/1", cr, del)
	}
	var newTip pgtype.Text
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT checkpoint_tip FROM runs WHERE id = $1`, f.newRun).Scan(&newTip); err != nil || newTip.String != supersedeNewTip {
		t.Fatalf("new run checkpoint_tip = %v (err %v), want the retried publish's tip persisted", newTip, err)
	}

	// D3 reader: the owner's hold list names where the old run's work now lives.
	holds, err := recovery.New(store.New(f.e.pool), f.e.pool, nil, recovery.Limits{CustodyHoldLimit: 8}, nil).
		ListHoldsForOwner(f.e.ctx, f.e.userID, true)
	if err != nil {
		t.Fatalf("ListHoldsForOwner: %v", err)
	}
	found := false
	for _, h := range holds.Holds {
		if h.RunID != f.oldRun.String() {
			continue
		}
		found = true
		if h.CheckpointRef != f.recoveryRef || h.CheckpointTip != retentionTestTip || h.CheckpointState != retentionSuperseded {
			t.Fatalf("hold checkpoint = {%q %q %q}, want {%q %q superseded}", h.CheckpointRef, h.CheckpointTip, h.CheckpointState,
				f.recoveryRef, retentionTestTip)
		}
	}
	if !found {
		t.Fatalf("the old run's open hold is missing from ListHoldsForOwner")
	}
}

// TestSupersessionOnDescendantPublishLiveDB (#1810 M1 rework): the new run's tip DESCENDS from
// the old run's retained tip, so the broker would accept it as a fast-forward and never refuse
// not_descendant. The old record must still be superseded, with the recovery ref created at the
// old tip BEFORE the new run's push, never left retained at a tip that is no longer the branch tip.
func TestSupersessionOnDescendantPublishLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.forge.mu.Lock()
	f.forge.descends[supersedeNewTip] = retentionTestTip
	f.forge.mu.Unlock()

	res := f.publishNew(t, f.svc1)
	if !res.Published || res.Skipped != "" {
		t.Fatalf("Publish = %+v, want published", res)
	}
	f.assertSuperseded(t)
	if tip, _ := f.forge.ref(f.branchRef); tip != supersedeNewTip {
		t.Fatalf("branch ref = %q, want the new run's tip", tip)
	}
	events := f.forge.eventLog()
	create, publish := -1, -1
	for i, ev := range events {
		switch ev {
		case "create " + f.recoveryRef + " " + retentionTestTip:
			create = i
		case "publish " + f.branchRef + " " + supersedeNewTip:
			publish = i
		}
	}
	if create < 0 || publish < 0 || create > publish {
		t.Fatalf("forge events = %q; want the recovery ref created at the old tip BEFORE the new run's push", events)
	}
	if pubs, creates := f.forge.calls(); pubs != 1 || creates != 1 {
		t.Fatalf("publish calls = %d, create calls = %d; want 1 and 1", pubs, creates)
	}
}

// TestTerminalPublishRefusedOverAnotherRunsRetentionLiveDB (#1810 M1 rework): a TERMINAL run whose
// tip descends from ANOTHER run's retained tip must not fast-forward over it (the other record
// would be left retained at a stale tip) and must never evict it: Skipped "superseded", no forge
// call at all, the other record untouched.
func TestTerminalPublishRefusedOverAnotherRunsRetentionLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.e.exec(t, `UPDATE runs SET status = 'failed', finished_at = now() WHERE id = $1`, f.newRun)
	f.forge.mu.Lock()
	f.forge.descends[supersedeNewTip] = retentionTestTip
	f.forge.mu.Unlock()

	res := f.publishNew(t, f.svc1)
	if res.Published || res.Skipped != "superseded" {
		t.Fatalf("Publish = %+v, want the superseded skip", res)
	}
	if pubs, creates := f.forge.calls(); pubs != 0 || creates != 0 {
		t.Fatalf("publish calls = %d, create calls = %d; want 0 and 0 (no forge call)", pubs, creates)
	}
	if r := f.row(t); r.State != retentionRetained || r.Tip != retentionTestTip || r.Ref != f.branchRef {
		t.Fatalf("other run's record = {state %q tip %q ref %q}, want retained at its tip", r.State, r.Tip, r.Ref)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != retentionTestTip {
		t.Fatalf("branch ref = %q, want untouched at %s", tip, retentionTestTip)
	}
}

// TestTerminalPublishSerializedWithSupersessionLiveDB (#1810 M1 rework): a TERMINAL run's publish
// holds its own retention lock from the superseded check through the record track, so a
// supersession of that run (a newer run's publish on another api replica) that tries to run
// between the check and the push cannot take the lock. The publish lands and the run's record
// tracks the new tip: no branch ref is left untracked by a superseded record.
func TestTerminalPublishSerializedWithSupersessionLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	const t2 = "5555555555555555555555555555555555555555"
	var oldWorker uuid.UUID
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT worker_id FROM runs WHERE id = $1`, f.oldRun).Scan(&oldWorker); err != nil {
		t.Fatalf("old worker: %v", err)
	}
	f.forge.mu.Lock()
	f.forge.descends[t2] = retentionTestTip
	f.forge.mu.Unlock()

	var (
		ran             bool
		freed, acquired bool
		supersedeErr    error
	)
	f.svc1.SetPublishFn(func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
		if !ran {
			ran = true
			// A supersession of the old run landing between its superseded check and its push.
			freed, acquired, supersedeErr = f.svc2.supersedeRetainedCheckpoint(f.e.ctx, f.oldRun)
		}
		return f.forge.publish(ctx, o)
	})

	res, err := f.svc1.Publish(f.e.ctx, store.Worker{ID: oldWorker, UserID: f.e.userID}, f.oldRun, t2, []byte("pack"))
	if err != nil || !res.Published {
		t.Fatalf("terminal run's Publish = %+v, %v; want published", res, err)
	}
	if !ran {
		t.Fatalf("the publish stub never ran")
	}
	if acquired || freed || supersedeErr != nil {
		t.Fatalf("inline supersession = {freed %v acquired %v err %v}, want it refused the lock", freed, acquired, supersedeErr)
	}
	r := f.row(t)
	if r.State != retentionRetained || r.Tip != t2 || r.Ref != f.branchRef || r.RecoveryRef.Valid {
		t.Fatalf("record = {state %q tip %q ref %q recovery %v}, want retained at the published tip %s",
			r.State, r.Tip, r.Ref, r.RecoveryRef, t2)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != t2 {
		t.Fatalf("branch ref = %q, want the terminal run's published tip %s", tip, t2)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("a recovery ref was created: the supersession interleaved with the terminal publish")
	}
}

// TestSupersessionFallbackFreesSettlingRecordLiveDB: the not_descendant fallback
// (freeCheckpointSlot) still covers what the pre-push claim does not list: a SETTLING record of an
// older run holding the branch ref is settled (its ref CAS-deleted) and the publish retried once.
func TestSupersessionFallbackFreesSettlingRecordLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.discardHold(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'settling' WHERE run_id = $1`, f.oldRun)

	res := f.publishNew(t, f.svc1)
	if !res.Published {
		t.Fatalf("Publish = %+v, want published after the fallback settled the old ref", res)
	}
	if pubs, creates := f.forge.calls(); pubs != 2 || creates != 0 {
		t.Fatalf("publish calls = %d, create calls = %d; want 2 (refused, then the one retry) and 0", pubs, creates)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != supersedeNewTip {
		t.Fatalf("branch ref = %q, want the new run's tip", tip)
	}
	if r := f.row(t); r.State != "deleted" {
		t.Fatalf("old record = %q, want deleted", r.State)
	}
}

// TestSupersessionOnlyForIssueRunsLiveDB: a self_improve run's branch is per-run, so a
// not_descendant refusal there never triggers supersession.
func TestSupersessionOnlyForIssueRunsLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.e.exec(t, `UPDATE runs SET kind = 'self_improve' WHERE id = $1`, f.newRun)
	f.forge.set(checkpointRefPrefix+"uzi/self-improve/"+f.newRun.String(), supersedeOtherTip)
	res := f.publishNew(t, f.svc1)
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("Publish = %+v, want the plain not_descendant skip", res)
	}
	if r := f.row(t); r.State != retentionRetained {
		t.Fatalf("old record = %q, want retained (untouched)", r.State)
	}
	if _, creates := f.forge.calls(); creates != 0 {
		t.Fatalf("create calls = %d, want 0", creates)
	}
}

// --- Refusals: the branch ref is left alone ------------------------------------------------------

// assertStopped: no recovery ref, the branch ref untouched at want, the record kept (superseding)
// with a last_error and a backed-off retry, and the publish skipped not_descendant.
func (f *supersedeFix) assertStopped(t *testing.T, res PublishResult, branchTip string, recoveryAt string) {
	t.Helper()
	if res.Published || res.Skipped != "not_descendant" {
		t.Fatalf("Publish = %+v, want the not_descendant skip", res)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != branchTip {
		t.Fatalf("branch ref = %q, want untouched at %s", tip, branchTip)
	}
	tip, ok := f.forge.ref(f.recoveryRef)
	if recoveryAt == "" && ok {
		t.Fatalf("recovery ref created at %s", tip)
	}
	if recoveryAt != "" && tip != recoveryAt {
		t.Fatalf("recovery ref = %q, want untouched at %s", tip, recoveryAt)
	}
	if cr, _ := f.forge.counts(f.recoveryRef); cr != 0 {
		t.Fatalf("recovery ref creates = %d, want 0", cr)
	}
	if _, del := f.forge.counts(f.branchRef); del != 0 {
		t.Fatalf("branch ref deletes = %d, want 0", del)
	}
	r := f.row(t)
	if r.State != retentionSuperseding || !r.LastError.Valid || r.Attempts != 1 || !time.Now().Before(r.NextAttemptAt.Time) {
		t.Fatalf("record = {state %q last_error %v attempts %d next %v}, want superseding with last_error and backoff",
			r.State, r.LastError, r.Attempts, r.NextAttemptAt.Time)
	}
	if strings.Contains(r.LastError.String, f.pat) {
		t.Fatalf("last_error carries the PAT")
	}
}

// TestSupersessionLeavesAdvancedBranchLiveDB: the branch ref was advanced by ANOTHER run (it is
// not at the record's tip): it is left alone and the record keeps its state with last_error.
func TestSupersessionLeavesAdvancedBranchLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.forge.set(f.branchRef, supersedeOtherTip)
	f.assertStopped(t, f.publishNew(t, f.svc1), supersedeOtherTip, "")
	if !strings.Contains(f.row(t).LastError.String, "not at the recorded tip") {
		t.Fatalf("last_error = %q, want the branch-moved reason", f.row(t).LastError.String)
	}
}

// TestSupersessionStopsOnRecoveryRefAtOtherTipLiveDB: a recovery ref already present at a
// different tip stops supersession; the branch ref is untouched.
func TestSupersessionStopsOnRecoveryRefAtOtherTipLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.forge.set(f.recoveryRef, supersedeOtherTip)
	f.assertStopped(t, f.publishNew(t, f.svc1), retentionTestTip, supersedeOtherTip)
	if !strings.Contains(f.row(t).LastError.String, "recovery ref exists at another tip") {
		t.Fatalf("last_error = %q, want the recovery-ref-exists reason", f.row(t).LastError.String)
	}
}

// TestSupersessionTipLagKeepsRecordLiveDB: the record's tip T1 lags the branch ref, which holds a
// later publish T2 of the same run: the record is kept (never closed as deleted) and the branch
// ref untouched.
func TestSupersessionTipLagKeepsRecordLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	const t2 = "5555555555555555555555555555555555555555"
	f.forge.set(f.branchRef, t2)
	f.assertStopped(t, f.publishNew(t, f.svc1), t2, "")
}

// TestPublishAdvancesRetainedRecordTipLiveDB (tip lag, M1 review N5): a successful publish by a
// run that owns a retained record whose ref is the branch ref moves the record to the new tip,
// so a later supersession binds to what origin holds and completes.
func TestPublishAdvancesRetainedRecordTipLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	const t2 = "5555555555555555555555555555555555555555"
	var oldWorker uuid.UUID
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT worker_id FROM runs WHERE id = $1`, f.oldRun).Scan(&oldWorker); err != nil {
		t.Fatalf("old worker: %v", err)
	}
	f.forge.mu.Lock()
	f.forge.descends[t2] = retentionTestTip
	f.forge.mu.Unlock()
	res, err := f.svc1.Publish(f.e.ctx, store.Worker{ID: oldWorker, UserID: f.e.userID}, f.oldRun, t2, []byte("pack"))
	if err != nil || !res.Published {
		t.Fatalf("old run's late Publish = %+v, %v; want published", res, err)
	}
	if r := f.row(t); r.Tip != t2 || r.State != retentionRetained {
		t.Fatalf("record = {state %q tip %q}, want retained at the new tip %s", r.State, r.Tip, t2)
	}
	// A record in any other state, or naming another ref, is not moved.
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'superseded' WHERE run_id = $1`, f.oldRun)
	if n, err := f.e.q.AdvanceCheckpointRetentionTip(f.e.ctx, store.AdvanceCheckpointRetentionTipParams{
		RunID: f.oldRun, Ref: f.branchRef, Tip: supersedeOtherTip,
	}); err != nil || n != 0 {
		t.Fatalf("AdvanceCheckpointRetentionTip on a superseded record moved %d rows (err %v), want 0", n, err)
	}
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'retained' WHERE run_id = $1`, f.oldRun)

	res = f.publishNew(t, f.svc1)
	if !res.Published {
		t.Fatalf("new run's Publish = %+v, want published", res)
	}
	if tip, _ := f.forge.ref(f.recoveryRef); tip != t2 {
		t.Fatalf("recovery ref = %q, want the advanced tip %s", tip, t2)
	}
}

// --- Interruption regressions --------------------------------------------------------------------

// crashAt runs one publish on svc1 whose session is terminated at op's write (before the fence),
// so the attempt dies exactly there, as a crashed api would.
func (f *supersedeFix) crashAt(t *testing.T, op string) {
	t.Helper()
	f.svc1.retentionHooks = &retentionTestHooks{beforeForgeWrite: func(id uuid.UUID, gotOp string) {
		if gotOp == op {
			f.rf.terminateHolder(t, id)
		}
	}}
	res := f.publishNew(t, f.svc1)
	f.svc1.retentionHooks = nil
	if res.Published {
		t.Fatalf("Publish = %+v, want it not to land (the supersession crashed)", res)
	}
	if r := f.row(t); r.State != retentionSuperseding {
		t.Fatalf("record after the crash = %q, want superseding", r.State)
	}
}

// reconcile drives the sweeper pass (confined to the old run) on svc2 and asserts the record
// reaches superseded with the tip reachable at the recovery ref.
func (f *supersedeFix) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.svc2.reconcileCheckpointRetentions(f.e.ctx, pgconv.UUID(f.oldRun)); err != nil {
		t.Fatalf("ReconcileCheckpointRetentions: %v", err)
	}
	f.assertSuperseded(t)
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatalf("branch ref still present after the reconcile")
	}
	if cr, _ := f.forge.counts(f.recoveryRef); cr != 1 {
		t.Fatalf("recovery ref creates = %d, want exactly 1", cr)
	}
	if _, del := f.forge.counts(f.branchRef); del != 1 {
		t.Fatalf("branch ref deletes = %d, want exactly 1", del)
	}
	res := f.publishNew(t, f.svc2)
	if !res.Published {
		t.Fatalf("new run's publish after the reconcile = %+v, want published", res)
	}
}

func TestSupersessionCrashAfterRecordBeforeCreateLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.crashAt(t, "create")
	if _, creates := f.forge.calls(); creates != 0 {
		t.Fatalf("create calls = %d, want 0 (crashed before the create)", creates)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != retentionTestTip {
		t.Fatalf("branch ref = %q, want the old tip still there", tip)
	}
	f.reconcile(t)
}

func TestSupersessionCrashAfterCreateBeforeBranchDeleteLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.crashAt(t, "delete-branch")
	if tip, _ := f.forge.ref(f.recoveryRef); tip != retentionTestTip {
		t.Fatalf("recovery ref = %q, want created before the crash", tip)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != retentionTestTip {
		t.Fatalf("branch ref = %q, want still at the old tip (crashed before its delete)", tip)
	}
	f.reconcile(t) // the re-drive's create finds the recovery ref at the tip (ErrRefExistsAtTip)
}

// TestSupersessionCrashAfterBranchDeleteLiveDB: forge steps 2 and 3 both landed but the record
// was never marked: the re-drive's create sees the source gone, the list finds the recovery ref
// at the tip, and it completes.
func TestSupersessionCrashAfterBranchDeleteLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'superseding', recovery_ref = $2 WHERE run_id = $1`, f.oldRun, f.recoveryRef)
	f.forge.set(f.recoveryRef, retentionTestTip)
	f.forge.mu.Lock()
	delete(f.forge.refs, f.branchRef)
	f.forge.mu.Unlock()
	if _, err := f.svc2.reconcileCheckpointRetentions(f.e.ctx, pgconv.UUID(f.oldRun)); err != nil {
		t.Fatalf("ReconcileCheckpointRetentions: %v", err)
	}
	f.assertSuperseded(t)
}

// TestSupersessionTipGoneClosesRecordLiveDB: origin holds the tip under neither ref: the record
// closes as deleted (nothing uzi owns references it) and the branch slot is free.
func TestSupersessionTipGoneClosesRecordLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.forge.mu.Lock()
	delete(f.forge.refs, f.branchRef)
	f.forge.mu.Unlock()
	f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'superseding', recovery_ref = $2 WHERE run_id = $1`, f.oldRun, f.recoveryRef)
	if _, err := f.svc2.reconcileCheckpointRetentions(f.e.ctx, pgconv.UUID(f.oldRun)); err != nil {
		t.Fatalf("ReconcileCheckpointRetentions: %v", err)
	}
	if r := f.row(t); r.State != "deleted" || !r.LastError.Valid {
		t.Fatalf("record = {state %q last_error %v}, want deleted with the audit note", r.State, r.LastError)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("a recovery ref was created for a tip origin no longer holds")
	}
}

// --- Deterministic paused-create regressions (the approver's scenario) --------------------------

// TestSupersessionPausedCreateHoldsOutOthersLiveDB (a): instance 1's attempt pauses before its
// create while holding the lock. Instance 2's publish trigger and a settle triggered after the
// hold is discarded both fail their try-lock and change nothing. Resumed, instance 1 creates,
// deletes the branch ref, finds no hold, and settles the recovery ref: the record ends deleted,
// origin holds neither old ref, and each ref saw exactly one create/delete.
func TestSupersessionPausedCreateHoldsOutOthersLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	p := pauseAt(f.svc1, "create")
	done := make(chan PublishResult, 1)
	go func() { done <- f.publishNew(t, f.svc1) }()
	p.waitPaused(t)

	if res := f.publishNew(t, f.svc2); res.Published || res.Skipped != "not_descendant" {
		t.Errorf("instance 2 Publish while instance 1 holds the lock = %+v, want the not_descendant skip", res)
	}
	f.discardHold(t)
	f.svc2.SettleRetainedCheckpoint(f.oldRun)
	if r := f.row(t); r.State != retentionSuperseding {
		t.Errorf("record while paused = %q, want superseding (untouched by instance 2)", r.State)
	}
	if _, creates := f.forge.calls(); creates != 0 {
		t.Errorf("create calls while paused = %d, want 0", creates)
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != retentionTestTip {
		t.Errorf("branch ref while paused = %q, want untouched", tip)
	}

	close(p.resume)
	res := <-done
	if !res.Published {
		t.Fatalf("instance 1 Publish = %+v, want published after its supersession", res)
	}
	if r := f.row(t); r.State != "deleted" || !r.VerifyAfter.Valid {
		t.Fatalf("record = {state %q verify_after %v}, want deleted with a recovery-ref re-verify", r.State, r.VerifyAfter)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("recovery ref left on origin")
	}
	if tip, _ := f.forge.ref(f.branchRef); tip != supersedeNewTip {
		t.Fatalf("branch ref = %q, want only the new run's tip", tip)
	}
	for _, ref := range []string{f.recoveryRef, f.branchRef} {
		if cr, del := f.forge.counts(ref); del != 1 || (ref == f.recoveryRef && cr != 1) || (ref == f.branchRef && cr != 0) {
			t.Fatalf("%s creates/deletes = %d/%d, want exactly one of each for the recovery ref and one delete for the branch ref", ref, cr, del)
		}
	}
}

// TestSupersessionPausedCreateLockLostLiveDB (b): instance 1 pauses before its create and its
// session is killed. Instance 2's reconcile takes the lock, supersedes and settles (the hold was
// discarded): record deleted, origin clean. Resumed, instance 1's fence fails: it performs NO
// create, so origin stays clean and no untracked ref appears.
func TestSupersessionPausedCreateLockLostLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	p := pauseAt(f.svc1, "create")
	done := make(chan PublishResult, 1)
	go func() { done <- f.publishNew(t, f.svc1) }()
	p.waitPaused(t)

	f.rf.terminateHolder(t, f.oldRun)
	f.discardHold(t)
	if _, err := f.svc2.reconcileCheckpointRetentions(f.e.ctx, pgconv.UUID(f.oldRun)); err != nil {
		t.Fatalf("instance 2 ReconcileCheckpointRetentions: %v", err)
	}
	if r := f.row(t); r.State != "deleted" {
		t.Fatalf("record after instance 2 = %q, want deleted", r.State)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("recovery ref left on origin by instance 2")
	}
	if _, ok := f.forge.ref(f.branchRef); ok {
		t.Fatalf("branch ref left on origin by instance 2")
	}
	_, createsBefore := f.forge.calls()
	if createsBefore != 1 {
		t.Fatalf("create calls after instance 2 = %d, want 1", createsBefore)
	}

	close(p.resume)
	res := <-done
	if res.Published {
		t.Fatalf("instance 1 Publish = %+v; its supersession lost the lock and must not report a freed slot", res)
	}
	if _, creates := f.forge.calls(); creates != createsBefore {
		t.Fatalf("create calls = %d after instance 1 resumed, want %d: the fence must stop the stale create", creates, createsBefore)
	}
	f.forge.mu.Lock()
	refs := len(f.forge.refs)
	f.forge.mu.Unlock()
	if refs != 0 {
		t.Fatalf("origin refs = %v, want none (no untracked ref)", f.forge.refs)
	}
	if r := f.row(t); r.State != "deleted" {
		t.Fatalf("record after instance 1 resumed = %q, want still deleted", r.State)
	}
}

// --- Concurrency and ordering --------------------------------------------------------------------

// TestSupersessionConcurrentAttemptsLiveDB: both instances' publish triggers and a reconcile race
// on the same record behind a start barrier: exactly one create and one branch delete, the record
// superseded, every publish landed or skipped benignly, and a later publish lands.
func TestSupersessionConcurrentAttemptsLiveDB(t *testing.T) {
	for _, start := range []string{retentionRetained, retentionSuperseding} {
		t.Run("from "+start, func(t *testing.T) {
			f := newSupersedeFix(t)
			if start == retentionSuperseding {
				f.e.exec(t, `UPDATE checkpoint_retentions SET state = 'superseding', recovery_ref = $2 WHERE run_id = $1`, f.oldRun, f.recoveryRef)
			}
			barrier := make(chan struct{})
			var wg sync.WaitGroup
			results := make(chan PublishResult, 2)
			for _, svc := range []*Service{f.svc1, f.svc2} {
				wg.Add(1)
				go func(svc *Service) {
					defer wg.Done()
					<-barrier
					results <- f.publishNew(t, svc)
				}(svc)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-barrier
				if _, err := f.svc1.reconcileCheckpointRetentions(f.e.ctx, pgconv.UUID(f.oldRun)); err != nil {
					t.Errorf("ReconcileCheckpointRetentions: %v", err)
				}
			}()
			close(barrier)
			wg.Wait()
			close(results)
			for res := range results {
				if !res.Published && res.Skipped != "not_descendant" {
					t.Fatalf("a racing Publish = %+v, want published or the benign not_descendant skip", res)
				}
			}
			if res := f.publishNew(t, f.svc2); !res.Published {
				t.Fatalf("a later Publish = %+v, want published", res)
			}
			f.assertSuperseded(t)
			if cr, _ := f.forge.counts(f.recoveryRef); cr != 1 {
				t.Fatalf("recovery ref creates = %d, want exactly 1", cr)
			}
			if _, del := f.forge.counts(f.branchRef); del != 1 {
				t.Fatalf("branch ref deletes = %d, want exactly 1", del)
			}
		})
	}
}

// TestSettlementBeforeSupersessionLiveDB: settlement wins the lock first (hold discarded, settle
// paused at its delete while a publish trigger fails its try-lock): the record ends deleted by
// deleting the branch ref, and a later supersession attempt finds it deleted and creates no
// recovery ref. Exactly one ref deleted, none orphaned.
func TestSettlementBeforeSupersessionLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	f.discardHold(t)
	p := pauseAt(f.svc1, "delete")
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.svc1.SettleRetainedCheckpoint(f.oldRun)
	}()
	p.waitPaused(t)
	if res := f.publishNew(t, f.svc2); res.Published || res.Skipped != "not_descendant" {
		t.Errorf("Publish while the settle holds the lock = %+v, want the not_descendant skip", res)
	}
	close(p.resume)
	<-done
	if r := f.row(t); r.State != "deleted" || r.Ref != f.branchRef || r.VerifyAfter.Valid {
		t.Fatalf("record = {state %q ref %q verify_after %v}, want deleted at the branch ref", r.State, r.Ref, r.VerifyAfter)
	}
	freed, acquired, err := f.svc2.supersedeRetainedCheckpoint(f.e.ctx, f.oldRun)
	if err != nil || !acquired || freed {
		t.Fatalf("supersede after settlement: freed=%v acquired=%v err=%v, want false/true/nil", freed, acquired, err)
	}
	if res := f.publishNew(t, f.svc2); !res.Published {
		t.Fatalf("Publish after settlement = %+v, want published", res)
	}
	if _, creates := f.forge.calls(); creates != 0 {
		t.Fatalf("create calls = %d, want 0: a settled record gets no recovery ref", creates)
	}
	if _, del := f.forge.counts(f.branchRef); del != 1 {
		t.Fatalf("branch ref deletes = %d, want exactly 1", del)
	}
	if _, ok := f.forge.ref(f.recoveryRef); ok {
		t.Fatalf("an orphaned recovery ref exists")
	}
}

// TestSupersessionThenHoldDiscardedSettlesRecoveryRefLiveDB: supersession first, the hold
// discarded while it is paused before its create: step 4 finds no hold and settles the recovery
// ref under the same lock. Exactly one ref of each deleted, none orphaned.
func TestSupersessionThenHoldDiscardedSettlesRecoveryRefLiveDB(t *testing.T) {
	f := newSupersedeFix(t)
	p := pauseAt(f.svc1, "create")
	done := make(chan PublishResult, 1)
	go func() { done <- f.publishNew(t, f.svc1) }()
	p.waitPaused(t)
	f.discardHold(t)
	close(p.resume)
	if res := <-done; !res.Published {
		t.Fatalf("Publish = %+v, want published", res)
	}
	if r := f.row(t); r.State != "deleted" || r.Ref != f.recoveryRef {
		t.Fatalf("record = {state %q ref %q}, want deleted at the recovery ref", r.State, r.Ref)
	}
	if cr, del := f.forge.counts(f.recoveryRef); cr != 1 || del != 1 {
		t.Fatalf("recovery ref creates/deletes = %d/%d, want 1/1", cr, del)
	}
	if _, del := f.forge.counts(f.branchRef); del != 1 {
		t.Fatalf("branch ref deletes = %d, want 1", del)
	}
}

// --- withRetentionLock: unlock and fence without a killed session (M1 review N3, N4) -----------

// TestRetentionUnlockNotHeldDestroysConnLiveDB (N3): when the deferred unlock reports the lock was
// not held (here: a hook released it inside the operation), the connection is destroyed, never
// returned to the pool: its backend ends. Control: an ordinary operation's backend stays pooled.
func TestRetentionUnlockNotHeldDestroysConnLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	backendAlive := func(pid int32) bool {
		var n int
		if err := f.e.pool.QueryRow(f.e.ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid = $1`, pid).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		return n > 0
	}
	run := func(unlockInside bool) int32 {
		runID := uuid.New()
		var pid int32
		f.svc.retentionHooks = &retentionTestHooks{afterLock: func(id uuid.UUID, conn *pgxpool.Conn) {
			if err := conn.QueryRow(f.e.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Errorf("backend pid: %v", err)
			}
			if unlockInside {
				var ok bool
				if err := conn.QueryRow(f.e.ctx, "SELECT pg_advisory_unlock($1, $2)",
					store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(id)).Scan(&ok); err != nil || !ok {
					t.Errorf("hook unlock: ok=%v err=%v", ok, err)
				}
			}
		}}
		defer func() { f.svc.retentionHooks = nil }()
		acquired, err := f.svc.withRetentionLock(f.e.ctx, runID, func(context.Context, func(context.Context) error) error { return nil })
		if err != nil || !acquired {
			t.Fatalf("withRetentionLock: acquired=%v err=%v", acquired, err)
		}
		return pid
	}

	pooled := run(false)
	if !backendAlive(pooled) {
		t.Fatalf("control: the backend of a cleanly unlocked connection is gone; it should be back in the pool")
	}
	destroyed := run(true)
	deadline := time.Now().Add(10 * time.Second)
	for backendAlive(destroyed) {
		if time.Now().After(deadline) {
			t.Fatalf("backend %d still alive 10s after its unlock returned false: the connection was returned to the pool", destroyed)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRetentionFenceSeesLockReleasedWithoutKillLiveDB (N4): the lock released on the pinned
// session WITHOUT ending it; the fence's pg_locks count is 0 and it returns ErrRetentionLockLost
// itself (the n == 0 branch, not a query error).
func TestRetentionFenceSeesLockReleasedWithoutKillLiveDB(t *testing.T) {
	f := newRetentionFix(t)
	runID := uuid.New()
	f.svc.retentionHooks = &retentionTestHooks{afterLock: func(id uuid.UUID, conn *pgxpool.Conn) {
		var ok bool
		if err := conn.QueryRow(f.e.ctx, "SELECT pg_advisory_unlock($1, $2)",
			store.CheckpointRetentionLockClass, store.CheckpointRetentionLockObjID(id)).Scan(&ok); err != nil || !ok {
			t.Errorf("hook unlock: ok=%v err=%v", ok, err)
		}
	}}
	var fenceErr error
	acquired, err := f.svc.withRetentionLock(f.e.ctx, runID, func(ctx context.Context, fence func(context.Context) error) error {
		fenceErr = fence(ctx)
		return nil
	})
	f.svc.retentionHooks = nil
	if err != nil || !acquired {
		t.Fatalf("withRetentionLock: acquired=%v err=%v", acquired, err)
	}
	// The bare sentinel text (no wrapped query error) proves the n == 0 branch.
	if !errors.Is(fenceErr, ErrRetentionLockLost) || fenceErr.Error() != ErrRetentionLockLost.Error() {
		t.Fatalf("fence = %v, want exactly ErrRetentionLockLost (the pg_locks count, not a query error)", fenceErr)
	}
	if !f.tryFromOtherSession(t, runID) {
		t.Fatalf("the lock is held after the operation")
	}
}
