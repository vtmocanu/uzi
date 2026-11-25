package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The fresh claim (issue #4628, plan section 4) against a real Postgres: the candidate exclusions, the
// skip rules, the R1 headroom refusal, the claimed transfer and its payload, and the V1 assembly refusals.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

// freshRecorder records what a claim publishes: the broadcast states and the lifecycle notifications.
type freshRecorder struct {
	mu       sync.Mutex
	states   []string
	notifies []string
}

func (r *freshRecorder) PublishMessage(uuid.UUID, int32, string, string, string, string, []byte, time.Time) {
}
func (r *freshRecorder) PublishState(_ uuid.UUID, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, status)
}
func (r *freshRecorder) PublishHealth(uuid.UUID, string, string, bool) {}
func (r *freshRecorder) PublishInput(uuid.UUID)                        {}
func (r *freshRecorder) Notify(_ uuid.UUID, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifies = append(r.notifies, status)
}

func (r *freshRecorder) seen() (states, notifies []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.states...), append([]string(nil), r.notifies...)
}

// fcFix is a freshFix whose destination can actually claim: the owner has a decryptable default
// Anthropic token and a sealed bot token, and the service publishes into a recorder.
type fcFix struct {
	*freshFix
	rec    *freshRecorder
	secret uuid.UUID
}

func newFCFix(t *testing.T, gate bool) *fcFix {
	t.Helper()
	f := newFreshFix(t, gate, 2)
	f.env.sealBotPAT(t, f.user)
	secret := f.env.seedAnthropicSecret(t, f.user, "fresh-default-"+uuid.NewString(), true)
	rec := &freshRecorder{}
	f.svc.SetBroadcaster(rec)
	f.svc.SetLifecycle(rec)
	return &fcFix{freshFix: f, rec: rec, secret: secret}
}

// pending starts a fresh resume of the fixture's run from a new available capture and returns the transfer id.
func (x *fcFix) pending() uuid.UUID {
	x.t.Helper()
	x.capture(2, "available", freshSha('a'), nil, 2*time.Hour)
	res, err := x.start(x.preview().SelectionToken, uuid.New())
	if err != nil {
		x.t.Fatalf("start: %v", err)
	}
	return res.Row.ID
}

// second is another held run of the same owner, bound to W1 with its own open hold, sharing the fixture.
func (x *fcFix) second() *fcFix {
	x.t.Helper()
	g := *x.freshFix
	g.run = seedOutageRun(x.t, x.env, x.user, x.repo, x.w1, "recovery_wait", "issue", 2, 0)
	x.env.exec(`UPDATE runs SET recovery_wait_cause='worker_requeue_exhausted', claim_released_at=now(), released_worker_id=$2 WHERE id=$1`, g.run, x.w1)
	if err := x.env.pool.QueryRow(x.env.ctx, `SELECT issue_iid FROM runs WHERE id=$1`, g.run).Scan(&g.iid); err != nil {
		x.t.Fatal(err)
	}
	return &fcFix{freshFix: &g, rec: x.rec, secret: x.secret}
}

func (x *fcFix) claimAs(w uuid.UUID) (*ClaimPayload, error) {
	x.t.Helper()
	return x.svc.Claim(x.env.ctx, wkrRow(x.t, x.env, w), nil)
}

func (x *fcFix) mustClaimAs(w uuid.UUID) *ClaimPayload {
	x.t.Helper()
	p, err := x.claimAs(w)
	if err != nil {
		x.t.Fatalf("Claim: %v", err)
	}
	return p
}

// wantIdle asserts a poll claimed nothing, without an error.
func (x *fcFix) wantIdle(what string, w uuid.UUID) {
	x.t.Helper()
	p, err := x.claimAs(w)
	if err != nil || p != nil {
		x.t.Fatalf("%s: Claim = (payload %v, err %v), want an idle poll", what, p != nil, err)
	}
}

func (x *fcFix) state(frid uuid.UUID) string {
	x.t.Helper()
	return x.transfer(frid).State
}

// wantHeld asserts the run is untouched at its held recovery_wait on W1.
func (x *fcFix) wantHeld(what string) {
	x.t.Helper()
	var status string
	var worker pgtype.UUID
	var gen int64
	if err := x.env.pool.QueryRow(x.env.ctx, `SELECT status, worker_id, claim_generation FROM runs WHERE id=$1`, x.run).Scan(&status, &worker, &gen); err != nil {
		x.t.Fatal(err)
	}
	if status != "recovery_wait" || worker.Bytes != x.w1 || gen != 2 {
		x.t.Fatalf("%s: run status=%q worker=%v generation=%d, want recovery_wait on W1 at 2", what, status, worker, gen)
	}
}

