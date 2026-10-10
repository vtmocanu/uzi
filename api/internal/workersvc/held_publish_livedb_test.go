package workersvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2545 M2: step A of held-work publication against a REAL Postgres, with the broker (pack
// pre-verify, the create push, the listing) replaced by a recording fake forge. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

const (
	heldTestTip  = "3333333333333333333333333333333333333333"
	heldTestTip2 = "4444444444444444444444444444444444444444"
	heldTestCov  = "5555555555555555555555555555555555555555555555555555555555555555"
)

// heldFake is the recording fake forge behind the three broker seams.
type heldFake struct {
	mu       sync.Mutex
	prepares int
	sends    int
	lists    int
	opts     []pushbroker.HeldPackOptions
	refs     map[string]string // the forge's refs, ref -> tip
	listErr  error
	// byPrepared maps a prepared pack back to the options it was built from, so the default
	// send can apply the ref the way a forge would.
	byPrepared map[*pushbroker.PreparedHeldPack]pushbroker.HeldPackOptions

	prepareFn func(o pushbroker.HeldPackOptions) error
	sendFn    func(ctx context.Context, o pushbroker.HeldPackOptions) (pushbroker.HeldCreateOutcome, error)
}

func (f *heldFake) counts() (prepares, sends, lists int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares, f.sends, f.lists
}

func (f *heldFake) setRef(ref, tip string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refs[ref] = tip
}

func (f *heldFake) prepare(_ context.Context, o pushbroker.HeldPackOptions) (*pushbroker.PreparedHeldPack, error) {
	f.mu.Lock()
	f.prepares++
	f.opts = append(f.opts, o)
	fn := f.prepareFn
	f.mu.Unlock()
	if fn != nil {
		if err := fn(o); err != nil {
			return nil, err
		}
	}
	p := &pushbroker.PreparedHeldPack{}
	f.mu.Lock()
	f.byPrepared[p] = o
	f.mu.Unlock()
	return p, nil
}

func (f *heldFake) send(ctx context.Context, p *pushbroker.PreparedHeldPack) (pushbroker.HeldCreateOutcome, error) {
	f.mu.Lock()
	f.sends++
	o := f.byPrepared[p]
	fn := f.sendFn
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, o)
	}
	f.setRef(pushbroker.HeldRef(o.RunID, o.Generation), o.Tip)
	return pushbroker.HeldCreateCreated, nil
}

func (f *heldFake) list(_ context.Context, _ pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := map[string]string{}
	for _, r := range refs {
		if tip, ok := f.refs[r]; ok {
			out[r] = tip
		}
	}
	return out, nil
}

type heldFix struct {
	t      *testing.T
	e      interlockLiveDB
	svc    *Service
	fake   *heldFake
	pat    string
	worker store.Worker
	run    uuid.UUID
	hold   uuid.UUID
	iid    int64
}

