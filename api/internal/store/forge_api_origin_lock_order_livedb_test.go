package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// ClaimRun's forge_locked CTE locks a target's cooldown rows ORDER BY origin, and for GitHub the API
// origin sorts before the Git origin. The run-lifecycle trigger releases both of a claim's
// reservations, so it must take the same order. Interleaving: a claimant holds the API row (its first
// lock) while a release of the just-claimed run starts, then the claimant asks for the Git row. A
// trigger that updates the Git row first holds it while waiting for the API row, and the two sessions
// deadlock (SQLSTATE 40P01); a trigger that locks in origin order simply waits and both commit.
func TestForgeAPIOriginReleaseLockOrderLiveDB(t *testing.T) {
	fx := newForgeAPIFixture(t, "github")
	w := fx.worker("claimant", 0, false)
	id := fx.unclaimedRun()
	apiFxRow(fx, w, apiFxGit, true, true, false)
	apiFxRow(fx, w, apiFxAPI, true, false, true)
	if got, err := forgeAdmissionClaim(fx, fx.q, w); err != nil || got.ID != id {
		t.Fatalf("claim: %s %v", got.ID, err)
	}
	for _, origin := range []string{apiFxGit, apiFxAPI} {
		if st := apiFxRead(fx, w, origin); !st.Reserved || st.RunID != id {
			t.Fatalf("no reservation on %s after claim: %+v", origin, st)
		}
	}

	ctx, cancel := context.WithTimeout(fx.ctx, 30*time.Second)
	defer cancel()
	connA, err := fx.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connA.Release()
	connB, err := fx.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer connB.Release()
	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(context.Background()) }()
	txB, err := connB.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txB.Rollback(context.Background()) }()

	// A holds the API row, the first row of its ordered lock.
	var one int
	if err := txA.QueryRow(ctx, `SELECT 1 FROM forge_origin_cooldowns
		WHERE user_id=$1 AND worker_id=$2 AND origin=$3 FOR UPDATE`, fx.userID, w, apiFxAPI).Scan(&one); err != nil {
		t.Fatalf("A locks the API row: %v", err)
	}

	// B releases the claim; the trigger now needs both rows.
	releaseDone := make(chan error, 1)
	go func() {
		_, err := txB.Exec(ctx, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
		releaseDone <- err
	}()
	pidB := int32(connB.Conn().PgConn().PID())
	for {
		var waiting bool
		if err := fx.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE pid=$1 AND wait_event_type='Lock')`, pidB).Scan(&waiting); err != nil {
			t.Fatalf("poll B: %v", err)
		}
		if waiting {
			break
		}
		select {
		case err := <-releaseDone:
			t.Fatalf("release finished while A held the API row: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}

	// A completes its ordered lock, as forge_locked does: API row again, then the Git row.
	aErr := func() error {
		rows, err := txA.Query(ctx, `SELECT c.origin FROM forge_origin_cooldowns c
			WHERE c.user_id=$1 AND c.worker_id=$2 AND c.origin IN ($3,$4)
			ORDER BY c.origin FOR UPDATE OF c`, fx.userID, w, apiFxGit, apiFxAPI)
		if err != nil {
			return err
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return txA.Commit(ctx)
	}()
	var pgErr *pgconn.PgError
	if errors.As(aErr, &pgErr) && pgErr.Code == "40P01" {
		t.Fatalf("claimant deadlocked with the release trigger: %v", aErr)
	}
	if aErr != nil {
		t.Fatalf("claimant: %v", aErr)
	}
	select {
	case err := <-releaseDone:
		if err != nil {
			if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
				t.Fatalf("release deadlocked with the claimant: %v", err)
			}
			t.Fatalf("release: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("release never finished after the claimant committed")
	}
	if err := txB.Commit(ctx); err != nil {
		t.Fatalf("commit release: %v", err)
	}
	for _, origin := range []string{apiFxGit, apiFxAPI} {
		if st := apiFxRead(fx, w, origin); !st.Exists || st.Reserved {
			t.Fatalf("reservation on %s survived the release: %+v", origin, st)
		}
	}
}
