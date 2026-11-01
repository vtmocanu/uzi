package recovery

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Closure-backed FINAL (issue #3727, C2 M2): server-side lock bounds, post-listing state changes
// and the request-ownership guarantees, against Postgres.

func (fx *closureFinalFx) observer() *pgx.Conn {
	fx.t.Helper()
	c, err := pgx.ConnectConfig(fx.ctx, fx.pool.Config().ConnConfig.Copy())
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// laneAudit models the prerequisite lane taking a newer listing pass on the closure.
func (fx *closureFinalFx) laneAudit() uuid.UUID {
	fx.t.Helper()
	row := fx.closure()
	fx.exec("UPDATE recovery_prerequisite_closures SET audit_next_at=now()-interval '1 second' WHERE id=$1", row.ID)
	pass := uuid.New()
	if _, err := fx.q.ReservePrerequisiteAudit(fx.ctx, store.ReservePrerequisiteAuditParams{ID: row.ID, PassID: pgtype.UUID{Bytes: pass, Valid: true}}); err != nil {
		fx.t.Fatal(err)
	}
	return pass
}

// The 500ms bounds are client-side only unless the server is told: a FINAL blocked on the
// closure row must end its own backend, so no lock stays held after the response.
func TestRecoveryClosureFinalLockTimeoutEndsTheBackendLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	closureID := fx.closure().ID
	holder, err := pgx.ConnectConfig(fx.ctx, fx.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	htx, err := holder.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = htx.Rollback(context.Background())
		_ = holder.Close(context.Background())
	})
	fx.svc.finalClosureHook = func(s string) {
		if s == "observed" {
			if _, err := htx.Exec(fx.ctx, "SELECT 1 FROM recovery_prerequisite_closures WHERE id=$1 FOR UPDATE", closureID); err != nil {
				t.Errorf("hold closure row: %v", err)
			}
		}
	}
	obs := fx.observer()
	type result struct {
		err error
		at  time.Time
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		_, err := fx.release(fx.w, fx.request(fx.capture))
		done <- result{err, time.Now()}
	}()
	var pid int32
	deadline := time.Now().Add(5 * time.Second)
	for pid == 0 {
		err := obs.QueryRow(fx.ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database()
 AND wait_event_type='Lock' AND strpos(query,'recovery_prerequisite_closures')>0 LIMIT 1`).Scan(&pid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		if pid == 0 && time.Now().After(deadline) {
			t.Fatal("the FINAL never waited on the closure row")
		}
		time.Sleep(5 * time.Millisecond)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked FINAL did not return")
	}
	if !errors.Is(r.err, ErrPrerequisitePinUnavailable) {
		t.Fatalf("blocked FINAL: %v", r.err)
	}
	if d := r.at.Sub(start); d > 3*time.Second {
		t.Fatalf("blocked FINAL took %v", d)
	}
	time.Sleep(600 * time.Millisecond)
	var waiting, held int
	if err := obs.QueryRow(fx.ctx, `SELECT count(*) FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock'`, pid).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	if err := obs.QueryRow(fx.ctx, `SELECT count(*) FROM pg_locks WHERE pid=$1 AND locktype='transactionid' AND granted`, pid).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if waiting != 0 || held != 0 {
		t.Fatalf("the FINAL's backend %d still waits (%d) or holds row locks (%d) after the response", pid, waiting, held)
	}
	fx.assertOpen()
	if fx.closure().AbsentSince.Valid {
		t.Fatal("a lock timeout became absence")
	}
}

// lockProbeDB records, before every statement of every transaction it opens once armed, the
// lock_timeout the server will apply to it. It proves the bound is set on the server, which no
// client-side deadline can: a closed socket does not always end a backend that is waiting.
type lockProbeDB struct {
	DB
	mu    sync.Mutex
	armed bool
	seen  []string
}

func (d *lockProbeDB) arm() { d.mu.Lock(); d.armed = true; d.mu.Unlock() }

func (d *lockProbeDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &lockProbeTx{Tx: tx, db: d}, nil
}

func (d *lockProbeDB) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	tx, err := d.DB.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &lockProbeTx{Tx: tx, db: d}, nil
}

type lockProbeTx struct {
	pgx.Tx
	db *lockProbeDB
}