// openHoldsOf counts the run's open custody holds by original worker.
func (x *fcFix) openHoldsOf(w uuid.UUID) int {
	x.t.Helper()
	var n int
	if err := x.env.pool.QueryRow(x.env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1 AND state='open' AND original_worker_id=$2`, x.run, w).Scan(&n); err != nil {
		x.t.Fatal(err)
	}
	return n
}

// usedWall is the wall time the run has used: its time in status before the last transition less the
// paused credit. A claim banks the held interval as paused, so this is unchanged by it.
func (x *fcFix) usedWall() float64 {
	x.t.Helper()
	var used float64
	if err := x.env.pool.QueryRow(x.env.ctx,
		`SELECT EXTRACT(EPOCH FROM (status_since - started_at)) - budget_paused_seconds FROM runs WHERE id=$1`, x.run).Scan(&used); err != nil {
		x.t.Fatal(err)
	}
	return used
}

func TestFreshClaimTakesThePendingTransferLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	f.env.exec(`UPDATE runs SET codex_claim_epoch=5, codex_cap_hash=decode('aabb','hex') WHERE id=$1`, f.run)
	usedBefore := f.usedWall()

	p := f.mustClaimAs(f.w2)
	if p == nil {
		t.Fatal("the destination's poll claimed nothing")
	}
	if p.RunID != f.run.String() {
		t.Fatalf("claimed run %s, want %s", p.RunID, f.run)
	}
	if p.FreshResume == nil || p.FreshResume.ID != frid.String() || p.FreshResume.Kind != "capture" ||
		p.FreshResume.Sha != freshSha('a') || p.FreshResume.EvidenceGeneration != 2 || p.FreshResume.CaptureID == "" || p.FreshResume.CheckpointRef != "" {
		t.Fatalf("payload fresh_resume = %+v", p.FreshResume)
	}
	raw, err := json.Marshal(p.FreshResume)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	for _, k := range []string{"id", "kind", "capture_id", "sha", "evidence_generation"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("fresh_resume wire form %s lacks %q", raw, k)
		}
	}

	row := f.transfer(frid)
	if row.State != "claimed" || row.DestinationWorkerID.Bytes != f.w2 || !row.ClaimGeneration.Valid || row.ClaimGeneration.Int64 != 3 ||
		row.PredecessorWorkerID.Bytes != f.w1 || row.SavedWorkerID.Bytes != f.w1 || len(row.SavedTuple) == 0 {
		t.Fatalf("transfer after the claim = %+v", row)
	}
	if d := time.Until(row.TransferDeadline.Time); d < 119*time.Minute || d > 121*time.Minute {
		t.Fatalf("transfer_deadline in %v, want about 2h", d)
	}
	var status string
	var worker pgtype.UUID
	var gen int64
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT status, worker_id, claim_generation FROM runs WHERE id=$1`, f.run).Scan(&status, &worker, &gen); err != nil {
		t.Fatal(err)
	}
	if status != "claimed" || worker.Bytes != f.w2 || gen != 3 {
		t.Fatalf("run after the claim: status=%q worker=%v generation=%d", status, worker, gen)
	}
	if f.openHoldsOf(f.w2) != 1 || f.openHoldsOf(f.w1) != 1 {
		t.Fatalf("holds: W2 %d, W1 %d, want one each (the new attempt's hold and the predecessor's)", f.openHoldsOf(f.w2), f.openHoldsOf(f.w1))
	}
	if used := f.usedWall(); used < usedBefore-1 || used > usedBefore+1 {
		t.Fatalf("remaining wall changed by the claim: used %.1fs before, %.1fs after", usedBefore, used)
	}
}

// freshBothCaps are the protocol capabilities of an eligible fresh resume destination.
var freshBothCaps = []string{capability.FreshResumeV1, capability.RecoveryInventoryV1}

// wantSkipped asserts a poll by claimant took nothing, left the transfer pending and the run held, and
// refused nothing (the hold version did not move).
func (x *fcFix) wantSkipped(what string, frid, claimant uuid.UUID) {
	x.t.Helper()
	v := x.version()
	x.wantIdle(what, claimant)
	if got := x.state(frid); got != "pending" {
		x.t.Fatalf("%s: transfer %q, want pending", what, got)
	}
	x.wantHeld(what)
	if x.version() != v {
		x.t.Fatalf("%s: a skip bumped the hold version (a refusal)", what)
	}
}

