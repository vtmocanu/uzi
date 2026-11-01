package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3727 (C2 M2) through the REAL observer: workersvc's closure-final port, the handler's
// adapter, the production limiter wiring (WorkerRoutes -> prerequisiteFinalAllowFor on
// mw.Limiter) and the HTTP release route, against Postgres. Only the forge is synthetic: a
// loopback smart-HTTP server that advertises refs, with a project lookup stub. Swapping the
// adapter's Present/Absent/Foreign mapping, or the limiter wiring, turns these red.

// pinGit is a fake git smart-HTTP remote advertising a mutable ref set.
type pinGit struct {
	mu   sync.Mutex
	refs map[string]string
	fail bool
	hits atomic.Int32
	// onList runs on every served listing, before the answer (a hook for the lock-bound test).
	onList func()
}

func (g *pinGit) set(ref, tip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if tip == "" {
		delete(g.refs, ref)
		return
	}
	g.refs[ref] = tip
}

func (g *pinGit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/info/refs") {
		http.NotFound(w, r)
		return
	}
	g.hits.Add(1)
	g.mu.Lock()
	onList := g.onList
	fail := g.fail
	names := make([]string, 0, len(g.refs))
	tips := map[string]string{}
	for k, v := range g.refs {
		names = append(names, k)
		tips[k] = v
	}
	g.mu.Unlock()
	if onList != nil {
		onList()
	}
	if fail {
		http.Error(w, "synthetic listing failure", http.StatusInternalServerError)
		return
	}
	sort.Strings(names)
	svc := r.URL.Query().Get("service")
	w.Header().Set("Content-Type", "application/x-"+svc+"-advertisement")
	enc := pktline.NewEncoder(w)
	_ = enc.EncodeString("# service=" + svc + "\n")
	_ = enc.Flush()
	for i, n := range names {
		line := tips[n] + " " + n
		if i == 0 {
			line += "\x00report-status delete-refs"
		}
		_ = enc.EncodeString(line + "\n")
	}
	_ = enc.Flush()
}

// pinProjectForge answers the project identity check for the one fixture repository.
type pinProjectForge struct {
	forgetest.BaseFake
	project forge.Project
}

func (f *pinProjectForge) GetProject(ctx context.Context, _ int64) (forge.Project, error) {
	return f.project, ctx.Err()
}

type pinForgeBuilder struct{ f forge.Forge }

func (b pinForgeBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.f, nil
}

const (
	wiredPinTip   = "1111111111111111111111111111111111111111"
	wiredOtherTip = "2222222222222222222222222222222222222222"
)

type wiredFinal struct {
	*recoveryEnv
	h       *Handler
	svc     *recovery.Service
	git     *pinGit
	capture uuid.UUID
	body    []byte
	pinRef  string
	expires time.Time
	worker  store.Worker
}

