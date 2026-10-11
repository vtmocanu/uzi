package recovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #2545 step B: the held-publication FINAL through recovery.Service.Release against a real
// Postgres, with a stubbed forge listing. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres (./e2e/run-store-it.sh).

const (
	heldTestTip    = "1111111111111111111111111111111111111111"
	heldTestDigest = "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"
)

type heldEnv struct {
	t         *testing.T
	ctx       context.Context
	pool      *pgxpool.Pool
	q         *store.Queries
	svc       *Service
	w         store.Worker
	user      uuid.UUID
	repo      uuid.UUID
	run, hold uuid.UUID
	pub       uuid.UUID
	reqPub    uuid.UUID // overrides the publication the request names
	retention time.Duration

	mu        sync.Mutex
	listCalls int
	listRefs  []string
	tips      map[string]string
	listErr   error
	remoteErr error
	settled   []uuid.UUID
}

func newHeldEnv(t *testing.T) *heldEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run ./e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	e := &heldEnv{t: t, ctx: ctx, pool: pool, q: store.New(pool), user: uuid.New(), repo: uuid.New(),
		run: uuid.New(), hold: uuid.New(), retention: 36 * time.Hour}
	caps := []string{capability.RecoveryArchiveV1, capability.RecoveryInventoryV1, capability.RecoveryHeldPublicationV1}
	e.w = store.Worker{ID: uuid.New(), UserID: e.user, Name: "held-" + uuid.NewString(), ProtocolCapabilities: caps}
	e.exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", e.user, fmt.Sprintf("held-final-%s@example.com", e.user))
	e.exec("INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities) VALUES($1,$2,$3,$4,'online',$5)", e.w.ID, e.user, e.w.Name, e.w.ID[:], caps)
	conn := uuid.New()
	e.exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES($1,$2,'github','https://example.com','bot',1,$3)", conn, e.user, []byte{1})
	e.exec("INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES($1,$2,1,'g/r','https://example.com/g/r','main',true)", e.repo, conn)
	e.exec("INSERT INTO runs(id,user_id,repo_id,issue_iid,kind,issue_title,issue_description,status,worker_id,claim_generation) VALUES($1,$2,$3,1,'issue','t','d','running',$4,1)", e.run, e.user, e.repo, e.w.ID)
	e.exec("INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state) VALUES($1,$2,$3,$4,1,$5,$6,$5,$4,true,'open')", e.hold, e.user, e.repo, e.run, e.w.ID, e.w.Name)
	e.exec("UPDATE runs SET status='failed' WHERE id=$1", e.run)

	// Step A, as the broker records it: prepared, invoked, created.
	pub, err := e.q.UpsertHeldPublication(ctx, store.UpsertHeldPublicationParams{
		RunID: e.run, Generation: 1, HoldID: e.hold, UserID: e.user, RepoID: e.repo, WorkerID: e.w.ID,
		Ref: pushbroker.HeldRef(e.run, 1), Tip: heldTestTip, CoverageDigest: heldTestDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.MarkHeldCreateInvoked(ctx, pub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: pub.ID}); err != nil {
		t.Fatal(err)
	}
	e.pub = pub.ID
	e.tips = map[string]string{pushbroker.HeldRef(e.run, 1): heldTestTip}
	e.svc = e.newService()
	return e
}

// newService builds a service wired to the env's stubbed forge, switch on.
func (e *heldEnv) newService() *Service {
	svc := New(e.q, e.pool, nil, Limits{ReadyRetention: e.retention, RequestDeadline: 10 * time.Second}, nil)
	svc.SetHeldPublication(true, func(context.Context, uuid.UUID) (pushbroker.ListRefsOptions, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		return pushbroker.ListRefsOptions{CloneURL: "https://forge.invalid/g/r.git"}, e.remoteErr
	})
	svc.heldListFn = func(_ context.Context, _ pushbroker.ListRefsOptions, refs ...string) (map[string]string, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.listCalls++
		e.listRefs = append(e.listRefs, refs...)
		if e.listErr != nil {
			return nil, e.listErr
		}
		out := map[string]string{}
		for _, r := range refs {
			if tip, ok := e.tips[r]; ok {
				out[r] = tip
			}
		}
		return out, nil
	}
	svc.SetCustodySettledHook(func(id uuid.UUID) {
		e.mu.Lock()
		e.settled = append(e.settled, id)
		e.mu.Unlock()
	})
	return svc
}