// The exclusions of the candidate read: each is a worker the transfer must never go to, proven by an
// idle poll while the exclusion stands and a claim by the same fixture's W2 once it is lifted.
func TestFreshClaimExclusionsLiveDB(t *testing.T) {
	cases := []struct {
		name  string
		apply func(f *fcFix) (claimant uuid.UUID) // sets up the exclusion; returns the worker that must be refused
		lift  func(f *fcFix)                      // removes it, so W2 can take the transfer (the control)
	}{
		{"the predecessor W1", func(f *fcFix) uuid.UUID {
			f.env.exec(`UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', protocol_capabilities=$2, last_heartbeat_at=now() WHERE id=$1`, f.w1, freshBothCaps)
			return f.w1
		}, func(*fcFix) {}},
		{"predecessor bound by worker_id only", func(f *fcFix) uuid.UUID {
			// No released_worker_id, no claim_released_at and no open custody hold name W1: only the
			// run's worker_id binds it, so only the worker_id exclusion refuses it.
			f.env.exec(`UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', protocol_capabilities=$2, last_heartbeat_at=now() WHERE id=$1`, f.w1, freshBothCaps)
			f.env.exec(`UPDATE runs SET released_worker_id=NULL, claim_released_at=NULL WHERE id=$1`, f.run)
			f.env.exec(`UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL, released_at=now() WHERE run_id=$1`, f.run)
			return f.w1
		}, func(*fcFix) {}},
		{"the released worker", func(f *fcFix) uuid.UUID {
			f.env.exec(`UPDATE runs SET released_worker_id=$2 WHERE id=$1`, f.run, f.w2)
			return f.w2
		}, func(f *fcFix) { f.env.exec(`UPDATE runs SET released_worker_id=$2 WHERE id=$1`, f.run, f.w1) }},
		{"a holder of an open hold of the run", func(f *fcFix) uuid.UUID {
			f.hold(1, f.w2)
			return f.w2
		}, func(f *fcFix) {
			f.env.exec(`UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL, released_at=now() WHERE run_id=$1 AND original_worker_id=$2`, f.run, f.w2)
		}},
		{"the excluded predecessor of a seeded transfer", func(f *fcFix) uuid.UUID {
			f.env.exec(`INSERT INTO run_fresh_resumes (run_id, user_id, request_id, evidence_generation, evidence_kind, checkpoint_ref, sha, candidate_token,
			    state, predecessor_worker_id, destination_worker_id, claim_generation, destination_attempt_id, seeded_generation, seeded_at)
			    VALUES ($1, $2, $3, 1, 'checkpoint_ref', 'refs/uzi-checkpoints/x', $4, 'tok', 'seeded', $5, $6, 1, '20260101T000000Z-g1-0123456789abcdef', 1, now())`,
				f.run, f.user, uuid.New(), freshSha('c'), f.w2, f.w1)
			return f.w2
		}, func(f *fcFix) { f.env.exec(`DELETE FROM run_fresh_resumes WHERE run_id=$1 AND state='seeded'`, f.run) }},
		{"a worker of another owner", func(f *fcFix) uuid.UUID {
			return f.destination(f.env.seedUser(f.t), freshBothCaps...)
		}, func(*fcFix) {}},
		{"a worker without recovery_inventory_v1", func(f *fcFix) uuid.UUID {
			return f.destination(f.user, capability.FreshResumeV1)
		}, func(*fcFix) {}},
		{"a worker without fresh_resume_v1", func(f *fcFix) uuid.UUID {
			return f.destination(f.user, capability.RecoveryInventoryV1)
		}, func(*fcFix) {}},
		{"a worker that is not hosted", func(f *fcFix) uuid.UUID {
			id := f.destination(f.user, freshBothCaps...)
			f.env.exec(`UPDATE workers SET kind='external', hosted_size=NULL WHERE id=$1`, id)
			return id
		}, func(*fcFix) {}},
		{"a draining worker", func(f *fcFix) uuid.UUID {
			f.env.exec(`UPDATE workers SET draining_since=now() WHERE id=$1`, f.w2)
			return f.w2
		}, func(f *fcFix) { f.env.exec(`UPDATE workers SET draining_since=NULL WHERE id=$1`, f.w2) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFCFix(t, true)
			frid := f.pending()
			claimant := c.apply(f)
			f.wantSkipped(c.name, frid, claimant)
			c.lift(f)
			if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil || p.FreshResume.ID != frid.String() {
				t.Fatalf("control: with the exclusion lifted W2 got %+v, want the transfer", p)
			}
		})
	}

	t.Run("the cross-check lane", func(t *testing.T) {
		f := newFCFix(t, true)
		frid := f.pending()
		v := f.version()
		if p, err := f.svc.ClaimCrossCheck(f.env.ctx, wkrRow(t, f.env, f.w2), nil); err != nil || p != nil {
			t.Fatalf("ClaimCrossCheck = (%v, %v), want idle", p != nil, err)
		}
		if f.state(frid) != "pending" || f.version() != v {
			t.Fatalf("the cross-check lane touched the transfer")
		}
		if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil {
			t.Fatalf("control: the run lane did not take the transfer")
		}
	})
}

// The gate is read at claim time: off, no fresh claim runs and the pending row stays as it was.
func TestFreshClaimGateOffLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	f.svc.p.FreshResume = false
	f.wantSkipped("gate off", frid, f.w2)
	f.svc.p.FreshResume = true
	if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil {
		t.Fatalf("control: the gate on did not take the transfer")
	}
}

// A candidate ClaimRun's own gates refuse (here a CI-config-guarded run, which W2 cannot claim) is
// rolled back to the savepoint whole, and the next pending row of the same poll is claimed.
func TestFreshClaimGatedCandidateFallsThroughLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	a := f.pending()
	g := f.second()
	g.capture(2, "available", freshSha('b'), nil, 2*time.Hour)
	res, err := g.start(g.preview().SelectionToken, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	b := res.Row.ID
	f.env.exec(`UPDATE runs SET ci_config_guard=true WHERE id=$1`, f.run)

	p := f.mustClaimAs(f.w2)
	if p == nil || p.RunID != g.run.String() || p.FreshResume == nil || p.FreshResume.ID != b.String() {
		t.Fatalf("poll = %+v, want the second row's run %s", p, g.run)
	}
	if f.state(a) != "pending" {
		t.Fatalf("the gated row is %q, want pending (rolled back, not refused)", f.state(a))
	}
	f.wantHeld("gated candidate")
	if f.openHoldsOf(f.w2) != 0 {
		t.Fatalf("the rolled-back attempt left a hold of W2")
	}
	// Control: the gate lifted, the same row is claimable.
	f.env.exec(`UPDATE runs SET ci_config_guard=false WHERE id=$1`, f.run)
	if p := f.mustClaimAs(f.w2); p == nil || p.RunID != f.run.String() {
		t.Fatalf("control: poll = %+v, want the first run", p)
	}
}

// The owner lock is only tried: while another transaction holds it the candidate is skipped without
// waiting and without a refusal.
func TestFreshClaimOwnerLockBusySkipsLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	holder, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(f.env.ctx) }()
	if err := store.LockFreshResumeOwner(f.env.ctx, holder, f.user); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.env.ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	p, err := f.svc.Claim(ctx, wkrRow(t, f.env, f.w2), nil)
	if err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v) with the owner lock held, want an idle poll", p != nil, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the claim waited %v on the owner lock", time.Since(start))
	}
	if f.state(frid) != "pending" {
		t.Fatalf("a busy owner lock refused the transfer: %q", f.state(frid))
	}
	if err := holder.Rollback(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil {
		t.Fatalf("control: the lock released, the transfer was not claimed")
	}
}