// newWiredFinal seeds a completed run holding one thin capture whose closure is driven to
// pinned through the production queries, with the forge served by a loopback git remote.
func newWiredFinal(t *testing.T, maxAdmissions int) *wiredFinal {
	t.Helper()
	e := newPrerequisiteClosureEnv(t)
	git := &pinGit{refs: map[string]string{"refs/heads/main": wiredOtherTip}}
	srv := httptest.NewServer(git)
	t.Cleanup(srv.Close)

	sealed, err := e.box.Seal([]byte("fixture-credential"))
	if err != nil {
		t.Fatal(err)
	}
	caps := []string{capability.RecoveryArchiveV1, capability.RecoveryInventoryV1, capability.RecoveryPrerequisiteClosureV1, capability.RecoveryHeldPublicationV2}
	e.mustExec("UPDATE forge_connections SET base_url='https://github.com', token_ciphertext=$2 WHERE user_id=$1", e.user, sealed)
	e.mustExec("UPDATE repos SET web_url='https://github.com/g/m2' WHERE id=$1", e.repo)
	e.mustExec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.worker.ID, caps)
	e.mustExec("UPDATE runs SET worker_id=$2, claim_generation=1 WHERE id=$1", e.run, e.worker.ID)
	e.worker.ProtocolCapabilities = caps

	h := e.handler()
	h.cfg.PrerequisiteClosureForges = []string{"github"}
	h.wsvc.SetTxBeginner(e.pool)
	h.wsvc.SetForgeBaseURLAllowed(func(string) bool { return true })
	h.wsvc.SetForges(pinForgeBuilder{f: &pinProjectForge{project: forge.Project{
		ForgeProjectID: 1, PathWithNamespace: "g/m2", WebURL: srv.URL + "/g/m2", DefaultBranch: "main"}}})
	// Production order: the shared recovery service exists before the worker routes mount, and
	// mounting hands it the per-worker admission gate built on the route limiter.
	svc := h.RecoveryService()
	h.WorkerRoutes(mw.NewLimiter(maxAdmissions, time.Hour, nil))
	if !svc.PrerequisiteFinalEnabled() {
		t.Fatal("the closure-backed FINAL is not enabled by the production wiring")
	}

	gen := int64(1)
	res, err := svc.Reserve(e.ctx, e.worker, e.run, apitypes.RecoveryReserveRequest{
		RunID: e.run.String(), IdempotencyKey: "wired-final", SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64), Generation: &gen})
	if err != nil {
		t.Fatal(err)
	}
	capture := uuid.MustParse(res.CaptureID)
	body := []byte("thin archive declaration")
	m := manifestFor(body)
	m.PrerequisiteShas = []string{strings.Repeat("b", 40)}
	if _, err := svc.Upload(e.ctx, e.worker, e.run, capture, m, strings.NewReader(string(body))); err != nil {
		t.Fatal(err)
	}
	q := store.New(e.pool)
	row, err := q.GetPrerequisiteClosureForCapture(e.ctx, capture)
	if err != nil {
		t.Fatal(err)
	}
	row, err = q.ReservePrerequisitePreparation(e.ctx, store.ReservePrerequisitePreparationParams{ID: row.ID, LeaseSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	e.mustExec(`UPDATE recovery_prerequisite_closures p SET state='invoked',live_run_id=p.run_id,
 invoked_at=now(),invoked_deadline=now()+interval '30 seconds',pin_tip=$2,
 connection_id=r.connection_id,forge_kind='github',normalized_base_url='https://github.com',
 project_id=r.forge_project_id,default_branch=r.default_branch
 FROM repos r WHERE p.id=$1 AND r.id=p.repo_id`, row.ID, wiredPinTip)
	if n, err := q.RecordPrerequisitePinned(e.ctx, store.RecordPrerequisitePinnedParams{ID: row.ID, Generation: row.Attempts}); err != nil || n != 1 {
		t.Fatalf("pin: %d %v", n, err)
	}
	pinned, err := q.GetPrerequisiteClosureForCapture(e.ctx, capture)
	if err != nil || pinned.State != "pinned" {
		t.Fatalf("closure not pinned: %+v %v", pinned, err)
	}
	e.mustExec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	// From here the repository lives on the loopback remote.
	e.mustExec("UPDATE repos SET web_url=$2 WHERE id=$1", e.repo, srv.URL+"/g/m2")
	f := &wiredFinal{recoveryEnv: e, h: h, svc: svc, git: git, capture: capture, body: body, pinRef: pinned.PinRef, worker: e.worker}
	if err := e.pool.QueryRow(e.ctx, "SELECT expires_at FROM recovery_captures WHERE id=$1", capture).Scan(&f.expires); err != nil {
		t.Fatal(err)
	}
	git.set(f.pinRef, wiredPinTip)
	return f
}

func (f *wiredFinal) release() (int, map[string]any) {
	f.t.Helper()
	gen := int64(1)
	rec := heldRouteRelease(f.t, f.h, f.worker, f.run, apitypes.RecoveryReleaseRequest{Generation: &gen, FinalDisposition: &apitypes.RecoveryFinalDisposition{
		Kind: "archive", CaptureID: f.capture.String(), SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64)}})
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		f.t.Fatalf("body %q is not JSON: %v", rec.Body.String(), err)
	}
	return rec.Code, m
}

func (f *wiredFinal) closure() store.RecoveryPrerequisiteClosure {
	f.t.Helper()
	row, err := store.New(f.pool).GetPrerequisiteClosureForCapture(f.ctx, f.capture)
	if err != nil {
		f.t.Fatal(err)
	}
	return row
}

func (f *wiredFinal) assertOpen() {
	f.t.Helper()
	var open bool
	if err := f.pool.QueryRow(f.ctx, `SELECT state='open' AND live_worker_id IS NOT NULL AND final_disposition IS NULL
 AND final_capture_id IS NULL AND final_closure_id IS NULL AND release_evidence IS NULL FROM recovery_custody_holds WHERE id=$1`, f.holdID).Scan(&open); err != nil || !open {
		f.t.Fatalf("hold is not untouched-open: %v %v", open, err)
	}
}