func newHeldFix(t *testing.T) *heldFix {
	t.Helper()
	e := setupInterlockLiveDB(t)
	box := newBox(t)
	// Assembled at runtime: no token-shaped literal in tracked source.
	pat := strings.Join([]string{"bot", "pat", "held", uuid.NewString()}, "-")
	sealed, err := box.Seal([]byte(pat))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	e.exec(t, `UPDATE forge_connections SET token_ciphertext = $1
	           WHERE id = (SELECT connection_id FROM repos WHERE id = $2)`, sealed, e.repoID)

	p := testParams()
	p.HeldPublication = true
	svc := New(e.q, box, p)
	svc.SetTxBeginner(e.pool)
	svc.SetForgeBaseURLAllowed(func(u string) bool { return u == "https://forge.e2e" })
	fake := &heldFake{refs: map[string]string{}, byPrepared: map[*pushbroker.PreparedHeldPack]pushbroker.HeldPackOptions{}}
	svc.heldPrepareFn, svc.heldSendFn, svc.heldListFn = fake.prepare, fake.send, fake.list

	caps := []string{capability.RecoveryHeldPublicationV1}
	wid := e.seedWorker(t, caps)
	f := &heldFix{t: t, e: e, svc: svc, fake: fake, pat: pat, worker: store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: caps}}
	f.iid = *e.nextIID
	f.run = e.seedLegacyRunningRun(t, wid)
	f.hold = uuid.New()
	e.exec(t, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
		VALUES ($1,$2,$3,$4,1,'open',$5,'held-ident',$5,$4,true)`, f.hold, e.userID, e.repoID, f.run, wid)
	e.exec(t, `UPDATE runs SET status='failed', fail_origin='agent_failure', claim_generation=1 WHERE id=$1`, f.run)
	return f
}

// rawHold runs an UPDATE on the hold with recovery_inventory_hold_guard disabled, to build a hold
// state the guard would not let a real writer reach.
func (f *heldFix) rawHold(sql string, args ...any) {
	f.t.Helper()
	tx, err := f.e.pool.Begin(f.e.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.e.ctx) }()
	for _, q := range []string{`ALTER TABLE recovery_custody_holds DISABLE TRIGGER recovery_inventory_hold_guard`, sql,
		`ALTER TABLE recovery_custody_holds ENABLE TRIGGER recovery_inventory_hold_guard`} {
		a := []any(nil)
		if q == sql {
			a = args
		}
		if _, err := tx.Exec(f.e.ctx, q, a...); err != nil {
			f.t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Commit(f.e.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *heldFix) req() HeldPublishRequest {
	return HeldPublishRequest{Tip: heldTestTip, Generation: 1, Coverage: heldTestCov}
}

func (f *heldFix) ref() string { return pushbroker.HeldRef(f.run, 1) }

func (f *heldFix) publish(req HeldPublishRequest) (HeldPublishResult, error) {
	return f.svc.PublishHeld(f.e.ctx, f.worker, f.run, req, []byte("pack"))
}

func (f *heldFix) row() (store.RunHeldPublication, bool) {
	f.t.Helper()
	r, err := f.e.q.GetHeldPublicationByRunGeneration(f.e.ctx, store.GetHeldPublicationByRunGenerationParams{RunID: f.run, Generation: 1})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.RunHeldPublication{}, false
	}
	if err != nil {
		f.t.Fatalf("read publication: %v", err)
	}
	return r, true
}

func (f *heldFix) mustRow() store.RunHeldPublication {
	f.t.Helper()
	r, ok := f.row()
	if !ok {
		f.t.Fatal("no run_held_publications row")
	}
	return r
}

func (f *heldFix) assertHoldOpen() {
	f.t.Helper()
	var state string
	var disp *string
	if err := f.e.pool.QueryRow(f.e.ctx, `SELECT state, final_disposition FROM recovery_custody_holds WHERE id=$1`, f.hold).Scan(&state, &disp); err != nil {
		f.t.Fatal(err)
	}
	if state != "open" || disp != nil {
		f.t.Fatalf("hold = %s/%v, want open and unclassified: step A never releases the hold", state, disp)
	}
}

func heldReason(t *testing.T, err error) string {
	t.Helper()
	var r *HeldRefusal
	if !errors.As(err, &r) {
		t.Fatalf("err = %v, want a *HeldRefusal", err)
	}
	return r.Reason
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A failed run's step A creates the ref once: the row is created with the identity the SERVER
// derived (repo, user, worker from the hold and run), the live pointer is set, the credential
// reached the broker only from the sealed connection, and the hold is untouched.
func TestHeldPublishHappyPathLiveDB(t *testing.T) {
	f := newHeldFix(t)
	if res, err := f.svc.HeldPublishGate(f.e.ctx, f.worker, f.run, f.req()); err != nil || res != nil {
		t.Fatalf("gate = %+v, %v; want to proceed to the body", res, err)
	}
	res, err := f.publish(f.req())
	if err != nil || res.State != "created" || res.Ref != f.ref() || res.Tip != heldTestTip || res.Reason != "" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	row := f.mustRow()
	if row.State != "created" || !row.LiveRunID.Valid || uuid.UUID(row.LiveRunID.Bytes) != f.run || !row.CreateInvokedAt.Valid ||
		!row.RefCreatedAt.Valid || row.HoldID != f.hold || row.RepoID != f.e.repoID || row.UserID != f.e.userID ||
		row.WorkerID != f.worker.ID || row.Tip != heldTestTip || row.CoverageDigest != heldTestCov {
		t.Fatalf("row = %+v", row)
	}
	f.assertHoldOpen()
	pre, sends, _ := f.fake.counts()
	if pre != 1 || sends != 1 {
		t.Fatalf("prepares=%d sends=%d, want 1 and 1", pre, sends)
	}
	o := f.fake.opts[0]
	if o.PAT != f.pat || o.Username != "bot" || o.CloneURL != "https://forge.e2e/g/interlock.git" || o.RunID != f.run ||
		o.Generation != 1 || o.Tip != heldTestTip || o.Branch != fmt.Sprintf("agent/issue-%d", f.iid) || o.DefaultBranch != "main" {
		t.Fatalf("broker options = %+v (PAT redacted in the comparison)", o)
	}
}

// Step A's scope (D1) is enforced before the body is read (the gate) and again under the locks
// (PublishHeld), and a refused request touches neither the broker nor the table.
func TestHeldPublishScopeRefusalsLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(f *heldFix)
		reason string
		owned  bool // false: ErrRunNotOwned
	}{
		{"cancelled run", func(f *heldFix) { f.e.exec(t, `UPDATE runs SET status='cancelled' WHERE id=$1`, f.run) }, HeldReasonNotFailed, true},
		{"push_secret_blocked", func(f *heldFix) {
			f.e.exec(t, `UPDATE runs SET fail_origin='push_secret_blocked' WHERE id=$1`, f.run)
		}, HeldReasonExcludedOrigin, true},
		{"worker_residue_blocked", func(f *heldFix) {
			f.e.exec(t, `UPDATE runs SET fail_origin='worker_residue_blocked' WHERE id=$1`, f.run)
		}, HeldReasonExcludedOrigin, true},
		{"lost generation g, failed g+1", func(f *heldFix) { f.e.exec(t, `UPDATE runs SET claim_generation=2 WHERE id=$1`, f.run) }, HeldReasonGenerationMismatch, true},
		{"run re-claimed by another worker", func(f *heldFix) {
			other := f.e.seedWorker(t, []string{capability.RecoveryHeldPublicationV1})
			f.e.exec(t, `UPDATE runs SET worker_id=$2 WHERE id=$1`, f.run, other)
		}, "", false},
		{"worker without the capability", func(f *heldFix) { f.worker.ProtocolCapabilities = nil }, HeldReasonUnsupported, true},
		{"switch off", func(f *heldFix) { f.svc.p.HeldPublication = false }, HeldReasonUnsupported, true},
		{"hold discarded", func(f *heldFix) {
			f.rawHold(`UPDATE recovery_custody_holds SET state='discarded', live_worker_id=NULL, live_run_id=NULL,
				release_evidence='owner_discard', released_at=now() WHERE id=$1`, f.hold)
		}, HeldReasonHoldMissing, true},
		{"hold not guarded", func(f *heldFix) {
			f.rawHold(`UPDATE recovery_custody_holds SET inventory_guarded=false WHERE id=$1`, f.hold)
		}, HeldReasonHoldMissing, true},
		{"hold live on another worker", func(f *heldFix) {
			other := f.e.seedWorker(t, nil)
			f.rawHold(`UPDATE recovery_custody_holds SET live_worker_id=$2 WHERE id=$1`, f.hold, other)
		}, HeldReasonHoldMissing, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHeldFix(t)
			tc.mutate(f)
			check := func(op string, err error) {
				t.Helper()
				if !tc.owned {
					if !errors.Is(err, ErrRunNotOwned) {
						t.Fatalf("%s: err = %v, want ErrRunNotOwned", op, err)
					}
				} else if got := heldReason(t, err); got != tc.reason {
					t.Fatalf("%s: reason = %q, want %q", op, got, tc.reason)
				}
			}
			_, err := f.svc.HeldPublishGate(f.e.ctx, f.worker, f.run, f.req())
			check("gate", err)
			_, err = f.publish(f.req())
			check("publish", err)
			if pre, sends, lists := f.fake.counts(); pre+sends+lists != 0 {
				t.Fatalf("broker used by a refused request: prepares=%d sends=%d lists=%d", pre, sends, lists)
			}
			if _, ok := f.row(); ok {
				t.Fatal("a refused request left a publication row")
			}
		})
	}
	t.Run("generation the hold does not cover", func(t *testing.T) {
		f := newHeldFix(t)
		f.e.exec(t, `UPDATE runs SET claim_generation=2 WHERE id=$1`, f.run)
		req := f.req()
		req.Generation = 2
		if _, err := f.publish(req); heldReason(t, err) != HeldReasonHoldMissing {
			t.Fatalf("err = %v, want hold_missing", err)
		}
	})
}

// The D1 scope is re-checked under the locks: a run that stops being failed between the unlocked
// gate and tx1 sends nothing and leaves no row.
func TestHeldPublishScopeRecheckedInTx1LiveDB(t *testing.T) {
	f := newHeldFix(t)
	f.fake.prepareFn = func(pushbroker.HeldPackOptions) error {
		f.e.exec(t, `UPDATE runs SET status='cancelled' WHERE id=$1`, f.run)
		return nil
	}
	_, err := f.publish(f.req())
	if heldReason(t, err) != HeldReasonNotFailed {
		t.Fatalf("err = %v, want not_failed from tx1", err)
	}
	if _, sends, _ := f.fake.counts(); sends != 0 {
		t.Fatalf("sends = %d, want 0", sends)
	}
	if _, ok := f.row(); ok {
		t.Fatal("tx1 left a row for a run that is no longer failed")
	}
}

// A request that names a different tip or coverage than the existing publication is refused with
// identity_changed and never sends. Covered at the gate (an existing row), at PublishHeld, and in
// tx1 (a row appears between the unlocked admission and the locks).
func TestHeldPublishIdentityChangedLiveDB(t *testing.T) {
	t.Run("existing created row", func(t *testing.T) {
		f := newHeldFix(t)
		if res, err := f.publish(f.req()); err != nil || res.State != "created" {
			t.Fatalf("first: %+v %v", res, err)
		}
		for _, req := range []HeldPublishRequest{
			{Tip: heldTestTip2, Generation: 1, Coverage: heldTestCov},
			{Tip: heldTestTip, Generation: 1, Coverage: strings.Repeat("6", 64)},
		} {
			if _, err := f.svc.HeldPublishGate(f.e.ctx, f.worker, f.run, req); heldReason(t, err) != HeldReasonIdentityChanged {
				t.Fatalf("gate err = %v", err)
			}
			if _, err := f.publish(req); heldReason(t, err) != HeldReasonIdentityChanged {
				t.Fatalf("publish err = %v", err)
			}
		}
		if _, sends, _ := f.fake.counts(); sends != 1 {
			t.Fatalf("sends = %d, want only the first", sends)
		}
	})
	t.Run("row appears after admission", func(t *testing.T) {
		f := newHeldFix(t)
		f.fake.prepareFn = func(pushbroker.HeldPackOptions) error {
			// A concurrent request recorded a DIFFERENT prepared publication for this generation.
			f.e.exec(t, `INSERT INTO run_held_publications
				(id,run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state)
				VALUES (gen_random_uuid(),$1,1,$2,$3,$4,$5,$1,$6,$7,$8,'prepared')`,
				f.run, f.hold, f.e.userID, f.e.repoID, f.worker.ID, f.ref(), heldTestTip2, heldTestCov)
			return nil
		}
		if _, err := f.publish(f.req()); heldReason(t, err) != HeldReasonIdentityChanged {
			t.Fatalf("err = %v, want identity_changed from tx1", err)
		}
		if _, sends, _ := f.fake.counts(); sends != 0 {
			t.Fatalf("sends = %d, want 0", sends)
		}
		if r := f.mustRow(); r.State != "prepared" || r.CreateInvokedAt.Valid || r.Tip != heldTestTip2 {
			t.Fatalf("the foreign row was altered: %+v", r)
		}
	})
}

// The marker is committed before the network call: a second connection sees state invoked with
// create_invoked_at set while the fake send is in flight.
func TestHeldPublishMarkerVisibleBeforeSendLiveDB(t *testing.T) {
	f := newHeldFix(t)
	var state string
	var marker bool
	f.fake.sendFn = func(ctx context.Context, o pushbroker.HeldPackOptions) (pushbroker.HeldCreateOutcome, error) {
		if err := f.e.pool.QueryRow(ctx, `SELECT state, create_invoked_at IS NOT NULL FROM run_held_publications WHERE run_id=$1`, f.run).Scan(&state, &marker); err != nil {
			t.Errorf("second connection read: %v", err)
		}
		f.fake.setRef(f.ref(), o.Tip)
		return pushbroker.HeldCreateCreated, nil
	}
	if res, err := f.publish(f.req()); err != nil || res.State != "created" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if state != "invoked" || !marker {
		t.Fatalf("second connection saw state=%q marker=%t while the send was in flight, want invoked and set", state, marker)
	}
}

// A failure after the invocation is recorded (tx2 rolls back) leaves the row invoked, and every
// later request only reconciles: zero further sends, even when the ref is absent.
func TestHeldPublishTx2FailureNeverResendsLiveDB(t *testing.T) {
	f := newHeldFix(t)
	f.svc.heldOutcomeHook = func() error { return errors.New("injected rollback") }
	f.fake.sendFn = func(_ context.Context, _ pushbroker.HeldPackOptions) (pushbroker.HeldCreateOutcome, error) {
		return pushbroker.HeldCreateUnknown, errors.New("response lost") // the forge did not apply it
	}
	res, err := f.publish(f.req())
	if err != nil || res.State != "invoked" {
		t.Fatalf("result = %+v, %v; want the row left invoked", res, err)
	}
	if r := f.mustRow(); r.State != "invoked" || !r.CreateInvokedAt.Valid {
		t.Fatalf("row after the failed tx2 = %+v", r)
	}
	f.svc.heldOutcomeHook = nil
	for i := 0; i < 2; i++ {
		res, err = f.publish(f.req())
		if err != nil || res.State != "invoked" {
			t.Fatalf("retry %d: %+v, %v; want a reconcile-only answer", i, res, err)
		}
	}
	pre, sends, lists := f.fake.counts()
	if sends != 1 || pre != 1 || lists != 2 {
		t.Fatalf("prepares=%d sends=%d lists=%d, want 1 prepare, 1 send and a listing per retry", pre, sends, lists)
	}
	// The forge applied the push after all: the next reconcile records created, still one send.
	f.fake.setRef(f.ref(), heldTestTip)
	if res, err = f.publish(f.req()); err != nil || res.State != "created" {
		t.Fatalf("after the ref appeared: %+v, %v", res, err)
	}
	if _, sends, _ = f.fake.counts(); sends != 1 {
		t.Fatalf("sends = %d, want 1", sends)
	}
}

// A replay while the first create is still blocked in the forge is answered from the pre-body
// gate: zero sends and zero base fetches (the gate short-circuits before the pack is touched).
func TestHeldPublishReplayWhileCreateBlockedLiveDB(t *testing.T) {
	f := newHeldFix(t)
	inSend, release := make(chan struct{}), make(chan struct{})
	f.fake.sendFn = func(_ context.Context, o pushbroker.HeldPackOptions) (pushbroker.HeldCreateOutcome, error) {
		close(inSend)
		<-release
		f.fake.setRef(f.ref(), o.Tip)
		return pushbroker.HeldCreateCreated, nil
	}
	first := make(chan HeldPublishResult, 1)
	go func() {
		res, err := f.publish(f.req())
		if err != nil {
			t.Errorf("first request: %v", err)
		}
		first <- res
	}()
	select {
	case <-inSend:
	case <-time.After(15 * time.Second):
		t.Fatal("the first create never reached the forge")
	}
	preBefore, _, _ := f.fake.counts()

	gate, err := f.svc.HeldPublishGate(f.e.ctx, f.worker, f.run, f.req())
	if err != nil || gate == nil || gate.State != "invoked" {
		t.Fatalf("gate during the blocked create = %+v, %v; want a reconcile-only invoked answer", gate, err)
	}
	res, err := f.publish(f.req())
	if err != nil || res.State != "invoked" {
		t.Fatalf("replay = %+v, %v; want invoked", res, err)
	}
	if pre, sends, _ := f.fake.counts(); sends != 1 || pre != preBefore {
		t.Fatalf("replay: prepares=%d (was %d) sends=%d, want zero base fetches and zero sends", pre, preBefore, sends)
	}
	close(release)
	select {
	case res := <-first:
		if res.State != "created" {
			t.Fatalf("first request = %+v", res)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("first request never finished")
	}
}

// Two requests that both pass the unlocked admission race to the marker: exactly one sends.
func TestHeldPublishConcurrentRequestsOneSendLiveDB(t *testing.T) {
	f := newHeldFix(t)
	var barrier sync.WaitGroup
	barrier.Add(2)
	f.fake.prepareFn = func(pushbroker.HeldPackOptions) error {
		barrier.Done()
		barrier.Wait() // both have passed admission before either reaches tx1
		return nil
	}
	var wg sync.WaitGroup
	results := make([]HeldPublishResult, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := f.publish(f.req())
			if err != nil {
				t.Errorf("request %d: %v", i, err)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	if pre, sends, _ := f.fake.counts(); pre != 2 || sends != 1 {
		t.Fatalf("prepares=%d sends=%d, want both requests prepared and exactly one send", pre, sends)
	}
	if r := f.mustRow(); r.Attempts < 1 || !r.CreateInvokedAt.Valid {
		t.Fatalf("row = %+v", r)
	}
}

// A forge that applies the push after the client's deadline: the outcome is create_unknown (the
// create may have landed), and a later reconcile that lists the ref at the tip records created.
// Absence of the ref never makes an unknown outcome terminal.
func TestHeldPublishUnknownThenReconcileLiveDB(t *testing.T) {
	f := newHeldFix(t)
	f.fake.sendFn = func(_ context.Context, _ pushbroker.HeldPackOptions) (pushbroker.HeldCreateOutcome, error) {
		return pushbroker.HeldCreateUnknown, context.DeadlineExceeded
	}
	res, err := f.publish(f.req())
	if err != nil || res.State != "create_unknown" || res.Reason != "" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	row := f.mustRow()
	if !row.LiveRunID.Valid || !row.NextAttemptAt.Valid || !row.NextAttemptAt.Time.After(time.Now().Add(30*time.Minute)) ||
		!strings.Contains(row.LastError.String, "deadline") {
		t.Fatalf("create_unknown row = %+v", row)
	}
	// The ref is absent: repeated reconciles through both entry points stay create_unknown.
	for i := 0; i < 3; i++ {
		gate, err := f.svc.HeldPublishGate(f.e.ctx, f.worker, f.run, f.req())
		if err != nil || gate == nil || gate.State != "create_unknown" {
			t.Fatalf("gate %d = %+v, %v", i, gate, err)
		}
		if res, err = f.publish(f.req()); err != nil || res.State != "create_unknown" {
			t.Fatalf("reconcile %d = %+v, %v", i, res, err)
		}
	}
	if r := f.mustRow(); r.State != "create_unknown" || !r.LiveRunID.Valid {
		t.Fatalf("an absent ref made the unknown outcome terminal: %+v", r)
	}
	// A listing failure is reported as retryable and changes nothing.
	f.fake.listErr = errors.New("forge down")
	if res, err = f.publish(f.req()); err != nil || res.State != "create_unknown" || res.Reason != HeldReasonForgeUnavailable {
		t.Fatalf("listing failure = %+v, %v", res, err)
	}
	f.fake.listErr = nil
	// The forge applied the push after the deadline.
	f.fake.setRef(f.ref(), heldTestTip)
	if res, err = f.publish(f.req()); err != nil || res.State != "created" {
		t.Fatalf("after the late apply: %+v, %v", res, err)
	}
	if r := f.mustRow(); r.State != "created" || !r.RefCreatedAt.Valid || r.LastError.Valid {
		t.Fatalf("row = %+v", r)
	}
	if _, sends, _ := f.fake.counts(); sends != 1 {
		t.Fatalf("sends = %d, want 1", sends)
	}
}

// A definitive ng from the remote is recorded as refused: the live pointer is cleared (no ref
// exists), the hold stays open, and a replay reports refused without sending.
func TestHeldPublishBrokerRefusedLiveDB(t *testing.T) {
	f := newHeldFix(t)
	f.fake.sendFn = func(_ context.Context, _ pushbroker.HeldPackOptions) (pushbroker.HeldCreateOutcome, error) {
		return pushbroker.HeldCreateRefused, fmt.Errorf("%w: hook declined", pushbroker.ErrHeldCreateRefused)
	}
	res, err := f.publish(f.req())
	if err != nil || res.State != "refused" || res.Reason != HeldReasonCreateRefused {
		t.Fatalf("result = %+v, %v", res, err)
	}
	row := f.mustRow()
	if row.State != "refused" || row.LiveRunID.Valid || row.RefusalReason.String != HeldReasonCreateRefused ||
		!strings.Contains(row.LastError.String, "hook declined") {
		t.Fatalf("refused row = %+v", row)
	}
	f.assertHoldOpen()
	if res, err = f.publish(f.req()); err != nil || res.State != "refused" {
		t.Fatalf("replay = %+v, %v", res, err)
	}
	if _, sends, _ := f.fake.counts(); sends != 1 {
		t.Fatalf("sends = %d, want 1", sends)
	}
}

// The remote's ng reason and a transport error are untrusted: what is stored passes the secret
// scrubber and carries no newline, ESC, C1, bidi or other control character.
func TestHeldPublishStoredErrorIsSanitizedLiveDB(t *testing.T) {
	f := newHeldFix(t)
	// Assembled at runtime: no token-shaped literal in tracked source.
	secret := "glpat-" + strings.Repeat("a1b2", 5)
	hostile := "remote: " + secret + "\nline two\x1b[31mred\x1b[0m‮evil\u0085next end\x00nul " + strings.Repeat("x", 2000)
	f.fake.sendFn = func(_ context.Context, _ pushbroker.HeldPackOptions) (pushbroker.HeldCreateOutcome, error) {
		return pushbroker.HeldCreateRefused, fmt.Errorf("%w: %s", pushbroker.ErrHeldCreateRefused, hostile)
	}
	if res, err := f.publish(f.req()); err != nil || res.State != "refused" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	got := f.mustRow().LastError.String
	if got == "" || len(got) > 512 || strings.Contains(got, secret) || strings.Contains(got, "a1b2a1b2") {
		t.Fatalf("stored last_error = %q", got)
	}
	for _, r := range got {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x202e || r == 0x2028 {
			t.Fatalf("stored last_error holds control rune %U: %q", r, got)
		}
	}
	if strings.Contains(got, "[31m") || !strings.Contains(got, "line two") {
		t.Fatalf("stored last_error = %q: the ANSI sequence must go, the text must stay", got)
	}
}

// The SSRF gate runs before the credential is decrypted or any host is dialed: with a base URL the
// allowlist refuses, step A (and the reconcile of an invoked row) makes no broker call, and the
// refusal comes from the allowlist even though the stored ciphertext could not be decrypted.
func TestHeldPublishSSRFBeforeDecryptLiveDB(t *testing.T) {
	f := newHeldFix(t)
	f.e.exec(t, `UPDATE forge_connections SET token_ciphertext = $1
	             WHERE id = (SELECT connection_id FROM repos WHERE id = $2)`, []byte("not-a-sealed-secret"), f.e.repoID)
	f.svc.SetForgeBaseURLAllowed(func(string) bool { return false })
	if _, err := f.svc.heldRemote(f.e.ctx, f.run); err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("heldRemote err = %v, want the allowlist refusal (not a decrypt error)", err)
	}
	if _, err := f.publish(f.req()); heldReason(t, err) != HeldReasonForgeUnavailable {
		t.Fatalf("publish err = %v, want forge_unavailable", err)
	}
	if pre, sends, lists := f.fake.counts(); pre+sends+lists != 0 {
		t.Fatalf("broker dialed: prepares=%d sends=%d lists=%d", pre, sends, lists)
	}
	if _, ok := f.row(); ok {
		t.Fatal("a request refused before the pre-verify left a row")
	}
	// An invoked row reconciles through the same gate: no listing, state reported as is.
	f.e.exec(t, `INSERT INTO run_held_publications
		(id,run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state)
		VALUES (gen_random_uuid(),$1,1,$2,$3,$4,$5,$1,$6,$7,$8,'prepared')`,
		f.run, f.hold, f.e.userID, f.e.repoID, f.worker.ID, f.ref(), heldTestTip, heldTestCov)
	f.e.exec(t, `UPDATE run_held_publications SET state='invoked', create_invoked_at=now() WHERE run_id=$1`, f.run)
	gate, err := f.svc.HeldPublishGate(f.e.ctx, f.worker, f.run, f.req())
	if err != nil || gate == nil || gate.State != "invoked" || gate.Reason != HeldReasonForgeUnavailable {
		t.Fatalf("gate = %+v, %v", gate, err)
	}
	if _, _, lists := f.fake.counts(); lists != 0 {
		t.Fatalf("lists = %d, want 0", lists)
	}
}

// The process-wide semaphore refuses the request over the cap without waiting, and a release
// frees a slot.
func TestHeldPublishSlotsBounded(t *testing.T) {
	svc := New(nil, nil, Params{})
	var releases []func()
	for i := 0; i < heldPublishMaxConcurrent; i++ {
		rel, ok := svc.AcquireHeldPublishSlot()
		if !ok {
			t.Fatalf("slot %d refused below the cap", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := svc.AcquireHeldPublishSlot(); ok {
		t.Fatal("slot granted over the cap")
	}
	releases[0]()
	rel, ok := svc.AcquireHeldPublishSlot()
	if !ok {
		t.Fatal("no slot after a release")
	}
	rel()
}

func TestSanitizeHeldText(t *testing.T) {
	secret := "ghp_" + strings.Repeat("Ab3", 8)
	for in, want := range map[string]string{
		"plain text":             "plain text",
		"a\nb\r\nc\td":           "a b c d",
		"x\x1b[31mred\x1b[0my":   "x red y",
		"bidi‮RLO⁦iso​":          "bidi RLO iso",
		"c1\u0085\u009bend":      "c1 end",
		"tok " + secret + " end": "tok [redacted] end",
	} {
		got := sanitizeHeldText(in)
		if strings.Contains(got, secret) {
			t.Errorf("sanitize(%q) = %q leaks the token", in, got)
		}
		if got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sanitizeHeldText(strings.Repeat("é", 1000)); len([]rune(got)) != heldLastErrorMax {
		t.Errorf("not bounded: %d runes", len([]rune(got)))
	}
}
