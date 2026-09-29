package handler

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/fetchctl"
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

	// Wait until Complete is blocked on a row lock (whichever row it tried first).
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := e.pool.QueryRow(e.ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("Complete returned %d without waiting on the mint's credential lock", code)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Complete never blocked on a lock")
		}
		time.Sleep(20 * time.Millisecond)
	}

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