func (f *wiredFinal) assertReleased() {
	f.t.Helper()
	var ok bool
	if err := f.pool.QueryRow(f.ctx, `SELECT h.state='released' AND h.final_disposition='archive' AND h.release_evidence='prerequisite_closure'
 AND h.final_capture_id=$2 AND h.final_closure_id=p.id AND h.live_worker_id IS NULL AND h.live_run_id IS NULL
 FROM recovery_custody_holds h JOIN recovery_prerequisite_closures p ON p.capture_id=$2 WHERE h.id=$1`, f.holdID, f.capture).Scan(&ok); err != nil || !ok {
		f.t.Fatalf("hold is not a closure-backed release: %v %v", ok, err)
	}
}

func TestHandlerPrerequisiteFinalPositiveLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		then func(f *wiredFinal)
	}{
		{"pin present at pin_tip", func(*wiredFinal) {}},
		// The source branch was force-pushed and another branch pruned after pinning: only the
		// pin ref matters, and it is still listed at the pinned tip.
		{"source branch rewritten and pruned", func(f *wiredFinal) {
			f.git.set("refs/heads/main", "3333333333333333333333333333333333333333")
			f.git.set("refs/heads/feature", "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWiredFinal(t, 100)
			tc.then(f)
			before := f.closure()
			code, m := f.release()
			if code != http.StatusOK || m["released"] != true || m["holds_released"] != float64(1) || m["generation"] != float64(1) {
				t.Fatalf("FINAL = %d %v", code, m)
			}
			f.assertReleased()
			after := f.closure()
			if after.State != "pinned" || after.AbsentSince.Valid || after.PinTip != before.PinTip || after.PinRef != before.PinRef ||
				after.CleanupAttempts != before.CleanupAttempts || after.LiveRunID != before.LiveRunID {
				t.Fatalf("release moved the pin: %+v -> %+v", before, after)
			}
			var kept bool
			if err := f.pool.QueryRow(f.ctx, `SELECT state='available' AND expires_at=$2 AND local_replica_worker_id=$3 FROM recovery_captures WHERE id=$1`,
				f.capture, f.expires, f.worker.ID).Scan(&kept); err != nil || !kept {
				t.Fatalf("capture not kept with its expiry: %v %v", kept, err)
			}
			rr := httptest.NewRecorder()
			if err := f.svc.Download(f.ctx, rr, f.user, f.run, f.capture); err != nil || rr.Body.String() != string(f.body) {
				t.Fatalf("capture bytes not kept: %q %v", rr.Body.String(), err)
			}
			if f.git.hits.Load() != 1 {
				t.Fatalf("expected exactly one forge listing, got %d", f.git.hits.Load())
			}
			// An exact replay is answered without another listing.
			code, m = f.release()
			if code != http.StatusOK || m["released"] != true || f.git.hits.Load() != 1 {
				t.Fatalf("replay = %d %v hits=%d", code, m, f.git.hits.Load())
			}
		})
	}
}

func TestHandlerPrerequisiteFinalAbsentPinLiveDB(t *testing.T) {
	f := newWiredFinal(t, 100)
	f.git.set(f.pinRef, "")
	code, m := f.release()
	if code != http.StatusConflict || m["reason"] != "prerequisite_pin_missing" {
		t.Fatalf("FINAL = %d %v", code, m)
	}
	f.assertOpen()
	row := f.closure()
	if row.State != "pinned" || !row.AbsentSince.Valid || !row.AbsencePassID.Valid {
		t.Fatalf("absence not committed: %+v", row)
	}
	// The recorded absence refuses before any further listing.
	hits := f.git.hits.Load()
	if code, m = f.release(); code != http.StatusConflict || m["reason"] != "prerequisite_pin_missing" || f.git.hits.Load() != hits {
		t.Fatalf("second FINAL = %d %v hits %d->%d", code, m, hits, f.git.hits.Load())
	}
}

