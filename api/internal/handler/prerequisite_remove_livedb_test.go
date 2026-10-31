package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type prerequisiteRemovalFixture struct {
	t      *testing.T
	ctx    context.Context
	h      *Handler
	router http.Handler
	pool   *pgxpool.Pool
	owner  uuid.UUID
	jwt    string
}

type prerequisiteRemovalRun struct {
	conn    uuid.UUID
	repo    uuid.UUID
	run     uuid.UUID
	worker  uuid.UUID
	hold    uuid.UUID
	capture uuid.UUID
	closure store.RecoveryPrerequisiteClosure
}

func newPrerequisiteRemovalFixture(t *testing.T) *prerequisiteRemovalFixture {
	t.Helper()
	// Register database drop before cliLiveDB registers pool.Close: LIFO cleanup
	// closes the pool first. Immutable closure/capture/hold tombstones stay intact.
	name := ephemeralPriorityDatabase(t)
	h, router, pool := cliLiveDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	assertPriorityDatabase(t, ctx, pool, name)
	owner := cliSeedUser(t, pool, false)
	f := &prerequisiteRemovalFixture{t: t, ctx: ctx, h: h, pool: pool, owner: owner}
	f.jwt = cliMintJWT(t, pool, owner)
	f.router = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r.WithContext(f.ctx))
	})
	return f
}

func (f *prerequisiteRemovalFixture) exec(db store.DBTX, sql string, args ...any) {
	f.t.Helper()
	if _, err := db.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *prerequisiteRemovalFixture) seedRun() prerequisiteRemovalRun {
	f.t.Helper()
	r := prerequisiteRemovalRun{conn: uuid.New(), hold: uuid.New(), capture: uuid.New()}
	f.exec(f.pool, `INSERT INTO forge_connections
		(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
		VALUES($1,$2,'github',$4,'bot',1,$3)`, r.conn, f.owner, []byte{1}, "https://"+r.conn.String()+".example.com/")
	r.repo = rmSeedRepo(f.t, f.pool, r.conn, 1, false)
	r.worker = custodySeedWorker(f.t, f.pool, f.owner)
	r.run = rmSeedRun(f.t, f.pool, f.owner, r.repo, "claimed")
	f.exec(f.pool, "UPDATE runs SET worker_id=$2,claim_generation=1 WHERE id=$1", r.run, r.worker)
	return r
}