// W1 locked by another writer is contention (skip, the row stays pending); W1 deleted is the
// predecessor-less transfer (claimed with no saved worker, restorable to a NULL worker).
func TestFreshClaimPredecessorContendedVersusDeletedLiveDB(t *testing.T) {
	t.Run("contended is a skip, never a deletion", func(t *testing.T) {
		f := newFCFix(t, true)
		frid := f.pending()
		holder, err := f.env.pool.Begin(f.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(f.env.ctx) }()
		if _, err := holder.Exec(f.env.ctx, `SELECT id FROM workers WHERE id=$1 FOR UPDATE`, f.w1); err != nil {
			t.Fatal(err)
		}
		// A claim that waited on W1 (or claimed over it) would not return idle within the bound.
		ctx, cancel := context.WithTimeout(f.env.ctx, 15*time.Second)
		p, cerr := f.svc.Claim(ctx, wkrRow(t, f.env, f.w2), nil)
		cancel()
		if cerr != nil || p != nil {
			t.Fatalf("Claim with W1 locked = (%v, %v), want an idle poll", p != nil, cerr)
		}
		if f.state(frid) != "pending" {
			t.Fatalf("W1 contention left the transfer %q, want pending (a skip, never a refusal)", f.state(frid))
		}
		f.wantHeld("W1 locked")
		if err := holder.Rollback(f.env.ctx); err != nil {
			t.Fatal(err)
		}
		// Control: unlocked, the same transfer is claimed with its saved worker.
		if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil {
			t.Fatalf("control: W1 unlocked, nothing was claimed")
		}
		if row := f.transfer(frid); row.SavedWorkerID.Bytes != f.w1 || row.PredecessorWorkerID.Bytes != f.w1 {
			t.Fatalf("saved/predecessor = %v/%v, want W1", row.SavedWorkerID, row.PredecessorWorkerID)
		}
	})

	t.Run("deleted proceeds with no saved worker", func(t *testing.T) {
		f := newFCFix(t, true)
		frid := f.pending()
		f.env.exec(`UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL, released_at=now() WHERE run_id=$1`, f.run)
		f.env.exec(`UPDATE runs SET released_worker_id=NULL WHERE id=$1`, f.run)
		f.env.exec(`DELETE FROM workers WHERE id=$1`, f.w1)
		p := f.mustClaimAs(f.w2)
		if p == nil || p.FreshResume == nil || p.FreshResume.ID != frid.String() {
			t.Fatalf("poll = %+v, want the transfer claimed with W1 deleted", p)
		}
		if row := f.transfer(frid); row.State != "claimed" || row.SavedWorkerID.Valid || row.PredecessorWorkerID.Valid {
			t.Fatalf("transfer = %+v, want claimed with NULL saved and predecessor workers", row)
		}
	})
}

// A claimed transfer is invisible to every other poll: a second destination finds nothing, in flight
// (the run and transfer locked by the claim) and after the transition, and never meets the guard (UZ416).
func TestFreshClaimSecondPollerLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	w3 := f.destination(f.user, freshBothCaps...)

	// In flight: another transaction holds the run and the transfer, as the claim does until its commit.
	holder, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(f.env.ctx) }()
	if _, err := holder.Exec(f.env.ctx, `SELECT 1 FROM runs r JOIN run_fresh_resumes t ON t.run_id=r.id WHERE r.id=$1 FOR UPDATE OF r, t`, f.run); err != nil {
		t.Fatal(err)
	}
	f.wantSkipped("run and transfer locked", frid, w3)
	if err := holder.Rollback(f.env.ctx); err != nil {
		t.Fatal(err)
	}

	// After the transition: W2 claims, and W3 polling from inside W2's assembly gets nothing.
	var inner *ClaimPayload
	var innerErr error
	f.svc.claimHooks = &claimTestHooks{beforeAssembly: func(context.Context, store.Run) {
		inner, innerErr = f.claimAs(w3)
	}}
	if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil {
		t.Fatalf("W2 claim = %+v", p)
	}
	if inner != nil || innerErr != nil {
		t.Fatalf("W3 poll during W2's claimed transfer = (%v, %v), want idle with no error", inner != nil, innerErr)
	}
	if f.state(frid) != "claimed" {
		t.Fatalf("transfer = %q, want claimed", f.state(frid))
	}
}

