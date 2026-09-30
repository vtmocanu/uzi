package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M1 at the store layer: the stored-files admission lock and the job_files schema's own
// CHECK constraints. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// TestLockStoredFilesSerializesAcrossOwnersLiveDB: two admissions for DIFFERENT owners still
// serialize, because both take the shared-budget key after their owner key. That is what makes the
// instance and shared-budget checks exact across owners. A second transaction for the SAME owner
// blocks on the owner key, and a transaction re-taking the locks it holds does not deadlock.
//
// CALIBRATION: delete the shared-key Exec from store.LockStoredFiles; the different-owner
// transaction then acquires immediately and the first check fails.
func TestLockStoredFilesSerializesAcrossOwnersLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	ownerA, ownerB := uuid.New(), uuid.New()

	holder, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(fx.ctx) }()
	if err := store.LockStoredFiles(fx.ctx, holder, ownerA); err != nil {
		t.Fatal(err)
	}
	// Re-taking held keys is a no-op, not a self-deadlock.
	if err := store.LockStoredFiles(fx.ctx, holder, ownerA); err != nil {
		t.Fatalf("re-taking the locks in the same transaction: %v", err)
	}

	for name, owner := range map[string]uuid.UUID{"different owner": ownerB, "same owner": ownerA} {
		acquired := make(chan error, 1)
		go func() {
			tx, err := fx.pool.Begin(fx.ctx)
			if err != nil {
				acquired <- err
				return
			}
			defer func() { _ = tx.Rollback(fx.ctx) }()
			acquired <- store.LockStoredFiles(fx.ctx, tx, owner)
		}()
		select {
		case err := <-acquired:
			t.Fatalf("%s: acquired the stored-files locks (%v) while another transaction held them", name, err)
		case <-time.After(300 * time.Millisecond):
		}
		// Release the holder for this round, then re-take it for the next.
		if err := holder.Rollback(fx.ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-acquired:
			if err != nil {
				t.Fatalf("%s: after the holder released: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: still blocked after the holder released", name)
		}
		if holder, err = fx.pool.Begin(fx.ctx); err != nil {
			t.Fatal(err)
		}
		if err := store.LockStoredFiles(fx.ctx, holder, ownerA); err != nil {
			t.Fatal(err)
		}
	}
}

// TestJobFilesSchemaChecksLiveDB: the DB backstops behind the service's validation: a committed
// row needs its content identity, a display name is path-free and control-free, the type is on the
// allowlist, an output belongs to a run, and deleting the file cascades its chunks.
func TestJobFilesSchemaChecksLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	sha := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	insert := func(state, dir, name string, contentType, sha256, storage any, run any) error {
		_, err := fx.pool.Exec(fx.ctx,
			`INSERT INTO job_files (user_id, direction, display_name, content_type, byte_size, sha256, storage_name, state, run_id)
			 VALUES ($1, $2, $3, $4, 3, $5, $6, $7, $8)`, fx.userID, dir, name, contentType, sha256, storage, state, run)
		return err
	}
	if err := insert("unattached", "input", "ok.txt", "text/plain", sha, sha+".txt", nil); err != nil {
		t.Fatalf("a well-formed committed input must insert: %v", err)
	}
	if err := insert("reserved", "input", "r.txt", nil, nil, nil, nil); err != nil {
		t.Fatalf("a reservation has no identity yet and must insert: %v", err)
	}
	for name, err := range map[string]error{
		"committed without identity": insert("unattached", "input", "a.txt", nil, nil, nil, nil),
		"path separator in name":     insert("unattached", "input", "a/b.txt", "text/plain", sha, sha+".txt", nil),
		"backslash in name":          insert("unattached", "input", `a\b.txt`, "text/plain", sha, sha+".txt", nil),
		"control character in name":  insert("unattached", "input", "a\tb.txt", "text/plain", sha, sha+".txt", nil),
		"type off the allowlist":     insert("unattached", "input", "a.exe", "application/x-msdownload", sha, sha+".txt", nil),
		"storage name not by hash":   insert("unattached", "input", "a.txt", "text/plain", sha, "report.txt", nil),
		"output without a run":       insert("attached", "output", "o.txt", "text/plain", sha, sha+".txt", nil),
		"unknown state":              insert("lost", "input", "a.txt", "text/plain", sha, sha+".txt", nil),
	} {
		if err == nil {
			t.Errorf("%s: the insert succeeded, want a CHECK violation", name)
		}
	}

	var id uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx,
		`INSERT INTO job_files (user_id, direction, display_name, content_type, byte_size, sha256, storage_name, state)
		 VALUES ($1, 'input', 'c.txt', 'text/plain', 3, $2, $3, 'unattached') RETURNING id`, fx.userID, sha, sha+".txt").Scan(&id); err != nil {
		t.Fatal(err)
	}
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO job_file_chunks (file_id, chunk_index, length, sealed) VALUES ($1, 0, 3, '\x00')`, id)
	mustExec(fx.ctx, t, fx.pool, `DELETE FROM job_files WHERE id = $1`, id)
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM job_file_chunks WHERE file_id = $1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("chunks after deleting the file = %d (err %v), want 0 (cascade)", n, err)
	}
}