func (f *prerequisiteRemovalFixture) seedArchive(db store.DBTX, r *prerequisiteRemovalRun) {
	f.t.Helper()
	f.exec(db, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
		 live_worker_id,live_run_id,inventory_guarded)
		VALUES($1,$2,$3,$4,1,'open',$5,'prerequisite-worker',$5,$4,true)`,
		r.hold, f.owner, r.repo, r.run, r.worker)
	f.exec(db, `INSERT INTO recovery_captures
		(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,
		 source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,chunk_count,
		 prerequisite_shas,reserved_bytes,coverage_digest,expires_at)
		VALUES($1,$2,$3,$4,$5,'prerequisite-worker',repeat('a',40),$1::uuid::text,
		 'available',true,3,repeat('f',64),1,ARRAY[repeat('b',40),repeat('c',40)],
		 3,repeat('a',64),now()+interval '1 day')`, r.capture, r.hold, r.run, f.owner, r.worker)
	var err error
	r.closure, err = store.New(db).InsertPrerequisiteClosure(f.ctx, r.capture)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *prerequisiteRemovalFixture) pending() prerequisiteRemovalRun {
	f.t.Helper()
	r := f.seedRun()
	f.seedArchive(f.pool, &r)
	f.exec(f.pool, "UPDATE runs SET status='failed' WHERE id=$1", r.run)
	return r
}

func removalText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
func removalUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func removalRows(t *testing.T, n int64, err error) {
	t.Helper()
	if err != nil || n != 1 {
		t.Fatalf("affected rows = %d, err = %v", n, err)
	}
}

func (f *prerequisiteRemovalFixture) discard(q *store.Queries, r prerequisiteRemovalRun) {
	f.t.Helper()
	n, err := q.DiscardCustodyHoldForOwner(f.ctx, store.DiscardCustodyHoldForOwnerParams{
		HoldID: r.hold, RunID: r.run, UserID: f.owner, ReleaseEvidence: removalText("owner_discard"),
	})
	removalRows(f.t, n, err)
}

func (f *prerequisiteRemovalFixture) marker(tx pgx.Tx, r prerequisiteRemovalRun) {
	f.t.Helper()
	q := store.New(tx)
	p, err := q.ReservePrerequisitePreparation(f.ctx, store.ReservePrerequisitePreparationParams{ID: r.closure.ID, LeaseSeconds: 60})
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err = q.LockPrerequisiteRun(f.ctx, r.run); err != nil {
		f.t.Fatal(err)
	}
	if _, err = q.LockPrerequisiteHold(f.ctx, r.hold); err != nil {
		f.t.Fatal(err)
	}
	if _, err = q.LockPrerequisiteCapture(f.ctx, r.capture); err != nil {
		f.t.Fatal(err)
	}
	if _, err = q.LockPrerequisiteClosure(f.ctx, r.closure.ID); err != nil {
		f.t.Fatal(err)
	}
	repo, err := q.GetRepoForUser(f.ctx, store.GetRepoForUserParams{ID: r.repo, UserID: f.owner})
	if err != nil {
		f.t.Fatal(err)
	}
	conn, err := q.GetForgeConnectionForUser(f.ctx, store.GetForgeConnectionForUserParams{ID: repo.ConnectionID, UserID: f.owner})
	if err != nil {
		f.t.Fatal(err)
	}
	c, err := q.MarkPrerequisiteInvoked(f.ctx, store.MarkPrerequisiteInvokedParams{
		ID: r.closure.ID, Generation: p.Attempts, SendSeconds: 60,
		PinTip: removalText(strings.Repeat("d", 40)), ConnectionID: removalUUID(conn.ID),
		ForgeKind: removalText(conn.ForgeType), NormalizedBaseUrl: removalText(strings.TrimRight(conn.BaseUrl, "/")),
		ProjectID: pgtype.Int8{Int64: repo.ForgeProjectID, Valid: true}, DefaultBranch: repo.DefaultBranch,
	})
	if err != nil || c.State != "invoked" || !c.LiveRunID.Valid {
		f.t.Fatalf("marker = %+v, err = %v", c, err)
	}
}

func (f *prerequisiteRemovalFixture) invoke(r prerequisiteRemovalRun) {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.ctx) }()
	f.marker(tx, r)
	// Clear only independent custody, with the actual owner-discard query.
	// No publication or forge-no-output evidence is invented.
	f.discard(store.New(tx), r)
	if err = tx.Commit(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *prerequisiteRemovalFixture) read(r prerequisiteRemovalRun) store.RecoveryPrerequisiteClosure {
	f.t.Helper()
	c, err := store.New(f.pool).GetPrerequisiteClosure(f.ctx, store.GetPrerequisiteClosureParams{CaptureID: r.capture, UserID: f.owner})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func removalPath(route string, r prerequisiteRemovalRun) string {
	if route == "repo" {
		return "/api/repos/" + r.repo.String()
	}
	return "/api/forge/connections/" + r.conn.String()
}

func (f *prerequisiteRemovalFixture) assertSurvives(r prerequisiteRemovalRun) {
	f.t.Helper()
	for _, row := range []struct {
		table string
		id    uuid.UUID
	}{
		{"forge_connections", r.conn}, {"repos", r.repo}, {"runs", r.run},
		{"recovery_custody_holds", r.hold}, {"recovery_captures", r.capture},
		{"recovery_prerequisite_closures", r.closure.ID},
	} {
		var exists bool
		if err := f.pool.QueryRow(f.ctx, "SELECT EXISTS(SELECT 1 FROM "+row.table+" WHERE id=$1)", row.id).Scan(&exists); err != nil || !exists {
			f.t.Fatalf("%s survives = %v, err = %v", row.table, exists, err)
		}
	}
}

// Only the two removal Exec statements and prerequisite pre-counts are intercepted.
// Auth and all preceding guard reads still use the real pool.
type prerequisiteRemovalDB struct {
	store.DBTX
	countHook func(int) (int64, error)
	onDelete  func(context.Context, string, ...any) (pgconn.CommandTag, error)
	counts    int
	deletes   int
}

type prerequisiteRemovalCountRow struct {
	count int64
	err   error
}

func (r prerequisiteRemovalCountRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*int64)) = r.count
	return nil
}

func (d *prerequisiteRemovalDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.HasPrefix(sql, "-- name: CountLivePrerequisiteClosuresFor") {
		d.counts++
		if d.countHook != nil {
			n, err := d.countHook(d.counts)
			return prerequisiteRemovalCountRow{count: n, err: err}
		}
	}
	return d.DBTX.QueryRow(ctx, sql, args...)
}

func (d *prerequisiteRemovalDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if strings.HasPrefix(sql, "-- name: DeleteRepoForUser ") || strings.HasPrefix(sql, "-- name: DeleteForgeConnectionForUser ") {
		d.deletes++
		if d.onDelete != nil {
			return d.onDelete(ctx, sql, args...)
		}
	}
	return d.DBTX.Exec(ctx, sql, args...)
}

func TestPrerequisiteRemovalGuardsLiveDB(t *testing.T) {
	f := newPrerequisiteRemovalFixture(t)
	for _, state := range []string{"invoked", "create_unknown", "pinned", "first_absence", "cleanup_exhausted"} {
		t.Run(state, func(t *testing.T) {
			fixture := *f
			fixture.t = t
			f := &fixture
			r := f.pending()
			f.invoke(r)
			q := store.New(f.pool)
			if state == "create_unknown" {
				n, err := q.MarkPrerequisiteCreateUnknown(f.ctx, store.MarkPrerequisiteCreateUnknownParams{ID: r.closure.ID, Generation: 1})
				removalRows(t, n, err)
			} else if state != "invoked" {
				n, err := q.RecordPrerequisitePinned(f.ctx, store.RecordPrerequisitePinnedParams{ID: r.closure.ID, Generation: 1})
				removalRows(t, n, err)
			}
			if state == "first_absence" || state == "cleanup_exhausted" {
				f.cleanupState(r, state)
			}
			before := f.read(r)
			d := &prerequisiteRemovalDB{DBTX: f.pool}
			f.h.q = store.New(d)
			for _, route := range []string{"repo", "connection"} {
				rec := cookieReq(t, f.router, http.MethodDelete, removalPath(route, r), f.jwt, "")
				assertPrerequisiteRemovalConflict(t, rec, 1)
				f.assertSurvives(r)
			}
			if d.deletes != 0 || d.counts != 2 {
				t.Fatalf("guard counts = %d, DELETE calls = %d", d.counts, d.deletes)
			}
			after := f.read(r)
			if after.State != before.State || after.LiveRunID != before.LiveRunID ||
				after.Reason != before.Reason || after.AbsentSince != before.AbsentSince ||
				after.CleanupExhaustedAt != before.CleanupExhaustedAt {
				t.Fatalf("removal changed closure: before=%+v after=%+v", before, after)
			}
			f.h.q = store.New(f.pool)
		})
	}
}

// A separate pool resolves now() through this owned database's clock. Lifecycle
// transitions still use generated queries and unchanged production triggers.
func (f *prerequisiteRemovalFixture) cleanupState(r prerequisiteRemovalRun, state string) {
	f.t.Helper()
	f.exec(f.pool, `CREATE TABLE IF NOT EXISTS prerequisite_removal_clock
		(singleton boolean PRIMARY KEY CHECK(singleton), observed_at timestamptz NOT NULL)`)
	f.exec(f.pool, `INSERT INTO prerequisite_removal_clock VALUES(true,pg_catalog.now())
		ON CONFLICT(singleton) DO UPDATE SET observed_at=pg_catalog.now()`)
	f.exec(f.pool, `CREATE OR REPLACE FUNCTION public.now() RETURNS timestamptz LANGUAGE sql STABLE
		AS 'SELECT observed_at FROM public.prerequisite_removal_clock WHERE singleton'`)
	cfg := f.pool.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = "public,pg_catalog"
	clock, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	defer clock.Close()
	q := store.New(clock)
	// Make the capture ineligible for retention through the actual owner API query.
	n, err := q.DiscardCaptureForOwner(f.ctx, store.DiscardCaptureForOwnerParams{ID: r.capture, RunID: r.run, UserID: f.owner})
	removalRows(f.t, n, err)
	if state == "first_absence" {
		pass := removalUUID(uuid.New())
		if _, err = q.ReservePrerequisiteAudit(f.ctx, store.ReservePrerequisiteAuditParams{ID: r.closure.ID, PassID: pass}); err != nil {
			f.t.Fatal(err)
		}
		n, err = q.RecordPrerequisiteAbsence(f.ctx, store.RecordPrerequisiteAbsenceParams{ID: r.closure.ID, PassID: pass, Generation: 0})
		removalRows(f.t, n, err)
		if !f.read(r).AbsentSince.Valid {
			f.t.Fatal("first absence was not recorded")
		}
		return
	}
	// Eight generated reservations/retries, each made due by the isolated clock.
	// No real-time waits, trigger changes, or direct lifecycle-row rewrites.
	for attempt := int32(1); attempt <= 8; attempt++ {
		pass := removalUUID(uuid.New())
		if _, err = q.ReservePrerequisiteCleanup(f.ctx, store.ReservePrerequisiteCleanupParams{ID: r.closure.ID, PassID: pass, LeaseSeconds: 60}); err != nil {
			f.t.Fatal(err)
		}
		n, err = q.RecordPrerequisiteCleanupRetry(f.ctx, store.RecordPrerequisiteCleanupRetryParams{ID: r.closure.ID, Generation: attempt, PassID: pass, LastError: "cleanup unavailable"})
		removalRows(f.t, n, err)
		f.exec(clock, "UPDATE prerequisite_removal_clock SET observed_at=observed_at+interval '2 hours'")
	}
	c := f.read(r)
	if c.CleanupAttempts != 8 || !c.CleanupExhaustedAt.Valid || !c.LiveRunID.Valid {
		f.t.Fatalf("exhausted shape: %+v", c)
	}
}

func TestPrerequisiteRemovalCustodyPrecedenceLiveDB(t *testing.T) {
	f := newPrerequisiteRemovalFixture(t)
	r := f.pending()
	d := &prerequisiteRemovalDB{DBTX: f.pool}
	f.h.q = store.New(d)
	rec := cookieReq(t, f.router, http.MethodDelete, removalPath("repo", r), f.jwt, "")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"custody_holds":1`) ||
		strings.Contains(rec.Body.String(), "prerequisite_closure_count") || d.counts != 0 || d.deletes != 0 {
		t.Fatalf("custody precedence = %d %s, counts=%d deletes=%d", rec.Code, rec.Body.String(), d.counts, d.deletes)
	}
	f.assertSurvives(r)
}

