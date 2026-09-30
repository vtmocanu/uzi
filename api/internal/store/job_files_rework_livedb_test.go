package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M1 rework, at the store layer: the display_name CHECKs reject Unicode format, line and
// paragraph separator characters, and the expiry and reclaim statements re-check a file's state on
// the UPDATE itself so a concurrent state change (M2's attach) is never expired. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// TestJobFileDisplayNameFormatCharsLiveDB: both display_name CHECKs (job_files and
// job_output_refusals, migration 00276) refuse bidi overrides and isolates, zero-width characters,
// the BOM and the line/paragraph separators, and still accept ordinary non-ASCII names.
//
// CALIBRATION: remove the last AND clause of either display_name CHECK in 00276; that table's
// subtests then insert the name and go red.
func TestJobFileDisplayNameFormatCharsLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	run := fx.queuedNewJob()
	sha := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	insertFile := func(name string) error {
		_, err := fx.pool.Exec(fx.ctx,
			`INSERT INTO job_files (user_id, direction, display_name, content_type, byte_size, sha256, storage_name, state)
			 VALUES ($1, 'input', $2, 'text/plain', 3, $3, $4, 'unattached')`, fx.userID, name, sha, sha+".txt")
		return err
	}
	insertRefusal := func(name string) error {
		_, err := fx.pool.Exec(fx.ctx,
			`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason) VALUES ($1, $2, 3, 'x')`, run, name)
		return err
	}
	for table, insert := range map[string]func(string) error{"job_files": insertFile, "job_output_refusals": insertRefusal} {
		if err := insert("résumé 報告.txt"); err != nil {
			t.Fatalf("%s: an ordinary non-ASCII name must insert: %v", table, err)
		}
		for name, bad := range map[string]string{
			"RLO U+202E":            "gnp\u202Eexe.txt",
			"LRI U+2066":            "a\u2066b.txt",
			"PDI U+2069":            "a\u2069b.txt",
			"line separator U+2028": "a\u2028b.txt",
			"para separator U+2029": "a\u2029b.txt",
			"zero-width U+200B":     "a\u200Bb.txt",
			"LRM U+200E":            "a\u200Eb.txt",
			"word joiner U+2060":    "a\u2060b.txt",
			"BOM U+FEFF":            "\uFEFFa.txt",
		} {
			if err := insert(bad); err == nil {
				t.Errorf("%s: %s accepted, want a CHECK violation", table, name)
			}
		}
	}
}

// seedExpiredAvailable inserts an 'available' file past its expiry with one chunk.
func seedExpiredAvailable(fx *fleetFixture, t *testing.T) uuid.UUID {
	t.Helper()
	sha := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	var id uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx,
		`INSERT INTO job_files (user_id, direction, display_name, content_type, byte_size, sha256, storage_name, state, chunk_count, expires_at)
		 VALUES ($1, 'input', 'v.txt', 'text/plain', 3, $2, $3, 'available', 1, now() - interval '1 minute') RETURNING id`,
		fx.userID, sha, sha+".txt").Scan(&id); err != nil {
		t.Fatal(err)
	}
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO job_file_chunks (file_id, chunk_index, length, sealed) VALUES ($1, 0, 3, '\x00')`, id)
	return id
}

// raceAttach runs stmt while another transaction holds an uncommitted attach of the file (state
// 'attached', the change M2's attach makes), then commits the attach. It returns what stmt saw.
func raceAttach(fx *fleetFixture, t *testing.T, file, run uuid.UUID, stmt func() (int64, error)) int64 {
	t.Helper()
	holder, err := fx.pool.Begin(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(fx.ctx) }()
	if _, err := holder.Exec(fx.ctx, `UPDATE job_files SET state = 'attached', run_id = $2, expires_at = NULL WHERE id = $1`, file, run); err != nil {
		t.Fatal(err)
	}
	type result struct {
		n   int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := stmt()
		done <- result{n, err}
	}()
	// The statement's UPDATE now waits on the holder's row lock (it selected the file from its own
	// snapshot, where the file was still available and expired).
	select {
	case r := <-done:
		t.Fatalf("the statement finished (%d, %v) while another transaction held the file's row", r.n, r.err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := holder.Commit(fx.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.n
	case <-time.After(10 * time.Second):
		t.Fatal("the statement never finished after the attach committed")
	}
	return 0
}

func assertAttachedWithChunk(fx *fleetFixture, t *testing.T, id uuid.UUID) {
	t.Helper()
	var state string
	var chunks int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT state, (SELECT count(*) FROM job_file_chunks WHERE file_id = $1) FROM job_files WHERE id = $1`, id).Scan(&state, &chunks); err != nil {
		t.Fatal(err)
	}
	if state != "attached" || chunks != 1 {
		t.Fatalf("file after the concurrent attach = %q with %d chunks, want attached with its 1 chunk", state, chunks)
	}
}

// TestExpireJobFilesSkipsConcurrentlyAttachedLiveDB: a file attached between ExpireJobFiles'
// snapshot and its UPDATE is neither expired nor stripped of its chunks.
//
// CALIBRATION: drop the state/expiry predicate from the UPDATE in ExpireJobFiles (select by id
// from a snapshot CTE instead); the file is then expired and its chunk deleted, and this fails.
func TestExpireJobFilesSkipsConcurrentlyAttachedLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	mustExec(fx.ctx, t, fx.pool, `DELETE FROM job_files`)
	run := fx.queuedNewJob()
	id := seedExpiredAvailable(fx, t)
	q := store.New(fx.pool)
	n := raceAttach(fx, t, id, run, func() (int64, error) { return q.ExpireJobFiles(fx.ctx, pgconv.Time(time.Now())) })
	if n != 0 {
		t.Fatalf("ExpireJobFiles reported %d expired, want 0 (the only candidate was attached)", n)
	}
	assertAttachedWithChunk(fx, t, id)
}

// TestReclaimJobFilesSkipsConcurrentlyAttachedLiveDB: the same for the recovery reclaim.
//
// CALIBRATION: drop the state predicate from the UPDATE in ReclaimJobFilesForRecovery.
func TestReclaimJobFilesSkipsConcurrentlyAttachedLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	mustExec(fx.ctx, t, fx.pool, `DELETE FROM job_files`)
	run := fx.queuedNewJob()
	id := seedExpiredAvailable(fx, t)
	q := store.New(fx.pool)
	n := raceAttach(fx, t, id, run, func() (int64, error) {
		return q.ReclaimJobFilesForRecovery(fx.ctx, store.ReclaimJobFilesForRecoveryParams{Now: pgconv.Time(time.Now()), Need: 1000})
	})
	if n != 0 {
		t.Fatalf("ReclaimJobFilesForRecovery freed %d bytes, want 0 (the only candidate was attached)", n)
	}
	assertAttachedWithChunk(fx, t, id)
}