func (t *lockProbeTx) probe(ctx context.Context, sql string) {
	t.db.mu.Lock()
	armed := t.db.armed
	t.db.mu.Unlock()
	if !armed || strings.HasPrefix(sql, "SET LOCAL") {
		return
	}
	var v string
	if err := t.Tx.QueryRow(ctx, "SHOW lock_timeout").Scan(&v); err != nil {
		v = "probe failed: " + err.Error()
	}
	t.db.mu.Lock()
	t.db.seen = append(t.db.seen, v)
	t.db.mu.Unlock()
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

func TestRecoveryClosureFinalCommitSetsServerLockTimeoutLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	probe := &lockProbeDB{DB: fx.pool}
	fx.svc.pool = probe
	fx.svc.finalClosureHook = func(s string) {
		if s == "observed" {
			probe.arm() // everything after the listing is the locked commit
		}
	}
	if ack, err := fx.release(fx.w, fx.request(fx.capture)); err != nil || !ack.Released {
		t.Fatalf("release: %+v %v", ack, err)
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	if len(probe.seen) < 6 {
		t.Fatalf("the locked commit ran only %d statements: %v", len(probe.seen), probe.seen)
	}
	for i, v := range probe.seen {
		if v != "500ms" {
			t.Fatalf("statement %d of the locked commit ran with lock_timeout=%q, want 500ms: %v", i, v, probe.seen)
		}
	}
}

// A request too late to fit the listing window must not move the closure's listing fence.
func TestRecoveryClosureFinalTooLateDoesNotReserveLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	before := fx.snapshot("recovery_prerequisite_closures", fx.closure().ID)
	ctx, cancel := context.WithTimeout(fx.ctx, finalClosureListReserve-time.Second)
	defer cancel()
	if _, err := fx.svc.Release(ctx, fx.w, fx.run, fx.request(fx.capture)); !errors.Is(err, ErrPrerequisitePinUnavailable) {
		t.Fatalf("too-late FINAL: %v", err)
	}
	if fx.snapshot("recovery_prerequisite_closures", fx.closure().ID) != before {
		t.Fatal("a too-late FINAL superseded the audit pass")
	}
	if r, o, a, f := fx.port.counts(); r+o+a+f != 0 {
		t.Fatalf("a too-late FINAL reached the port: %d %d %d %d", r, o, a, f)
	}
	fx.assertOpen()
}

// The re-precheck after a reservation miss maps a deadline or cancellation to the retryable
// 503, never a raw error (a 500).
func TestRecoveryClosureFinalPrecheckTimeoutIsUnavailableLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	ctx, cancel := context.WithCancel(fx.ctx)
	defer cancel()
	fx.port.reserveMiss = true
	fx.svc.finalClosureHook = func(s string) {
		if s == "reserved" {
			cancel()
		}
	}
	if _, err := fx.svc.Release(ctx, fx.w, fx.run, fx.request(fx.capture)); !errors.Is(err, ErrPrerequisitePinUnavailable) {
		t.Fatalf("re-precheck under a cancelled request: %v", err)
	}
	fx.assertOpen()
}

// The lane (or another actor) records a verdict between this FINAL's listing and its commit:
// nothing releases, and the typed refusal reports what is now on record.
func TestRecoveryClosureFinalLaneVerdictAfterListingLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	t.Run("first absence recorded", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.svc.finalClosureHook = func(s string) {
			if s == "observed" {
				pass := fx.laneAudit()
				fx.rows(fx.q.RecordPrerequisiteAbsence(fx.ctx, store.RecordPrerequisiteAbsenceParams{
					ID: fx.closure().ID, PassID: pgtype.UUID{Bytes: pass, Valid: true}, Generation: 0}))
			}
		}
		fx.reject(fx.w, fx.request(fx.capture), ErrPrerequisitePinMissing)
		fx.assertOpen()
		if row := fx.closure(); row.State != "pinned" || !row.AbsentSince.Valid {
			t.Fatalf("the lane's absence was not kept: %+v", row)
		}
	})
	t.Run("closure leaves pinned", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.svc.finalClosureHook = func(s string) {
			if s == "observed" {
				pass := fx.laneAudit()
				fx.rows(fx.q.RecordPrerequisitePinnedForeign(fx.ctx, store.RecordPrerequisitePinnedForeignParams{
					ID: fx.closure().ID, PassID: pgtype.UUID{Bytes: pass, Valid: true}, Generation: 0}))
			}
		}
		fx.reject(fx.w, fx.request(fx.capture), ErrManifestConflict)
		fx.assertOpen()
		if row := fx.closure(); row.State != "foreign" {
			t.Fatalf("the closure state was not kept: %+v", row)
		}
	})
}