func TestPrerequisiteRemovalOwnerIsolationLiveDB(t *testing.T) {
	f := newPrerequisiteRemovalFixture(t)
	r := f.pending()
	f.invoke(r)
	foreign := cliMintJWT(t, f.pool, cliSeedUser(t, f.pool, false))
	for _, route := range []string{"repo", "connection"} {
		rec := cookieReq(t, f.router, http.MethodDelete, removalPath(route, r), foreign, "")
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "prerequisite") {
			t.Fatalf("%s foreign response = %d %s", route, rec.Code, rec.Body.String())
		}
		f.assertSurvives(r)
	}
}

func TestPrerequisiteRemovalCountFailureLiveDB(t *testing.T) {
	f := newPrerequisiteRemovalFixture(t)
	r := f.pending()
	f.discard(store.New(f.pool), r)
	for _, route := range []string{"repo", "connection"} {
		d := &prerequisiteRemovalDB{DBTX: f.pool, countHook: func(int) (int64, error) {
			return 0, errors.New("private count failure")
		}}
		f.h.q = store.New(d)
		rec := cookieReq(t, f.router, http.MethodDelete, removalPath(route, r), f.jwt, "")
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "private") || d.deletes != 0 || d.counts != 1 {
			t.Fatalf("%s response = %d %s, counts=%d deletes=%d", route, rec.Code, rec.Body.String(), d.counts, d.deletes)
		}
		f.assertSurvives(r)
	}
}