// R1: the headroom recheck at claim time. Near the cap the claim is refused with
// insufficient_custody_headroom, the refusal survives the savepoint, and an ordinary Resume then reaches W1.
func TestFreshClaimHeadroomRefusalLiveDB(t *testing.T) {
	t.Run("near the cap the claim is refused and persists, then Resume reaches W1", func(t *testing.T) {
		f := newFCFix(t, true)
		f.limit.n = 3
		frid := f.pending() // one own hold: 1+2 <= 3 admits the start
		v := f.version()
		f.hold(1, f.w1) // a second own hold: 2+2 > 3
		f.wantIdle("over the per-run bound", f.w2)
		row := f.transfer(frid)
		if row.State != "refused" || row.RefusalReason.String != FreshRefusalInsufficientHeadroom {
			t.Fatalf("transfer = %q/%q, want refused insufficient_custody_headroom", row.State, row.RefusalReason.String)
		}
		f.wantHeld("headroom refusal")
		if f.version() != v+1 {
			t.Fatalf("hold version = %d, want %d", f.version(), v+1)
		}
		if f.openHoldsOf(f.w2) != 0 {
			t.Fatalf("the refused claim left a hold of W2")
		}
		run, err := f.env.q.GetRunByIDForUser(f.env.ctx, store.GetRunByIDForUserParams{ID: f.run, UserID: f.user})
		if err != nil {
			t.Fatal(err)
		}
		if handled, err := f.svc.ResumeWorkerRecoveryFresh(f.env.ctx, f.user, run, 36000); !handled || err != nil {
			t.Fatalf("Resume: handled=%v err=%v", handled, err)
		}
		p := f.mustClaimAs(f.w1)
		if p == nil || p.RunID != f.run.String() || p.FreshResume != nil {
			t.Fatalf("W1 claim after Resume = %+v, want the ordinary claim of the run", p)
		}
	})

	t.Run("limit of zero or less admits", func(t *testing.T) {
		f := newFCFix(t, true)
		f.limit.n = 3
		frid := f.pending()
		f.hold(1, f.w1)
		f.limit.n = 0
		if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil || p.FreshResume.ID != frid.String() {
			t.Fatalf("limit 0: poll = %+v, want the transfer claimed", p)
		}
	})

	t.Run("the first claim at the boundary is admitted, the second refused when the count has grown", func(t *testing.T) {
		f := newFCFix(t, true)
		f.limit.n = 4
		a := f.pending()
		g := f.second()
		g.capture(2, "available", freshSha('b'), nil, 2*time.Hour)
		res, err := g.start(g.preview().SelectionToken, uuid.New())
		if err != nil {
			t.Fatal(err)
		}
		// Two held runs and two reservations: four units, exactly the limit. The claim is charged its own
		// reservation once, so the count may equal the limit.
		if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil || p.FreshResume.ID != a.String() {
			t.Fatalf("first claim at the boundary = %+v, want the older transfer", p)
		}
		// Another held run of the owner takes the fifth unit before the second claim.
		other := seedOutageRun(t, f.env, f.user, f.repo, f.w1, "recovery_wait", "issue", 1, 0)
		f.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id)
		            VALUES($1,$2,$3,$4,1,'open',$5,'fresh-other',$5,$4)`, uuid.New(), f.user, f.repo, other, f.w1)
		f.wantIdle("second claim over the limit", f.w2)
		if row := g.transfer(res.Row.ID); row.State != "refused" || row.RefusalReason.String != FreshRefusalInsufficientHeadroom {
			t.Fatalf("second transfer = %q/%q, want refused insufficient_custody_headroom", row.State, row.RefusalReason.String)
		}
		if f.state(a) != "claimed" {
			t.Fatalf("the first transfer = %q, want claimed", f.state(a))
		}
	})
}

// The claim path pins its isolation and locks: Prepare, ClaimRun and MarkFreshResumeClaimed run in one
// READ COMMITTED transaction that holds the run's row lock. The beginner hands out REPEATABLE READ
// transactions, so the isolation seen at the mark is the claim's own doing.
type fcSpyBeginner struct {
	pool interface {
		BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
	}
	onMark func(ctx context.Context, tx pgx.Tx)
}

func (b fcSpyBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	return &fcSpyTx{Tx: tx, b: b}, nil
}

type fcSpyTx struct {
	pgx.Tx
	b fcSpyBeginner
}

func (t *fcSpyTx) Begin(ctx context.Context) (pgx.Tx, error) {
	sp, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &fcSpyTx{Tx: sp, b: t.b}, nil
}

func (t *fcSpyTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "UPDATE run_fresh_resumes") && strings.Contains(sql, "state = 'claimed'") {
		t.b.onMark(ctx, t.Tx)
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

func TestFreshClaimIsOneReadCommittedTransactionHoldingTheRunLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	var marks int
	var iso string
	var lockErr error
	f.svc.SetTxBeginner(fcSpyBeginner{pool: f.env.pool, onMark: func(ctx context.Context, tx pgx.Tx) {
		marks++
		if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
			t.Errorf("isolation probe: %v", err)
		}
		// A second connection cannot take the run's row lock while the claim transaction holds it.
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		other, err := f.env.pool.Begin(c)
		if err != nil {
			t.Errorf("probe tx: %v", err)
			return
		}
		defer func() { _ = other.Rollback(c) }()
		var id uuid.UUID
		lockErr = other.QueryRow(c, `SELECT id FROM runs WHERE id=$1 FOR UPDATE NOWAIT`, f.run).Scan(&id)
	}})
	if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil || p.FreshResume.ID != frid.String() {
		t.Fatalf("claim = %+v", p)
	}
	if marks != 1 {
		t.Fatalf("MarkFreshResumeClaimed ran %d times, want 1", marks)
	}
	if iso != "read committed" {
		t.Fatalf("claim transaction isolation = %q, want read committed", iso)
	}
	var pgErr *pgconn.PgError
	if !errors.As(lockErr, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("run lock probe at the mark = %v, want 55P03 (the claim holds the run)", lockErr)
	}
}

// The snapshot-carrying claim runs the same fresh claim in its own transaction.
func TestFreshClaimWithActiveSnapshotLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	f.env.exec(`UPDATE workers SET snapshot_register_nonce='fresh-w2-nonce', snapshot_epoch=0 WHERE id=$1`, f.w2)
	p, err := f.svc.Claim(f.env.ctx, wkrRow(t, f.env, f.w2), &ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: "fresh-w2-nonce"})
	if err != nil || p == nil || p.FreshResume == nil || p.FreshResume.ID != frid.String() {
		t.Fatalf("snapshot claim = (%+v, %v), want the transfer", p, err)
	}
}

// fcTuple reads the columns a refusal puts back, as one comparable string.
func (x *fcFix) fcTuple() string {
	x.t.Helper()
	var s string
	if err := x.env.pool.QueryRow(x.env.ctx, `SELECT concat_ws('|', status, worker_id, COALESCE(recovery_wait_cause,''), COALESCE(hold_reason,''),
	    claim_released_at, released_worker_id, status_since, budget_paused_seconds, requeue_count, health, started_at, session_id)
	    FROM runs WHERE id=$1`, x.run).Scan(&s); err != nil {
		x.t.Fatal(err)
	}
	return s
}

// noFailedOrPaused asserts what a fresh refusal published: the restored recovery_wait and nothing else.
func (x *fcFix) wantOnlyRecoveryWaitPublished() {
	x.t.Helper()
	states, notifies := x.rec.seen()
	for _, got := range [][]string{states, notifies} {
		if len(got) != 1 || got[0] != "recovery_wait" {
			x.t.Fatalf("published states %v and notifications %v, want exactly one recovery_wait each (never failed or paused)", states, notifies)
		}
	}
}

func (x *fcFix) disableSecretAfterAssembly() {
	x.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil {
			x.t.Errorf("assembly = (%v, %v), want a payload", p != nil, err)
		}
		x.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, x.secret)
	}}
}

// V1: a credential disabled between assembly and the finish refuses the claimed transfer instead of
// parking the run. The run is back at its prior hold with no hold of W2 left, an ordinary Resume then
// reaches W1, and only the restored recovery_wait is published. The claim's fences stay: the
// generation advanced, the Codex epoch moved and the capability hash is gone (D25).
func TestFreshClaimCredentialDisabledRefusesAtAssemblyLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	f.env.exec(`UPDATE runs SET codex_claim_epoch=5, codex_cap_hash=decode('aabb','hex'), session_id='sess-fresh' WHERE id=$1`, f.run)
	before, usedBefore := f.fcTuple(), f.usedWall()
	f.disableSecretAfterAssembly()

	if p, err := f.claimAs(f.w2); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v), want an idle poll", p != nil, err)
	}
	row := f.transfer(frid)
	if row.State != "refused" || row.RefusalReason.String != freshRefusalCredentialDisabled {
		t.Fatalf("transfer = %q/%q, want refused credential_disabled", row.State, row.RefusalReason.String)
	}
	if after := f.fcTuple(); after != before {
		t.Fatalf("the run did not return to its prior hold:\nbefore %s\nafter  %s", before, after)
	}
	if used := f.usedWall(); used != usedBefore {
		t.Fatalf("remaining wall changed by claim and refusal: used %.1fs before, %.1fs after", usedBefore, used)
	}
	var gen, epoch int64
	var hash []byte
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT claim_generation, codex_claim_epoch, codex_cap_hash FROM runs WHERE id=$1`, f.run).Scan(&gen, &epoch, &hash); err != nil {
		t.Fatal(err)
	}
	if gen != 3 || epoch <= 5 || hash != nil {
		t.Fatalf("fences after the refusal: generation=%d epoch=%d hash=%x, want 3, >5 and NULL", gen, epoch, hash)
	}
	if f.openHoldsOf(f.w2) != 0 || f.openHoldsOf(f.w1) != 1 {
		t.Fatalf("holds: W2 %d, W1 %d, want none of W2's and W1's kept", f.openHoldsOf(f.w2), f.openHoldsOf(f.w1))
	}
	f.wantOnlyRecoveryWaitPublished()

	// An ordinary Resume reaches W1.
	f.env.exec(`UPDATE user_secrets SET disabled_at = NULL, enablement_rev = enablement_rev + 1 WHERE id = $1`, f.secret)
	f.svc.claimHooks = nil
	run, err := f.env.q.GetRunByIDForUser(f.env.ctx, store.GetRunByIDForUserParams{ID: f.run, UserID: f.user})
	if err != nil {
		t.Fatal(err)
	}
	if handled, err := f.svc.ResumeWorkerRecoveryFresh(f.env.ctx, f.user, run, 36000); !handled || err != nil {
		t.Fatalf("Resume: handled=%v err=%v", handled, err)
	}
	p := f.mustClaimAs(f.w1)
	if p == nil || p.RunID != f.run.String() || p.FreshResume != nil {
		t.Fatalf("W1 claim after the refusal and Resume = %+v", p)
	}
}