// Two identical FINALs: the one paused after its listing is answered by the original receipt,
// and the release is written exactly once.
func TestRecoveryClosureFinalIdenticalRequestsReleaseOnceLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	t.Run("paused after the listing", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		var seen atomic.Int32
		paused, resume := make(chan struct{}), make(chan struct{})
		fx.svc.finalClosureHook = func(s string) {
			if s == "observed" && seen.Add(1) == 1 {
				close(paused)
				<-resume
			}
		}
		type result struct {
			ack apitypes.RecoveryReleaseResponse
			err error
		}
		first := make(chan result, 1)
		go func() {
			ack, err := fx.release(fx.w, fx.request(fx.capture))
			first <- result{ack, err}
		}()
		select {
		case <-paused:
		case <-time.After(5 * time.Second):
			t.Fatal("first request never reached its listing")
		}
		ack, err := fx.release(fx.w, fx.request(fx.capture))
		if err != nil || !ack.Released {
			t.Fatalf("second request: %+v %v", ack, err)
		}
		fx.assertReleased()
		written := fx.snapshot("recovery_custody_holds", fx.hold)
		close(resume)
		select {
		case r := <-first:
			if r.err != nil || r.ack != ack {
				t.Fatalf("paused request: %+v %v want %+v", r.ack, r.err, ack)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("paused request never returned")
		}
		if fx.snapshot("recovery_custody_holds", fx.hold) != written {
			t.Fatal("the replayed request rewrote the release")
		}
	})
	t.Run("unsynchronised", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		const n = 4
		errs := make([]error, n)
		acks := make([]bool, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ack, err := fx.release(fx.w, fx.request(fx.capture))
				errs[i], acks[i] = err, ack.Released
			}()
		}
		wg.Wait()
		released := 0
		for i := range errs {
			switch {
			case errs[i] == nil && acks[i]:
				released++
			case errors.Is(errs[i], ErrPrerequisitePinUnavailable):
			default:
				t.Fatalf("request %d: ack=%v err=%v", i, acks[i], errs[i])
			}
		}
		if released == 0 {
			t.Fatal("no request was acknowledged")
		}
		fx.assertReleased()
	})
}

// A cancelled request returns promptly, leaves no transaction open and changes the closure only
// by its own listing reservation.
func TestRecoveryClosureFinalCancelledRequestLeavesNothingOpenLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	idleInTx := func(fx *closureFinalFx, obs *pgx.Conn) int {
		var n int
		if err := obs.QueryRow(fx.ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state LIKE 'idle in transaction%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	check := func(fx *closureFinalFx, obs *pgx.Conn, before store.RecoveryPrerequisiteClosure) {
		t.Helper()
		fx.assertOpen()
		after := fx.closure()
		_, _, _, _ = fx.port.counts()
		fx.port.mu.Lock()
		passes := append([]uuid.UUID(nil), fx.port.passes...)
		fx.port.mu.Unlock()
		if len(passes) != 1 || after.LastAuditPassID != (pgtype.UUID{Bytes: passes[0], Valid: true}) ||
			after.State != "pinned" || after.AbsentSince.Valid || after.CleanupAttempts != before.CleanupAttempts ||
			!after.AuditNextAt.Time.Equal(before.AuditNextAt.Time) {
			t.Fatalf("a cancelled request changed more than its own reservation: %+v -> %+v (passes %v)", before, after, passes)
		}
		if n := idleInTx(fx, obs); n != 0 {
			t.Fatalf("%d transactions left idle in transaction", n)
		}
	}
	t.Run("paused after the listing", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		obs := fx.observer()
		before := fx.closure()
		ctx, cancel := context.WithCancel(fx.ctx)
		defer cancel()
		paused, resume := make(chan struct{}), make(chan struct{})
		fx.svc.finalClosureHook = func(s string) {
			if s == "observed" {
				close(paused)
				<-resume
			}
		}
		done := make(chan error, 1)
		go func() {
			_, err := fx.svc.Release(ctx, fx.w, fx.run, fx.request(fx.capture))
			done <- err
		}()
		<-paused
		cancel()
		cancelled := time.Now()
		close(resume)
		select {
		case err := <-done:
			if !errors.Is(err, ErrPrerequisitePinUnavailable) {
				t.Fatalf("cancelled request: %v", err)
			}
			if d := time.Since(cancelled); d > time.Second {
				t.Fatalf("cancelled request took %v", d)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled request never returned")
		}
		check(fx, obs, before)
	})
	t.Run("blocked in the listing", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		obs := fx.observer()
		before := fx.closure()
		fx.port.block = true
		ctx, cancel := context.WithCancel(fx.ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := fx.svc.Release(ctx, fx.w, fx.run, fx.request(fx.capture))
			done <- err
		}()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, o, _, _ := fx.port.counts(); o == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("request never reached its listing")
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		cancelled := time.Now()
		select {
		case err := <-done:
			if !errors.Is(err, ErrPrerequisitePinUnavailable) {
				t.Fatalf("cancelled request: %v", err)
			}
			if d := time.Since(cancelled); d > time.Second {
				t.Fatalf("cancelled request took %v", d)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelled request never returned")
		}
		check(fx, obs, before)
	})
}