func TestHandlerPrerequisiteFinalForeignPinLiveDB(t *testing.T) {
	f := newWiredFinal(t, 100)
	f.git.set(f.pinRef, wiredOtherTip)
	code, m := f.release()
	if code != http.StatusConflict || m["reason"] != "prerequisite_pin_foreign" {
		t.Fatalf("FINAL = %d %v", code, m)
	}
	f.assertOpen()
	if row := f.closure(); row.State != "foreign" || row.LiveRunID.Valid {
		t.Fatalf("foreign not committed: %+v", row)
	}
	if got, ok := f.gitTip(); !ok || got != wiredOtherTip {
		t.Fatalf("the foreign ref was touched: %q %v", got, ok)
	}
}

func (f *wiredFinal) gitTip() (string, bool) {
	f.git.mu.Lock()
	defer f.git.mu.Unlock()
	tip, ok := f.git.refs[f.pinRef]
	return tip, ok
}

func TestHandlerPrerequisiteFinalListingFailureLiveDB(t *testing.T) {
	f := newWiredFinal(t, 100)
	f.git.mu.Lock()
	f.git.fail = true
	f.git.mu.Unlock()
	code, m := f.release()
	if code != http.StatusServiceUnavailable || m["reason"] != "prerequisite_pin_unavailable" {
		t.Fatalf("FINAL = %d %v", code, m)
	}
	f.assertOpen()
	if row := f.closure(); row.State != "pinned" || row.AbsentSince.Valid || row.CleanupAttempts != 0 {
		t.Fatalf("a failed listing wrote the closure: %+v", row)
	}
	if f.git.hits.Load() != 1 {
		t.Fatalf("hits %d", f.git.hits.Load())
	}
}

// Rate-bound control through the REAL limiter: with a budget of N, N failing requests spend
// every admission (none is refunded), and request N+1 is denied with no forge call and no
// reservation, even after the forge recovers.
func TestHandlerPrerequisiteFinalRateBoundLiveDB(t *testing.T) {
	const n = 3
	f := newWiredFinal(t, n)
	f.git.mu.Lock()
	f.git.fail = true
	f.git.mu.Unlock()
	for i := 1; i <= n; i++ {
		if code, m := f.release(); code != http.StatusServiceUnavailable || m["reason"] != "prerequisite_pin_unavailable" {
			t.Fatalf("failing request %d = %d %v", i, code, m)
		}
		if got := int(f.git.hits.Load()); got != i {
			t.Fatalf("after request %d the forge saw %d listings", i, got)
		}
	}
	before := f.closure()
	f.git.mu.Lock()
	f.git.fail = false
	f.git.mu.Unlock()
	code, m := f.release()
	if code != http.StatusServiceUnavailable || m["reason"] != "prerequisite_pin_unavailable" {
		t.Fatalf("request N+1 = %d %v", code, m)
	}
	if f.git.hits.Load() != n {
		t.Fatalf("request N+1 reached the forge: %d listings", f.git.hits.Load())
	}
	after := f.closure()
	if after.LastAuditPassID != before.LastAuditPassID || after.State != before.State || after.AbsentSince != before.AbsentSince {
		t.Fatalf("request N+1 reserved or wrote the closure: %+v -> %+v", before, after)
	}
	f.assertOpen()
}