// V1, terminal arm: an assembly that would fail the run (FailClaimAssemblyExact) refuses the transfer; the
// run is never failed. Control: the same owner's ordinary claim of a queued run does fail.
func TestFreshClaimTerminalAssemblyRefusesInsteadOfFailingLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	f.env.exec(`DELETE FROM user_secrets WHERE user_id=$1`, f.user) // no model credential: a terminal assembly failure
	before := f.fcTuple()

	if p, err := f.claimAs(f.w2); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v), want an idle poll", p != nil, err)
	}
	row := f.transfer(frid)
	if row.State != "refused" || row.RefusalReason.String != freshRefusalAssemblyFailed {
		t.Fatalf("transfer = %q/%q, want refused claim_assembly_failed", row.State, row.RefusalReason.String)
	}
	if after := f.fcTuple(); after != before {
		t.Fatalf("the run did not return to its prior hold:\nbefore %s\nafter  %s", before, after)
	}
	var failOrigin pgtype.Text
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT fail_origin FROM runs WHERE id=$1`, f.run).Scan(&failOrigin); err != nil || failOrigin.Valid {
		t.Fatalf("fail_origin = %v (%v), want none", failOrigin, err)
	}
	f.wantOnlyRecoveryWaitPublished()
	if f.openHoldsOf(f.w2) != 0 {
		t.Fatalf("a hold of W2 is left")
	}

	// Control: the ordinary claim of a plain queued run of the same owner fails it, the arm the refusal replaces.
	plain := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status) VALUES ($1,$2,$3,'issue',4242,'t','d','queued')`, plain, f.user, f.repo)
	f.mustClaimAs(f.w2)
	if got := statusOf(t, f.env, plain); got != "failed" {
		t.Fatalf("control: the ordinary no-credential claim left status %q, want failed", got)
	}
}

// V1, pool wait: an empty auto pool refuses the transfer instead of parking the run in pool_wait.
func TestFreshClaimPoolWaitRefusesInsteadOfParkingLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	f.env.exec(`UPDATE workers SET anthropic_bind_mode='auto' WHERE id=$1`, f.w2)
	before := f.fcTuple()

	if p, err := f.claimAs(f.w2); err != nil || p != nil {
		t.Fatalf("Claim = (%v, %v), want an idle poll", p != nil, err)
	}
	row := f.transfer(frid)
	if row.State != "refused" || row.RefusalReason.String != freshRefusalPoolWait {
		t.Fatalf("transfer = %q/%q, want refused pool_wait", row.State, row.RefusalReason.String)
	}
	if after := f.fcTuple(); after != before {
		t.Fatalf("the run did not return to its prior hold:\nbefore %s\nafter  %s", before, after)
	}
	f.wantOnlyRecoveryWaitPublished()
}