// A closure-backed release can only name the pinned closure of its own capture: the composite
// foreign key and the hold guard refuse every other identity, and the closure cannot be deleted
// while a release names it. The exact-observation query's closure EXISTS therefore has no
// stageable negative; this proves the schema, not the query, closes that door.
func TestRecoveryClosureFinalReleaseIdentityIsSchemaEnforcedLiveDB(t *testing.T) {
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	// A second thin capture on the same hold, with its own (pending) closure.
	second := fx.reserve("thin-final-sibling")
	body, m := closureManifest()
	if _, err := fx.svc.Upload(fx.ctx, fx.w, fx.run, second, m, strings.NewReader(string(body))); err != nil {
		t.Fatal(err)
	}
	sibling, err := fx.q.GetPrerequisiteClosureForCapture(fx.ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	fx.exec("UPDATE runs SET status='completed' WHERE id=$1", fx.run)
	// The production query refuses the sibling's closure for this capture.
	n, err := fx.q.ReleaseFinalClosureHold(fx.ctx, store.ReleaseFinalClosureHoldParams{
		ID: fx.hold, RunID: fx.run, UserID: fx.w.UserID, Generation: fx.gen, WorkerID: fx.w.ID,
		FinalCaptureID: fx.capture, FinalSourceSha: "aaaa1111", FinalCoverageDigest: strings.Repeat("a", 64), ClosureID: sibling.ID})
	if err != nil || n != 0 {
		t.Fatalf("query accepted a foreign closure: %d %v", n, err)
	}
	// So does a raw write that skips the query's predicates: the guard or the foreign key refuses it.
	tag, err := fx.pool.Exec(fx.ctx, `UPDATE recovery_custody_holds SET state='released', live_worker_id=NULL, live_run_id=NULL,
 final_disposition='archive', final_capture_id=$2, final_source_sha='aaaa1111', final_coverage_digest=$3,
 release_evidence='prerequisite_closure', final_closure_id=$4, released_at=now() WHERE id=$1`,
		fx.hold, fx.capture, strings.Repeat("a", 64), sibling.ID)
	var pgErr *pgconn.PgError
	fkRefused := err != nil && errors.As(err, &pgErr) && pgErr.Code == "23503"
	if err == nil && tag.RowsAffected() != 0 || err != nil && !fkRefused {
		t.Fatalf("a mismatching closure identity was staged: rows=%d err=%v", tag.RowsAffected(), err)
	}
	fx.assertOpen()
	// A genuine release is observed as the closed archive, and its closure cannot be deleted.
	ack, err := fx.release(fx.w, fx.request(fx.capture))
	if err != nil || !ack.Released {
		t.Fatalf("release: %+v %v", ack, err)
	}
	got, err := fx.svc.ObserveExactHold(fx.ctx, fx.w, fx.run, fx.gen)
	if err != nil || got == nil || got.ReleaseEvidence != "prerequisite_closure" {
		t.Fatalf("genuine release not observed: %+v %v", got, err)
	}
	_, err = fx.pool.Exec(fx.ctx, "DELETE FROM recovery_prerequisite_closures WHERE id=$1", fx.closure().ID)
	// 55000 is the closure table's own delete guard; 23503 is the foreign key's RESTRICT behind it.
	if !errors.As(err, &pgErr) || (pgErr.Code != "23503" && pgErr.Code != "55000") {
		t.Fatalf("a named closure was deletable: %v", err)
	}
}