func TestPrerequisiteRemovalFallbackLiveDB(t *testing.T) {
	f := newPrerequisiteRemovalFixture(t)
	r := f.pending()
	f.discard(store.New(f.pool), r)
	for _, route := range []string{"repo", "connection"} {
		for _, mode := range []string{"zero", "recount-error", "wrong-constraint"} {
			t.Run(route+"/"+mode, func(t *testing.T) {
				fixture := *f
				fixture.t = t
				f := &fixture
				d := &prerequisiteRemovalDB{DBTX: f.pool}
				d.countHook = func(call int) (int64, error) {
					if mode == "recount-error" && call == 2 {
						return 0, errors.New("private recount failure")
					}
					return 0, nil
				}
				d.onDelete = func(context.Context, string, ...any) (pgconn.CommandTag, error) {
					constraint := prerequisiteRestrictConstraint
					if mode == "wrong-constraint" {
						constraint = "unrelated_live_run_id_fkey"
					}
					return pgconn.CommandTag{}, &pgconn.PgError{Code: "23503", ConstraintName: constraint, Detail: "private forge credential"}
				}
				f.h.q = store.New(d)
				rec := cookieReq(t, f.router, http.MethodDelete, removalPath(route, r), f.jwt, "")
				wantCounts := 2
				if mode == "wrong-constraint" {
					wantCounts = 1
					if rec.Code != http.StatusInternalServerError {
						t.Fatalf("wrong constraint = %d %s", rec.Code, rec.Body.String())
					}
				} else {
					assertPrerequisiteRemovalConflict(t, rec, 1)
				}
				if strings.Contains(rec.Body.String(), "private") || d.deletes != 1 || d.counts != wantCounts {
					t.Fatalf("response=%s counts=%d deletes=%d", rec.Body.String(), d.counts, d.deletes)
				}
				f.assertSurvives(r)
			})
		}
	}
}