// Crossed refusals: run A (W1 to W2) and run B (W2 to W1) are refused at the same moment, each needing the
// other's worker as its saved predecessor. Both finish without a deadlock (40P01) or an error.
func TestFreshClaimCrossedAssemblyRefusalsLiveDB(t *testing.T) {
	a := newFCFix(t, true)
	aID := a.pending()

	// B is the mirror: held on W2, destination W1.
	mirror := *a.freshFix
	mirror.w1, mirror.w2 = a.w2, a.w1
	mirror.run = seedOutageRun(t, a.env, a.user, a.repo, mirror.w1, "recovery_wait", "issue", 2, 0)
	a.env.exec(`UPDATE runs SET recovery_wait_cause='worker_requeue_exhausted', claim_released_at=now(), released_worker_id=$2 WHERE id=$1`, mirror.run, mirror.w1)
	if err := a.env.pool.QueryRow(a.env.ctx, `SELECT issue_iid FROM runs WHERE id=$1`, mirror.run).Scan(&mirror.iid); err != nil {
		t.Fatal(err)
	}
	a.env.exec(`UPDATE workers SET kind='hosted', hosted_size='m', template_declared='base', protocol_capabilities=$2, last_heartbeat_at=now() WHERE id=$1`, a.w1, freshBothCaps)
	b := &fcFix{freshFix: &mirror, rec: a.rec, secret: a.secret}
	bID := b.pending()

	// Both claims stop after assembly until both have arrived, so their finishing transactions run together.
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	go func() {
		for i := 0; i < 2; i++ {
			select {
			case <-arrived:
			case <-time.After(20 * time.Second):
				t.Error("only one claim reached its assembly")
				close(release)
				return
			}
		}
		a.env.exec(`UPDATE user_secrets SET disabled_at = now(), enablement_rev = enablement_rev + 1 WHERE id = $1`, a.secret)
		close(release)
	}()
	var reached sync.Map // run id -> true, once that run's claim reached its assembly
	a.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, _ *ClaimPayload, _ error) {
		reached.Store(run.ID, true)
		arrived <- struct{}{}
		<-release
	}}
	defer func() { a.svc.claimHooks = nil }()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []struct{ w, run uuid.UUID }{{a.w2, a.run}, {a.w1, b.run}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A fresh claim of one owner that finds the owner lock taken by the other claim skips and polls
			// idle (the lock is only tried), so each worker polls until its own claim reached assembly.
			for try := 0; try < 200; try++ {
				if _, errs[i] = a.svc.Claim(a.env.ctx, wkrRow(t, a.env, c.w), nil); errs[i] != nil {
					return
				}
				if _, ok := reached.Load(c.run); ok {
					return
				}
				time.Sleep(25 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		var pgErr *pgconn.PgError
		if err != nil || errors.As(err, &pgErr) {
			t.Fatalf("claim %d: %v, want a clean refusal (no 40P01 or lock error)", i, err)
		}
	}
	if got := a.state(aID); got != "refused" {
		t.Fatalf("transfer A = %q, want refused", got)
	}
	if got := b.state(bID); got != "refused" {
		t.Fatalf("transfer B = %q, want refused", got)
	}
}

// The W1-keyed dedupe is rechecked on the locked versions: a terminal_pending row of W1 for the run's
// current generation that commits AFTER the unlocked candidate read must skip the candidate, claiming
// nothing. The seam is afterFreshCandidateRead, between the candidate read and the per-candidate step.
func TestFreshClaimRechecksW1DedupeUnderLocksLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	// A live (unreleased) W1 claim is what the dedupe protects.
	f.env.exec(`UPDATE runs SET claim_released_at=NULL WHERE id=$1`, f.run)
	var hookRan bool
	afterFreshCandidateRead = func(cands []store.ListPendingFreshResumeCandidatesRow) {
		hookRan = true
		if len(cands) != 1 || cands[0].ID != frid {
			t.Errorf("candidates = %+v, want the one transfer", cands)
		}
		f.env.exec(`INSERT INTO worker_active_runs(worker_id,run_id,claim_generation,phase,terminal_pending,terminal_pending_until,snapshot_epoch,reported_at)
		            VALUES($1,$2,2,'running',true,now() + interval '1 hour',1,now() - interval '1 day')`, f.w1, f.run)
	}
	defer func() { afterFreshCandidateRead = nil }()

	f.wantSkipped("terminal_pending committed after the candidate read", frid, f.w2)
	if !hookRan {
		t.Fatal("the candidate-read seam never ran")
	}
	if f.openHoldsOf(f.w2) != 0 {
		t.Fatalf("a skipped candidate left a hold of W2")
	}

	// Control: the row gone, the same transfer is claimed.
	afterFreshCandidateRead = nil
	f.env.exec(`DELETE FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2`, f.w1, f.run)
	if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil || p.FreshResume.ID != frid.String() {
		t.Fatalf("control: poll = %+v, want the transfer claimed", p)
	}
}

