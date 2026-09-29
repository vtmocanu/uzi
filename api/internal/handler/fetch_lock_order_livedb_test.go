package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/fetchctl"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestFetchCompleteVsReclaimMintLockOrderLiveDB pins the lock order fetchctl documents
// (runs -> run_fetch_credentials -> run_fetch_reservations) on the one pair that used to
// invert it: a re-claim's mint holds the run's credential row and then releases the prior
// claim's open reservations, while a Complete for one of those reservations arrives in
// between. With Complete locking the reservation first and the credential only when it
// reconciles the counters, each transaction waited on the other and Postgres aborted one
// (SQLSTATE 40P01: the claim or the log write failed). With the credential locked first,
// Complete waits holding nothing, the mint finishes, and Complete then finds the rotated
// credential gone (403, as for any old-claim credential), the counters consistent.
func TestFetchCompleteVsReclaimMintLockOrderLiveDB(t *testing.T) {
	e := newFCEnv(t, nil, true)
	run, cred := e.boundRun("running", 1, 1)
	adm := e.mustBegin(cred)

	// The re-claim: the run moves to generation 2 (still running) and the claim's mint runs
	// in its own transaction, which now holds the credential row.
	e.exec(`UPDATE runs SET claim_generation = 2 WHERE id = $1`, run)
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	qa := store.New(tx)
	_, newHash, err := fetchctl.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := qa.MintRunFetchCredential(e.ctx, store.MintRunFetchCredentialParams{RunID: run, ClaimGeneration: 2, TokenHash: newHash}); err != nil || n != 1 {
		t.Fatalf("mint = %d, %v", n, err)
	}

	// Complete for the generation-1 reservation, concurrently.
	done := make(chan int, 1)
	go func() { done <- e.complete(completeReq(cred, adm.ReservationID)).Code }()

	// Wait until Complete is blocked on a row lock the mint holds (whichever row it tried
	// first). The probe counts only a backend running one of Complete's lock queries and
	// blocked by the mint's own backend, so a lock waiter elsewhere in the database cannot
	// satisfy it.
	e.waitForLockWaiter(backendPID(t, e, tx), []string{"LockFetchCredentialRunByHash", "LockFetchReservation"}, func() string {
		select {
		case code := <-done:
			return fmt.Sprintf("Complete returned %d without waiting on the mint's credential lock", code)
		default:
			return ""
		}
	})

	// The mint's second step takes the reservations. Under the old order Complete held the
	// reservation here, and one of the two was aborted as a deadlock.
	released, err := qa.ReleasePriorGenerationFetchReservations(e.ctx, store.ReleasePriorGenerationFetchReservationsParams{RunID: run, ClaimGeneration: 2})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		t.Fatalf("the re-claim's mint deadlocked against Complete: %v", err)
	}
	if err != nil {
		t.Fatalf("release prior-generation reservations: %v", err)
	}
	if released != 1 {
		t.Fatalf("released %d reservations, want 1 (Complete must not have settled it first)", released)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatalf("commit the mint: %v", err)
	}

	select {
	case code := <-done:
		if code != http.StatusForbidden {
			t.Fatalf("Complete after the rotation = %d, want 403 (the old credential no longer exists; 500 is the deadlock)", code)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Complete never returned")
	}
	if c := e.counters(run); c.inflight != 0 || c.reserved != 0 {
		t.Fatalf("counters after the release = %+v, want nothing in flight or reserved", c)
	}
	if n := e.count(`SELECT count(*) FROM run_fetches WHERE run_id = $1`, run); n != 0 {
		t.Fatalf("%d attempts logged under a rotated credential, want 0", n)
	}
}