func TestPrerequisiteRemovalPendingAndTerminalLiveDB(t *testing.T) {
	f := newPrerequisiteRemovalFixture(t)
	for _, route := range []string{"repo", "connection"} {
		for _, state := range []string{"pending", "refused", "deleted"} {
			t.Run(route+"/"+state, func(t *testing.T) {
				fixture := *f
				fixture.t = t
				f := &fixture
				r := f.pending()
				if state == "refused" || state == "deleted" {
					f.invoke(r)
					q := store.New(f.pool)
					if state == "refused" {
						n, err := q.RecordPrerequisiteCreateRefused(f.ctx, store.RecordPrerequisiteCreateRefusedParams{ID: r.closure.ID, Generation: 1})
						removalRows(t, n, err)
					} else {
						n, err := q.RecordPrerequisitePinned(f.ctx, store.RecordPrerequisitePinnedParams{ID: r.closure.ID, Generation: 1})
						removalRows(t, n, err)
						n, err = q.DiscardCaptureForOwner(f.ctx, store.DiscardCaptureForOwnerParams{ID: r.capture, RunID: r.run, UserID: f.owner})
						removalRows(t, n, err)
						pass := removalUUID(uuid.New())
						p, err := q.ReservePrerequisiteCleanup(f.ctx, store.ReservePrerequisiteCleanupParams{ID: r.closure.ID, PassID: pass, LeaseSeconds: 60})
						if err != nil {
							t.Fatal(err)
						}
						n, err = q.RecordPrerequisiteDeleted(f.ctx, store.RecordPrerequisiteDeletedParams{ID: r.closure.ID, Generation: p.CleanupAttempts, PassID: pass})
						removalRows(t, n, err)
					}
				} else {
					f.discard(store.New(f.pool), r)
				}
				rec := cookieReq(t, f.router, http.MethodDelete, removalPath(route, r), f.jwt, "")
				if rec.Code != http.StatusNoContent {
					t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
				}
				var runExists bool
				if err := f.pool.QueryRow(f.ctx, "SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1)", r.run).Scan(&runExists); err != nil || runExists {
					t.Fatalf("deleted run exists = %v, err = %v", runExists, err)
				}
				c := f.read(r)
				if c.State != state || c.LiveRunID.Valid {
					t.Fatalf("tombstone = %+v", c)
				}
				// Capture->hold and closure->capture/hold RESTRICT preserve audit rows;
				// they are distinct from a live run pointer and do not lock the run.
				wantCaptureState := "available"
				if state == "deleted" {
					wantCaptureState = "discarded"
				}
				var holdState, captureState string
				if err := f.pool.QueryRow(f.ctx, `SELECT h.state,c.state FROM recovery_custody_holds h
					JOIN recovery_captures c ON c.hold_id=h.id WHERE c.id=$1`, r.capture).Scan(&holdState, &captureState); err != nil || holdState != "discarded" || captureState != wantCaptureState {
					t.Fatalf("audit tombstones = %q/%q, err=%v", holdState, captureState, err)
				}
				if state == "pending" {
					f.h.wsvc.SetTxBeginner(f.pool)
					n, err := f.h.wsvc.SweepPrerequisiteClosures(f.ctx)
					removalRows(t, n, err)
					c = f.read(r)
					if c.State != "refused" || c.Reason != "archive_unavailable" || c.InvokedAt.Valid ||
						c.LiveRunID.Valid || c.LastError != "" || c.Attempts != 1 || c.ReconciliationAttempts != 0 {
						t.Fatalf("delete won, executor outcome = %+v", c)
					}
					n, err = f.h.wsvc.SweepPrerequisiteClosures(f.ctx)
					if err != nil || n != 0 {
						t.Fatalf("terminal retry = %d, err=%v", n, err)
					}
				}
			})
		}
	}
}