func (e *heldEnv) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("%s: %v", sql, err)
	}
}

func (e *heldEnv) lists() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.listCalls
}

func (e *heldEnv) request() apitypes.RecoveryReleaseRequest {
	g := int64(1)
	pub := e.pub
	if e.reqPub != uuid.Nil {
		pub = e.reqPub
	}
	return apitypes.RecoveryReleaseRequest{Generation: &g, FinalDisposition: &apitypes.RecoveryFinalDisposition{
		Kind: FinalKindHeldPublication, PublicationID: pub.String(), SourceSha: heldTestTip, CoverageDigest: heldTestDigest,
	}}
}

func (e *heldEnv) release(w store.Worker, req apitypes.RecoveryReleaseRequest) (apitypes.RecoveryReleaseResponse, error) {
	e.t.Helper()
	return e.svc.Release(e.ctx, w, e.run, req)
}

func (e *heldEnv) mustRelease() apitypes.RecoveryReleaseResponse {
	e.t.Helper()
	res, err := e.release(e.w, e.request())
	if err != nil {
		e.t.Fatalf("release: %v", err)
	}
	e.wantAck(res)
	return res
}

func (e *heldEnv) wantAck(res apitypes.RecoveryReleaseResponse) {
	e.t.Helper()
	if !res.Released || res.HoldsReleased != 1 || res.Retained || res.Reason != "" || res.RunID != e.run.String() ||
		res.Generation == nil || *res.Generation != 1 {
		e.t.Fatalf("response = %+v, want the release ACK", res)
	}
}

func (e *heldEnv) wantRetained(res apitypes.RecoveryReleaseResponse, err error, reason string) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("err = %v, want a retained %s response", err, reason)
	}
	if res.Released || res.HoldsReleased != 0 || !res.Retained || res.Reason != reason {
		e.t.Fatalf("response = %+v, want retained %s", res, reason)
	}
	e.wantOpen()
}

// wantOpen asserts nothing was released or acknowledged.
func (e *heldEnv) wantOpen() {
	e.t.Helper()
	var state string
	var finalDisp *string
	if err := e.pool.QueryRow(e.ctx, "SELECT state, final_disposition FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&state, &finalDisp); err != nil {
		e.t.Fatal(err)
	}
	if state != "open" || finalDisp != nil {
		e.t.Fatalf("hold = %s/%v, want it still open", state, finalDisp)
	}
	var pubState string
	var acked *time.Time
	if err := e.pool.QueryRow(e.ctx, "SELECT state, acknowledged_at FROM run_held_publications WHERE id=$1", e.pub).Scan(&pubState, &acked); err != nil {
		e.t.Fatal(err)
	}
	if acked != nil || pubState == "acknowledged" {
		e.t.Fatalf("publication = %s acked=%v, want unacknowledged", pubState, acked)
	}
}

func (e *heldEnv) wantErr(err, want error) {
	e.t.Helper()
	if !errors.Is(err, want) {
		e.t.Fatalf("err = %v, want %v", err, want)
	}
	e.wantOpen()
}

func TestHeldFinalAThenBLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	e.mustRelease()

	var state, evidence, disp, sha, digest string
	var pubID uuid.UUID
	var liveWorker, captureID *uuid.UUID
	if err := e.pool.QueryRow(e.ctx, `SELECT state, release_evidence, final_disposition, final_source_sha, final_coverage_digest,
		final_publication_id, live_worker_id, final_capture_id FROM recovery_custody_holds WHERE id=$1`, e.hold).
		Scan(&state, &evidence, &disp, &sha, &digest, &pubID, &liveWorker, &captureID); err != nil {
		t.Fatal(err)
	}
	if state != "released" || evidence != "held_publication" || disp != "held_publication" || sha != heldTestTip ||
		digest != heldTestDigest || pubID != e.pub || liveWorker != nil || captureID != nil {
		t.Fatalf("hold = %s/%s/%s %s %s pub=%s live=%v capture=%v", state, evidence, disp, sha, digest, pubID, liveWorker, captureID)
	}
	var pubState string
	var retentionSecs float64
	if err := e.pool.QueryRow(e.ctx, `SELECT state, extract(epoch FROM expires_at - acknowledged_at) FROM run_held_publications WHERE id=$1`, e.pub).
		Scan(&pubState, &retentionSecs); err != nil {
		t.Fatal(err)
	}
	if pubState != "acknowledged" || retentionSecs != e.retention.Seconds() {
		t.Fatalf("publication = %s retention %vs, want acknowledged and exactly %vs", pubState, retentionSecs, e.retention.Seconds())
	}
	var captures int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM recovery_captures WHERE run_id=$1`, e.run).Scan(&captures); err != nil || captures != 0 {
		t.Fatalf("captures = %d, %v; a held release writes none", captures, err)
	}
	if len(e.settled) != 1 || e.settled[0] != e.run {
		t.Fatalf("custody settled hook = %v, want one call for the run", e.settled)
	}
	if len(e.listRefs) != 1 || e.listRefs[0] != pushbroker.HeldRef(e.run, 1) {
		t.Fatalf("listed refs = %v", e.listRefs)
	}

	// The reconcile receipt echoes the held identity, and the owner list names the ref.
	rec, err := e.svc.Reconcile(e.ctx, e.w, e.run, uuid.New(), apitypes.RecoveryReconcileRequest{
		Generation: 1, SourceSha: heldTestTip, CoverageDigest: heldTestDigest, Checksum: strings.Repeat("a", 64),
	})
	if err != nil || rec.Outcome != "accepted" || rec.FinalReceipt == nil || rec.FinalReceipt.Kind != FinalKindHeldPublication ||
		rec.FinalReceipt.PublicationID != e.pub.String() || rec.FinalReceipt.SourceSha != heldTestTip || rec.FinalReceipt.CaptureID != "" {
		t.Fatalf("reconcile = %+v, %v", rec, err)
	}
	holds, err := e.svc.ListHoldsForOwner(e.ctx, e.user, false)
	if err != nil || len(holds.Holds) != 1 {
		t.Fatalf("holds = %+v, %v", holds, err)
	}
	if h := holds.Holds[0]; h.FinalPublicationID != e.pub.String() || h.HeldRef != pushbroker.HeldRef(e.run, 1) ||
		h.FinalReceipt == nil || h.FinalReceipt.PublicationID != e.pub.String() {
		t.Fatalf("owner hold = %+v", h)
	}
}

// The plan-gate regression: an exact replay reads nothing mutable, so it is answered even when
// the worker lost both capabilities, the switch is off and the forge is down.
func TestHeldFinalReplayAfterCapabilityLossLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	e.mustRelease()
	listed := e.lists()

	e.exec("UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", e.w.ID)
	degraded := e.w
	degraded.ProtocolCapabilities = nil
	e.svc.heldEnabled = false
	e.listErr = errors.New("forge down")
	e.remoteErr = errors.New("forge connection gone")

	res, err := e.release(degraded, e.request())
	if err != nil {
		t.Fatalf("replay = %v, want the original ACK", err)
	}
	e.wantAck(res)
	if e.lists() != listed {
		t.Fatalf("the replay listed the forge (%d -> %d calls)", listed, e.lists())
	}

	mismatch := e.request()
	mismatch.FinalDisposition.CoverageDigest = strings.Repeat("3", 64)
	if _, err := e.release(degraded, mismatch); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("digest mismatch = %v, want ErrManifestConflict", err)
	}
	mismatch = e.request()
	mismatch.FinalDisposition.PublicationID = uuid.NewString()
	if _, err := e.release(degraded, mismatch); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("publication mismatch = %v, want ErrManifestConflict", err)
	}
	mismatch = e.request()
	mismatch.FinalDisposition.SourceSha = strings.Repeat("4", 40)
	if _, err := e.release(degraded, mismatch); !errors.Is(err, ErrManifestConflict) {
		t.Fatalf("tip mismatch = %v, want ErrManifestConflict", err)
	}

	foreign := degraded
	foreign.ID = uuid.New()
	if _, err := e.release(foreign, e.request()); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("foreign worker = %v, want ErrNotAuthorized", err)
	}
	foreign = degraded
	foreign.UserID = uuid.New()
	if _, err := e.release(foreign, e.request()); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("foreign owner = %v, want ErrNotAuthorized", err)
	}
	other := e.request()
	g := int64(2)
	other.Generation = &g
	if _, err := e.release(degraded, other); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("foreign generation = %v, want ErrNotAuthorized", err)
	}
	if _, err := e.svc.Release(e.ctx, degraded, uuid.New(), e.request()); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("foreign run = %v, want ErrNotAuthorized", err)
	}
	if e.lists() != listed {
		t.Fatalf("a refused replay listed the forge (%d -> %d calls)", listed, e.lists())
	}
}

// A replay that races the release is answered by the under-lock receipt compare, before any
// definitive refusal and without a second acknowledge.
func TestHeldFinalReplayRacingReleaseLiveDB(t *testing.T) {
	// The loser's unlocked recheck may see any ref state; the winner commits before the loser
	// takes its locks, and the loser must still get the ACK, never ref_missing or ref_mismatch.
	for _, tc := range []struct {
		name string
		tips func(e *heldEnv) map[string]string
	}{
		{"ref at tip", func(e *heldEnv) map[string]string { return e.tips }},
		{"ref missing at the first look", func(*heldEnv) map[string]string { return map[string]string{} }},
		{"ref foreign at the first look", func(e *heldEnv) map[string]string {
			return map[string]string{pushbroker.HeldRef(e.run, 1): strings.Repeat("9", 40)}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newHeldEnv(t)
			winner := e.newService()
			atTip := e.tips
			e.tips = tc.tips(e)
			ran := false
			e.svc.heldPreLockHook = func() {
				e.mu.Lock()
				e.tips = atTip
				e.mu.Unlock()
				res, err := winner.Release(e.ctx, e.w, e.run, e.request())
				if err != nil || !res.Released {
					t.Errorf("winner = %+v, %v", res, err)
				}
				// After the winner commits, every mutable gate turns against the loser: the
				// locked receipt compare must still answer the original ACK first.
				e.exec("UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", e.w.ID)
				e.exec("UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1", e.run)
				ran = true
			}
			res, err := e.release(e.w, e.request())
			if err != nil || !ran {
				t.Fatalf("loser = %+v, %v (winner ran: %v)", res, err, ran)
			}
			e.wantAck(res)
			// The winner's commit alone settled custody.
			if len(e.settled) != 1 {
				t.Fatalf("custody settled %d times, want 1", len(e.settled))
			}
		})
	}
}

func TestHeldFinalNotOpenWithoutReceiptTakesNoRunLockLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		"ALTER TABLE recovery_custody_holds DISABLE TRIGGER recovery_inventory_hold_guard",
		fmt.Sprintf("UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL, released_at=now(), release_evidence='owner_discard' WHERE id='%s'", e.hold),
		"ALTER TABLE recovery_custody_holds ENABLE TRIGGER recovery_inventory_hold_guard",
	} {
		if _, err := tx.Exec(e.ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatal(err)
	}

	// Another transaction holds the run row; a request that took the run lock would wait on it.
	blocker, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(e.ctx) }()
	if _, err := blocker.Exec(e.ctx, "SELECT 1 FROM runs WHERE id=$1 FOR UPDATE", e.run); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 5*time.Second)
	defer cancel()
	start := time.Now()
	res, err := e.svc.Release(ctx, e.w, e.run, e.request())
	if err != nil || !res.Retained || res.Reason != HeldReleaseNotEligible || res.Released {
		t.Fatalf("release = %+v, %v; want retained %s", res, err, HeldReleaseNotEligible)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("the refusal took %v: it waited for the run lock", d)
	}
	if e.lists() != 0 {
		t.Fatal("a not-eligible hold listed the forge")
	}
}

func TestHeldFinalRetainedLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(e *heldEnv)
		reason string
	}{
		{"cancelled run", func(e *heldEnv) { e.exec("UPDATE runs SET status='cancelled' WHERE id=$1", e.run) }, HeldReleaseNotEligible},
		{"running run", func(e *heldEnv) { e.exec("UPDATE runs SET status='running' WHERE id=$1", e.run) }, HeldReleaseNotEligible},
		{"push_secret_blocked", func(e *heldEnv) { e.exec("UPDATE runs SET fail_origin='push_secret_blocked' WHERE id=$1", e.run) }, HeldReleaseNotEligible},
		{"worker_residue_blocked", func(e *heldEnv) { e.exec("UPDATE runs SET fail_origin='worker_residue_blocked' WHERE id=$1", e.run) }, HeldReleaseNotEligible},
		{"stale generation", func(e *heldEnv) { e.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", e.run) }, HeldReleaseNotEligible},
		{"run claimed elsewhere", func(e *heldEnv) {
			other := uuid.New()
			e.exec("INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')", other, e.user, "other-"+other.String(), other[:])
			e.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", e.run, other)
		}, HeldReleaseNotEligible},
		{"publication of another id", func(e *heldEnv) { e.reqPub = uuid.New() }, HeldReleaseNotEligible},
		{"publication not created", func(e *heldEnv) {
			e.exec("ALTER TABLE run_held_publications DISABLE TRIGGER held_publication_guard")
			defer e.exec("ALTER TABLE run_held_publications ENABLE TRIGGER held_publication_guard")
			e.exec("UPDATE run_held_publications SET state='create_unknown' WHERE id=$1", e.pub)
		}, HeldReleaseNotEligible},
		{"missing inventory capability", func(e *heldEnv) {
			e.w.ProtocolCapabilities = []string{capability.RecoveryHeldPublicationV1}
		}, HeldReleaseUnsupported},
		{"missing held capability", func(e *heldEnv) {
			e.w.ProtocolCapabilities = []string{capability.RecoveryInventoryV1}
		}, HeldReleaseUnsupported},
		{"switch off", func(e *heldEnv) { e.svc.heldEnabled = false }, HeldReleaseUnsupported},
		{"forge not wired", func(e *heldEnv) { e.svc.heldRemote = nil }, HeldReleaseUnavailable},
		{"retention under a second", func(e *heldEnv) {
			e.svc.limits.ReadyRetention = 500 * time.Millisecond
		}, HeldReleaseUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newHeldEnv(t)
			tc.setup(e)
			res, err := e.release(e.w, e.request())
			e.wantRetained(res, err, tc.reason)
		})
	}

	t.Run("capabilities lost between the unlocked gate and the lock", func(t *testing.T) {
		e := newHeldEnv(t)
		e.svc.heldPreLockHook = func() {
			e.exec("UPDATE workers SET protocol_capabilities='{}' WHERE id=$1", e.w.ID)
		}
		res, err := e.release(e.w, e.request())
		e.wantRetained(res, err, HeldReleaseUnsupported)
	})
	t.Run("foreign worker", func(t *testing.T) {
		e := newHeldEnv(t)
		foreign := e.w
		foreign.ID = uuid.New()
		_, err := e.release(foreign, e.request())
		e.wantErr(err, ErrNotAuthorized)
	})
	t.Run("foreign generation", func(t *testing.T) {
		e := newHeldEnv(t)
		req := e.request()
		g := int64(7)
		req.Generation = &g
		_, err := e.release(e.w, req)
		e.wantErr(err, ErrNotAuthorized)
	})
}

func TestHeldFinalForgeRecheckLiveDB(t *testing.T) {
	ref := func(e *heldEnv) string { return pushbroker.HeldRef(e.run, 1) }
	t.Run("ref missing", func(t *testing.T) {
		e := newHeldEnv(t)
		e.tips = map[string]string{}
		res, err := e.release(e.w, e.request())
		e.wantRetained(res, err, HeldReleaseRefMissing)
	})
	t.Run("ref at a foreign tip", func(t *testing.T) {
		e := newHeldEnv(t)
		e.tips[ref(e)] = strings.Repeat("9", 40)
		res, err := e.release(e.w, e.request())
		e.wantRetained(res, err, HeldReleaseRefMismatch)
	})
	t.Run("listing fails", func(t *testing.T) {
		e := newHeldEnv(t)
		e.listErr = errors.New("git ls-remote: https://user:secretpat@forge.invalid unreachable")
		res, err := e.release(e.w, e.request())
		e.wantRetained(res, err, HeldReleaseUnavailable)
		if strings.Contains(fmt.Sprintf("%+v", res), "secretpat") {
			t.Fatal("the forge error reached the response")
		}
	})
	t.Run("connection derivation fails", func(t *testing.T) {
		e := newHeldEnv(t)
		e.remoteErr = errors.New("held publication: the SSRF gate refused")
		res, err := e.release(e.w, e.request())
		e.wantRetained(res, err, HeldReleaseUnavailable)
		if e.lists() != 0 {
			t.Fatal("listed without a connection")
		}
	})
	t.Run("listing bounded by the recheck timeout", func(t *testing.T) {
		e := newHeldEnv(t)
		var deadline time.Time
		e.svc.heldListFn = func(ctx context.Context, _ pushbroker.ListRefsOptions, _ ...string) (map[string]string, error) {
			deadline, _ = ctx.Deadline()
			return nil, ctx.Err()
		}
		start := time.Now()
		_, err := e.release(e.w, e.request())
		if err != nil || deadline.IsZero() || deadline.Sub(start) > heldListTimeout+time.Second {
			t.Fatalf("err=%v deadline in %v, want within %v", err, deadline.Sub(start), heldListTimeout)
		}
	})
	t.Run("ref gone is not decided before the locked state agrees", func(t *testing.T) {
		// A cancelled run with a missing ref is not eligible, not ref_missing.
		e := newHeldEnv(t)
		e.tips = map[string]string{}
		e.exec("UPDATE runs SET status='cancelled' WHERE id=$1", e.run)
		res, err := e.release(e.w, e.request())
		e.wantRetained(res, err, HeldReleaseNotEligible)
	})
}

func TestHeldFinalBadRequestLiveDB(t *testing.T) {
	mut := map[string]func(*apitypes.RecoveryReleaseRequest){
		"no generation":     func(r *apitypes.RecoveryReleaseRequest) { r.Generation = nil },
		"zero generation":   func(r *apitypes.RecoveryReleaseRequest) { g := int64(0); r.Generation = &g },
		"no publication id": func(r *apitypes.RecoveryReleaseRequest) { r.FinalDisposition.PublicationID = "" },
		"bad publication id": func(r *apitypes.RecoveryReleaseRequest) {
			r.FinalDisposition.PublicationID = "not-a-uuid"
		},
		"uppercase publication id": func(r *apitypes.RecoveryReleaseRequest) {
			r.FinalDisposition.PublicationID = strings.ToUpper(r.FinalDisposition.PublicationID)
		},
		"no tip":    func(r *apitypes.RecoveryReleaseRequest) { r.FinalDisposition.SourceSha = "" },
		"short tip": func(r *apitypes.RecoveryReleaseRequest) { r.FinalDisposition.SourceSha = "abcd1234" },
		"uppercase tip": func(r *apitypes.RecoveryReleaseRequest) {
			r.FinalDisposition.SourceSha = strings.ToUpper(heldTestTip + "a")
		},
		"48-hex tip": func(r *apitypes.RecoveryReleaseRequest) { r.FinalDisposition.SourceSha = strings.Repeat("a", 48) },
		"bad digest": func(r *apitypes.RecoveryReleaseRequest) { r.FinalDisposition.CoverageDigest = "abc" },
		"uppercase digest": func(r *apitypes.RecoveryReleaseRequest) {
			r.FinalDisposition.CoverageDigest = strings.ToUpper(heldTestDigest)
		},
		"capture id given": func(r *apitypes.RecoveryReleaseRequest) { r.FinalDisposition.CaptureID = uuid.NewString() },
	}
	for name, m := range mut {
		t.Run(name, func(t *testing.T) {
			e := newHeldEnv(t)
			// Shape is stateless: even a worker that cannot be authenticated for a hold gets 400.
			foreign := e.w
			foreign.ID = uuid.New()
			req := e.request()
			m(&req)
			if _, err := e.release(foreign, req); !errors.Is(err, ErrBadRequest) {
				t.Fatalf("err = %v, want ErrBadRequest", err)
			}
			if e.lists() != 0 {
				t.Fatal("a malformed request listed the forge")
			}
			e.wantOpen()
		})
	}
}

// A release whose acknowledge matches no row rolls the hold release back with it.
func TestHeldFinalAcknowledgeMatchesNoRowRollsBackLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	e.svc.heldPreAckHook = func(tx pgx.Tx) {
		// The release already ran in this transaction; take the publication out of 'created' so
		// the acknowledge CAS finds nothing.
		if _, err := tx.Exec(e.ctx, "UPDATE run_held_publications SET state='delete_unknown' WHERE id=$1", e.pub); err != nil {
			t.Errorf("hook: %v", err)
		}
	}
	_, err := e.release(e.w, e.request())
	if !errors.Is(err, errHeldAcknowledgeLost) {
		t.Fatalf("err = %v, want errHeldAcknowledgeLost", err)
	}
	// Rolled back: the hold is open again and the publication is as it was.
	var state string
	if err := e.pool.QueryRow(e.ctx, "SELECT state FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&state); err != nil || state != "open" {
		t.Fatalf("hold = %q, %v; want open", state, err)
	}
	var pubState string
	if err := e.pool.QueryRow(e.ctx, "SELECT state FROM run_held_publications WHERE id=$1", e.pub).Scan(&pubState); err != nil || pubState != "created" {
		t.Fatalf("publication = %q, %v; want created", pubState, err)
	}
	if len(e.settled) != 0 {
		t.Fatal("custody settled for a rolled-back release")
	}
	// The retry succeeds.
	e.svc.heldPreAckHook = nil
	e.mustRelease()
}