// TestFetchSweepVsCompleteLockOrderLiveDB pins the same lock order on the stale sweep: a
// Complete for a stale reservation holds the run's credential (its first lock) when the
// sweep starts, and takes the reservation only after the sweep has made its first move.
// With the sweep locking the credentials first (LockCredentialsWithStaleFetchReservations,
// FOR UPDATE OF c), the sweep waits holding nothing, Complete settles and logs the attempt,
// and the sweep then finds nothing of this run's left to release. Without that lock the
// sweep's release takes the reservation and then waits on the credential, Complete waits on
// the reservation, and Postgres aborts one of the two (SQLSTATE 40P01).
func TestFetchSweepVsCompleteLockOrderLiveDB(t *testing.T) {
	e := newFCEnv(t, fcCaps{settings.KeyFetchMaxFileBytes: "1000"}, true)
	run, cred := e.boundRun("running", 1, 1)
	adm := e.mustBegin(cred)
	e.exec(`UPDATE run_fetch_reservations SET created_at = now() - interval '1 hour' WHERE id = $1`, adm.ReservationID)
	resID, err := uuid.Parse(adm.ReservationID)
	if err != nil {
		t.Fatal(err)
	}

	// Complete's first step, in its own transaction, in fetchctl.Complete's order: the
	// credential row, locked.
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	qc := store.New(tx)
	lk, err := qc.LockFetchCredentialRunByHash(e.ctx, fetchctl.HashCredential(cred))
	if err != nil {
		t.Fatalf("lock the credential: %v", err)
	}

	// The sweep, concurrently.
	type sweepResult struct {
		n   int64
		err error
	}
	swept := make(chan sweepResult, 1)
	go func() {
		n, err := fetchctl.New(e.pool, e.caps).SweepStale(e.ctx)
		swept <- sweepResult{n, err}
	}()
	e.waitForLockWaiter(backendPID(t, e, tx), []string{"LockCredentialsWithStaleFetchReservations", "ReleaseStaleFetchReservations"}, func() string {
		select {
		case r := <-swept:
			return fmt.Sprintf("the sweep returned (%d, %v) without waiting on Complete's credential lock", r.n, r.err)
		default:
			return ""
		}
	})

	// Complete's second step: the reservation. Had the sweep taken it before the credential,
	// this waits on the sweep while the sweep waits on the credential this transaction holds.
	res, err := qc.LockFetchReservation(e.ctx, store.LockFetchReservationParams{ID: resID, RunID: lk.RunID})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
		t.Fatalf("Complete deadlocked against the stale sweep: %v", err)
	}
	if err != nil {
		t.Fatalf("lock the reservation: %v", err)
	}
	if res.Settled {
		t.Fatal("the sweep settled the reservation while Complete held the run's credential")
	}
	if err := qc.SettleFetchReservation(e.ctx, res.ID); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if err := qc.ReconcileFetchCounters(e.ctx, store.ReconcileFetchCountersParams{
		RunID: lk.RunID, ReleaseBytes: res.Bytes, ReleaseInflight: 1, UsedBytes: 100,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ts := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	if err := qc.InsertRunFetch(e.ctx, store.InsertRunFetchParams{
		RunID: lk.RunID, ReservationID: res.ID, Url: "https://docs.example.com/a", FinalUrl: "https://docs.example.com/a",
		Verdict: fetchctl.VerdictAllowed, HttpStatus: 200, ContentType: "text/html", Bytes: 100,
		Sha256: strings.Repeat("ab", 32), StartedAt: ts, FinishedAt: ts,
	}); err != nil {
		t.Fatalf("log the attempt: %v", err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatalf("commit Complete: %v", err)
	}

	select {
	case r := <-swept:
		if errors.As(r.err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatalf("the stale sweep deadlocked against Complete: %v", r.err)
		}
		if r.err != nil {
			t.Fatalf("SweepStale: %v", r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the sweep never returned")
	}
	if c := e.counters(run); c != (fcCounters{used: 100, files: 1, attempts: 1}) {
		t.Fatalf("counters = %+v, want Complete's settlement only (nothing released twice)", c)
	}
	if n := e.count(`SELECT count(*) FROM run_fetches WHERE reservation_id = $1`, res.ID); n != 1 {
		t.Fatalf("%d attempts logged for the reservation, want 1", n)
	}
}

// backendPID is the Postgres backend of tx's connection.
func backendPID(t *testing.T, e *fcEnv, tx pgx.Tx) int32 {
	t.Helper()
	var pid int32
	if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("pg_backend_pid: %v", err)
	}
	return pid
}

// waitForLockWaiter waits until a backend running one of the named sqlc queries is waiting
// on a lock held by the blocker backend. Matching both the blocker (pg_blocking_pids) and the
// query keeps a lock waiter from another test or session out of the probe. returned reports
// a non-empty failure when the side expected to wait has already finished.
func (e *fcEnv) waitForLockWaiter(blocker int32, queries []string, returned func() string) {
	e.t.Helper()
	patterns := make([]string, len(queries))
	for i, q := range queries {
		patterns[i] = "-- name: " + q + " %"
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := e.pool.QueryRow(e.ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'
			   AND $1::int = ANY(pg_blocking_pids(pid))
			   AND query LIKE ANY($2::text[])`, blocker, patterns).Scan(&waiting); err != nil {
			e.t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		if msg := returned(); msg != "" {
			e.t.Fatal(msg)
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no %v backend ever blocked on backend %d", queries, blocker)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