// The workersvc transactions (listing reservation, absence and foreign outcomes) bound their lock
// waits on the server: a request blocked behind another session ends its own backend, so no lock
// stays held and no connection keeps waiting after the 503.
func TestHandlerPrerequisiteFinalServerSideLockBoundsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		// lockAt is when the competing session takes the closure row lock.
		lockAt string
		setup  func(f *wiredFinal)
	}{
		{"listing reservation", "before", func(*wiredFinal) {}},
		{"absence outcome", "listing", func(f *wiredFinal) { f.git.set(f.pinRef, "") }},
		{"foreign outcome", "listing", func(f *wiredFinal) { f.git.set(f.pinRef, wiredOtherTip) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWiredFinal(t, 100)
			tc.setup(f)
			closureID := f.closure().ID
			holder, err := pgx.ConnectConfig(f.ctx, f.pool.Config().ConnConfig.Copy())
			if err != nil {
				t.Fatal(err)
			}
			htx, err := holder.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = htx.Rollback(context.Background())
				_ = holder.Close(context.Background())
			})
			lock := func() {
				if _, err := htx.Exec(f.ctx, "SELECT 1 FROM recovery_prerequisite_closures WHERE id=$1 FOR UPDATE", closureID); err != nil {
					t.Errorf("hold closure row: %v", err)
				}
			}
			if tc.lockAt == "before" {
				lock()
			} else {
				f.git.mu.Lock()
				f.git.onList = lock
				f.git.mu.Unlock()
			}
			obs, err := pgx.ConnectConfig(f.ctx, f.pool.Config().ConnConfig.Copy())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = obs.Close(context.Background()) })
			type result struct {
				code int
				m    map[string]any
				at   time.Time
			}
			done := make(chan result, 1)
			start := time.Now()
			go func() {
				code, m := f.release()
				done <- result{code, m, time.Now()}
			}()
			var pid int32
			for deadline := time.Now().Add(5 * time.Second); pid == 0; time.Sleep(5 * time.Millisecond) {
				err := obs.QueryRow(f.ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database()
 AND wait_event_type='Lock' AND strpos(query,'recovery_prerequisite_closures')>0 LIMIT 1`).Scan(&pid)
				if err != nil && err != pgx.ErrNoRows {
					t.Fatal(err)
				}
				if pid == 0 && time.Now().After(deadline) {
					t.Fatal("the FINAL never waited on the closure row")
				}
			}
			var r result
			select {
			case r = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the blocked FINAL did not return")
			}
			if r.code != http.StatusServiceUnavailable || r.m["reason"] != "prerequisite_pin_unavailable" {
				t.Fatalf("blocked FINAL = %d %v", r.code, r.m)
			}
			if d := r.at.Sub(start); d > 4*time.Second {
				t.Fatalf("blocked FINAL took %v", d)
			}
			time.Sleep(600 * time.Millisecond)
			var waiting, held int
			if err := obs.QueryRow(f.ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock'`, pid).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if err := obs.QueryRow(f.ctx, `SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='transactionid' AND granted`, pid).Scan(&held); err != nil {
				t.Fatal(err)
			}
			if waiting != 0 || held != 0 {
				t.Fatalf("backend %d still waits (%d) or holds row locks (%d) after the response", pid, waiting, held)
			}
			f.assertOpen()
		})
	}
}

// lockProbeBeginner records, before every statement of every transaction workersvc opens, the
// lock_timeout the server will apply to it. A client-side deadline alone does not end a backend
// that is waiting on a lock, so the bound has to be set on the server.
type lockProbeBeginner struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	seen []string
}

func (b *lockProbeBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &lockProbeTx{Tx: tx, b: b}, nil
}

type lockProbeTx struct {
	pgx.Tx
	b *lockProbeBeginner
}

func (t *lockProbeTx) probe(ctx context.Context, sql string) {
	if strings.HasPrefix(sql, "SET LOCAL") {
		return
	}
	var v string
	if err := t.Tx.QueryRow(ctx, "SHOW lock_timeout").Scan(&v); err != nil {
		v = "probe failed: " + err.Error()
	}
	t.b.mu.Lock()
	t.b.seen = append(t.b.seen, v)
	t.b.mu.Unlock()
}

func (t *lockProbeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.probe(ctx, sql)
	return t.Tx.Exec(ctx, sql, args...)
}

func (t *lockProbeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.probe(ctx, sql)
	return t.Tx.Query(ctx, sql, args...)
}

func (t *lockProbeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.probe(ctx, sql)
	return t.Tx.QueryRow(ctx, sql, args...)
}

func TestHandlerPrerequisiteFinalTransactionsSetServerLockTimeoutLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *wiredFinal)
		code  int
		stmts int // reservation, plus the outcome transaction when there is one
	}{
		{"present", func(*wiredFinal) {}, http.StatusOK, 3},
		{"absent", func(f *wiredFinal) { f.git.set(f.pinRef, "") }, http.StatusConflict, 6},
		{"foreign", func(f *wiredFinal) { f.git.set(f.pinRef, wiredOtherTip) }, http.StatusConflict, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWiredFinal(t, 100)
			probe := &lockProbeBeginner{pool: f.pool}
			f.h.wsvc.SetTxBeginner(probe)
			tc.setup(f)
			if code, m := f.release(); code != tc.code {
				t.Fatalf("FINAL = %d %v", code, m)
			}
			probe.mu.Lock()
			defer probe.mu.Unlock()
			if len(probe.seen) < tc.stmts {
				t.Fatalf("workersvc ran only %d statements, want at least %d: %v", len(probe.seen), tc.stmts, probe.seen)
			}
			for i, v := range probe.seen {
				if v != "500ms" {
					t.Fatalf("statement %d ran with lock_timeout=%q, want 500ms: %v", i, v, probe.seen)
				}
			}
		})
	}
}