func TestPrerequisiteRemovalMarkerRaceLiveDB(t *testing.T) {
	for _, route := range []string{"repo", "connection"} {
		t.Run(route, func(t *testing.T) {
			f := newPrerequisiteRemovalFixture(t)
			r := f.seedRun()
			f.exec(f.pool, "UPDATE runs SET status='failed' WHERE id=$1", r.run)
			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(f.ctx) }()
			// Keep pending archive admission in this transaction too: a committed
			// open hold would correctly trip the earlier repo custody guard.
			f.seedArchive(tx, &r)
			f.marker(tx, r)
			f.discard(store.New(tx), r)
			entered := make(chan struct{})
			release := make(chan struct{})
			deleteResult := make(chan error, 1)
			done := make(chan *httptest.ResponseRecorder, 1)
			finished := make(chan struct{})
			raceCtx, cancelRace := context.WithCancel(f.ctx)
			f.ctx = raceCtx
			d := &prerequisiteRemovalDB{DBTX: f.pool}
			d.onDelete = func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					deleteResult <- ctx.Err()
					return pgconn.CommandTag{}, ctx.Err()
				}
				tag, execErr := f.pool.Exec(ctx, sql, args...)
				deleteResult <- execErr
				return tag, execErr
			}
			f.h.q = store.New(d)
			go func() {
				defer close(finished)
				done <- cookieReq(t, f.router, http.MethodDelete, removalPath(route, r), f.jwt, "")
			}()
			defer func() {
				cancelRace()
				<-finished
			}()
			select {
			case <-entered:
			case rec := <-done:
				t.Fatalf("DELETE was not reached: %d %s", rec.Code, rec.Body.String())
			case <-f.ctx.Done():
				t.Fatal("timed out waiting for DELETE entry")
			}
			if err = tx.Commit(f.ctx); err != nil {
				close(release)
				<-done
				t.Fatal(err)
			}
			close(release)
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-done:
			case <-f.ctx.Done():
				t.Fatal("timed out waiting for DELETE response")
			}
			var actual *pgconn.PgError
			if err = <-deleteResult; !errors.As(err, &actual) || actual.Code != "23503" ||
				actual.ConstraintName != prerequisiteRestrictConstraint {
				t.Fatalf("actual DELETE error = %v", err)
			}
			assertPrerequisiteRemovalConflict(t, rec, 1)
			if d.counts != 2 || d.deletes != 1 {
				t.Fatalf("counts=%d deletes=%d", d.counts, d.deletes)
			}
			f.assertSurvives(r)
			if c := f.read(r); c.State != "invoked" || !c.LiveRunID.Valid {
				t.Fatalf("live closure = %+v", c)
			}
		})
	}
}

func TestPrerequisiteRemovalScheduleControlLiveDB(t *testing.T) {
	f := newPrerequisiteRemovalFixture(t)
	r := f.pending()
	f.invoke(r)
	schedule := uuid.New()
	f.exec(f.pool, `INSERT INTO run_schedules
		(id,user_id,repo_id,target,prompt,timing,cron_expr,timezone)
		VALUES($1,$2,$3,'prompt','maintenance','recurring','0 * * * *','UTC')`, schedule, f.owner, r.repo)
	f.exec(f.pool, "UPDATE runs SET schedule_id=$2 WHERE id=$1", r.run, schedule)
	rec := cookieReq(t, f.router, http.MethodDelete, "/api/schedules/"+schedule.String(), f.jwt, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("schedule delete = %d %s", rec.Code, rec.Body.String())
	}
	f.assertSurvives(r)
	var scheduleID pgtype.UUID
	if err := f.pool.QueryRow(f.ctx, "SELECT schedule_id FROM runs WHERE id=$1", r.run).Scan(&scheduleID); err != nil || scheduleID.Valid {
		t.Fatalf("schedule pointer = %+v, err=%v", scheduleID, err)
	}
	if c := f.read(r); c.State != "invoked" || !c.LiveRunID.Valid {
		t.Fatalf("schedule removal changed closure: %+v", c)
	}
}