// The candidate read carries ClaimRun's weekly-reserve gate: a reserve-held pending row is not even a
// candidate (the seam sees an empty list); with the reserve off the same row is read and claimed.
func TestFreshClaimCandidateReadHonorsWeeklyReserveLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	f.svc.p.UsagePollInterval = 5 * time.Minute
	f.svc.p.CodexUsagePollInterval = 5 * time.Minute
	if _, err := f.env.q.SetUserUsageReserve(f.env.ctx, store.SetUserUsageReserveParams{
		ID: f.user, WeeklyReservePct: pgtype.Int2{Int16: 50, Valid: true}, WeeklyReservePauseRunning: true,
	}); err != nil {
		t.Fatal(err)
	}
	f.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, five_hour_resets_at,
	                seven_day_pct, seven_day_resets_at, source, synced_at)
	            VALUES ($1, $2, 10, now() + interval '1 hour', 95, now() + interval '48 hours', 'usage_endpoint', now() - interval '1 minute')`,
		f.secret, f.user)

	var seen []int
	afterFreshCandidateRead = func(cands []store.ListPendingFreshResumeCandidatesRow) { seen = append(seen, len(cands)) }
	defer func() { afterFreshCandidateRead = nil }()

	f.wantSkipped("reserve-held row", frid, f.w2)
	if len(seen) != 1 || seen[0] != 0 {
		t.Fatalf("candidates per poll under the reserve = %v, want [0] (not even attempted)", seen)
	}

	// Control: the reserve off, the row is a candidate and is claimed.
	if _, err := f.env.q.SetUserUsageReserve(f.env.ctx, store.SetUserUsageReserveParams{ID: f.user, WeeklyReservePauseRunning: true}); err != nil {
		t.Fatal(err)
	}
	if p := f.mustClaimAs(f.w2); p == nil || p.FreshResume == nil || p.FreshResume.ID != frid.String() {
		t.Fatalf("control: poll = %+v, want the transfer claimed", p)
	}
	if len(seen) != 2 || seen[1] != 1 {
		t.Fatalf("candidates per poll = %v, want [0 1]", seen)
	}
}

// The V1 finisher's lock order with W2's worker row really held: an active publication enrollment makes
// finishRunClaimTx take W2 FOR UPDATE before the run lock, then the saved predecessor W1 FOR KEY SHARE
// NOWAIT. A second connection holds W1 FOR UPDATE and then asks for W2 FOR UPDATE (blocking): the
// finisher must give up W2 on its 55P03 and retry, so the second connection gets W2, commits, and the
// finisher then completes. A blocking W1 lock (or one taken after the run lock) deadlocks here (40P01).
func TestFreshClaimFinisherNoWaitUnderHeldWorkerLocksLiveDB(t *testing.T) {
	f := newFCFix(t, true)
	frid := f.pending()
	f.env.exec(`UPDATE workers SET protocol_capabilities = protocol_capabilities || ARRAY['publication_resume_v1']::text[] WHERE id=$1`, f.w2)

	var holderErr error
	var holderDone sync.WaitGroup
	f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, run store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil {
			t.Errorf("assembly = (%v, %v), want a payload", p != nil, err)
			return
		}
		f.enrollPublication(run, f.w2)
		holder, herr := f.env.pool.Begin(f.env.ctx)
		if herr != nil {
			t.Errorf("holder: %v", herr)
			return
		}
		if _, herr := holder.Exec(f.env.ctx, `SELECT id FROM workers WHERE id=$1 FOR UPDATE`, f.w1); herr != nil {
			t.Errorf("holder W1 lock: %v", herr)
			_ = holder.Rollback(f.env.ctx)
			return
		}
		holderDone.Add(1)
		go func() {
			defer holderDone.Done()
			// By now the finisher holds W2 and is failing its NOWAIT lock on W1 (the retry delay is longer).
			time.Sleep(40 * time.Millisecond)
			ctx, cancel := context.WithTimeout(f.env.ctx, 15*time.Second)
			defer cancel()
			if _, herr := holder.Exec(ctx, `SELECT id FROM workers WHERE id=$1 FOR UPDATE`, f.w2); herr != nil {
				holderErr = herr
				_ = holder.Rollback(f.env.ctx)
				return
			}
			holderErr = holder.Commit(f.env.ctx)
		}()
	}}
	defer func() { f.svc.claimHooks = nil }()

	ctx, cancel := context.WithTimeout(f.env.ctx, 60*time.Second)
	defer cancel()
	p, err := f.svc.Claim(ctx, wkrRow(t, f.env, f.w2), nil)
	holderDone.Wait()
	if err != nil {
		t.Fatalf("Claim = %v, want the finisher to retry its 55P03 and succeed", err)
	}
	if holderErr != nil {
		t.Fatalf("second connection: %v, want it to take W2 once the finisher gave it up", holderErr)
	}
	if p == nil || p.FreshResume == nil || p.FreshResume.ID != frid.String() {
		t.Fatalf("payload = %+v, want the claimed transfer delivered", p)
	}
}

// enrollPublication writes an active publication-resume enrollment for a claimed run, so the finisher
// locks the claiming worker's row before the run row.
func (x *fcFix) enrollPublication(run store.Run, worker uuid.UUID) {
	x.t.Helper()
	tx, err := x.env.pool.Begin(x.env.ctx)
	if err != nil {
		x.t.Error(err)
		return
	}
	defer func() { _ = tx.Rollback(x.env.ctx) }()
	if err := store.New(tx).OptInPublicationResume(x.env.ctx); err != nil {
		x.t.Error(err)
		return
	}
	intent, _ := json.Marshal(map[string]any{
		"run_id": run.ID, "repo_id": x.repo, "branch": "agent/issue-1", "claim_generation": run.ClaimGeneration,
		"original_head": pubHeadA, "floor": map[string]any{"kind": "absent"}, "candidate": nil,
	})
	if _, err := tx.Exec(x.env.ctx, `UPDATE runs SET branch=$2 WHERE id=$1`, run.ID, "agent/issue-1"); err != nil {
		x.t.Error(err)
		return
	}
	if _, err := tx.Exec(x.env.ctx, `INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, worker_id, intent, state)
	    VALUES ($1, 1, $2, $3, $4, 'active')`, run.ID, run.ClaimGeneration, worker, intent); err != nil {
		x.t.Error(err)
		return
	}
	if err := tx.Commit(x.env.ctx); err != nil {
		x.t.Error(err)
	}
}
