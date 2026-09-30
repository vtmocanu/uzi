package recovery

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1909 M1 rework, recovery side: admission destroys nothing (job files are reclaimed only
// after the upload's size and checksum verified, in the stream transaction), a verified upload
// reclaims exactly the excess, and an owner's discard or expiry is never undone by a retry.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway database.

// availableJobFile stores an n-byte file and rests it 'available' for a day, created hoursAgo hours
// ago, so the reclaim order among several is deterministic.
func (e *sfEnv) availableJobFile(t *testing.T, jf *workersvc.JobFiles, name string, n, hoursAgo int) uuid.UUID {
	t.Helper()
	id := e.jobFile(t, jf, name, n, nil)
	e.exec(`UPDATE job_files SET state = 'available', expires_at = now() + interval '1 day', created_at = now() - make_interval(hours => $2) WHERE id = $1`, id, hoursAgo)
	return id
}

func (e *sfEnv) chunks(t *testing.T, table, col string, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM `+table+` WHERE `+col+` = $1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRecoveryAdmissionDestroysNothingBeforeBytesLiveDB: a worker that declares a size and then
// sends no bytes, or bytes that fail the checksum, must not expire anyone's job files. Before the
// fix admission reclaimed on the DECLARED size and committed before reading a byte, so this repeated
// indefinitely.
//
// CALIBRATION: call qtx.ReclaimJobFilesForRecovery in admit again (Need = shared + declared -
// budget); the victim is then expired by the first attempt and this test goes red.
func TestRecoveryAdmissionDestroysNothingBeforeBytesLiveDB(t *testing.T) {
	e := newSFEnv(t)
	svc, jf := e.recovery(1000), e.jobFiles()
	victim := e.availableJobFile(t, jf, "victim.txt", 900, 5)

	declared := bytes.Repeat([]byte("a"), 900)
	for name, body := range map[string][]byte{
		"empty body":     nil,
		"garbage bytes":  bytes.Repeat([]byte("z"), 900),
		"truncated body": declared[:10],
	} {
		id := e.newCapture(t, svc)
		if _, err := svc.Upload(e.ctx, e.wkr, e.runID, id, manifestOf(declared), bytes.NewReader(body)); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("%s: upload = %v, want ErrIntegrity", name, err)
		}
		if got := e.jobState(t, victim); got != "available" {
			t.Fatalf("%s: the victim job file is %q after an upload that never verified, want available", name, got)
		}
		if n := e.chunks(t, "job_file_chunks", "file_id", victim); n != 1 {
			t.Fatalf("%s: the victim holds %d chunks, want its 1", name, n)
		}
		if st, _ := e.captureState(t, id); st != "needs_action" {
			t.Fatalf("%s: capture state = %q, want needs_action", name, st)
		}
	}
}

// TestRecoveryVerifiedUploadReclaimsExactExcessLiveDB: a verified upload that does not fit the
// shared budget reclaims exactly the excess (oldest available first) in the stream transaction, and
// leaves the rest.
//
// CALIBRATION: pass Need = the whole declared size instead of the excess in reclaimForBudget; the
// newer file is then expired too and this test goes red.
func TestRecoveryVerifiedUploadReclaimsExactExcessLiveDB(t *testing.T) {
	e := newSFEnv(t)
	svc, jf := e.recovery(1000), e.jobFiles()
	var files []uuid.UUID
	for i, name := range []string{"f1.txt", "f2.txt", "f3.txt", "f4.txt"} {
		files = append(files, e.availableJobFile(t, jf, name, 200, 10-i)) // f1 is the oldest.
	}

	// 800 stored + 800 = 1600 of 1000: the excess is 600, which the three oldest files cover.
	body := bytes.Repeat([]byte("r"), 800)
	id := e.newCapture(t, svc)
	if _, err := svc.Upload(e.ctx, e.wkr, e.runID, id, manifestOf(body), bytes.NewReader(body)); err != nil {
		t.Fatalf("verified upload needing a reclaim: %v", err)
	}
	if st, _ := e.captureState(t, id); st != "available" {
		t.Fatalf("capture state = %q, want available", st)
	}
	for i, f := range files {
		want, chunks := "expired", 0
		if i == 3 {
			want, chunks = "available", 1 // the newest: the excess was already covered.
		}
		if got := e.jobState(t, f); got != want {
			t.Errorf("file %d = %q, want %q", i+1, got, want)
		}
		if n := e.chunks(t, "job_file_chunks", "file_id", f); n != chunks {
			t.Errorf("file %d holds %d chunks, want %d", i+1, n, chunks)
		}
	}
}

// TestRecoveryStreamQuotaAtBindRollsBackLiveDB: admission may pass on reclaimable bytes that are
// gone by the time the upload verifies (another admission took them). The stream then cannot bring
// the total within budget: the capture fails ErrQuota with no bind, no chunks, and no reclaim.
//
// CALIBRATION: drop the re-sum check at the end of reclaimForBudget; the upload then binds over the
// budget and this test goes red.
func TestRecoveryStreamQuotaAtBindRollsBackLiveDB(t *testing.T) {
	e := newSFEnv(t)
	svc, jf := e.recovery(1000), e.jobFiles()
	victim := e.availableJobFile(t, jf, "victim.txt", 700, 5)

	body := bytes.Repeat([]byte("r"), 500)
	id := e.newCapture(t, svc)
	if _, done, err := svc.admit(e.ctx, e.wkr, e.runID, id, manifestOf(body)); err != nil || done {
		t.Fatalf("admit (700 stored + 500 declared, 700 reclaimable) = done %v, %v; want admitted", done, err)
	}
	// Meanwhile another upload's bytes land inside their TTL: not reclaimable.
	e.jobFile(t, jf, "fresh.txt", 700, nil)

	if _, err := svc.stream(e.ctx, e.wkr, e.runID, id, manifestOf(body), bytes.NewReader(body)); !errors.Is(err, ErrQuota) {
		t.Fatalf("stream that cannot fit even after reclaiming = %v, want ErrQuota", err)
	}
	if got := e.jobState(t, victim); got != "available" {
		t.Fatalf("victim = %q, want available (the reclaim rolls back with the failed stream)", got)
	}
	if n := e.chunks(t, "recovery_capture_chunks", "capture_id", id); n != 0 {
		t.Fatalf("the failed stream left %d chunks", n)
	}
	var bound bool
	if err := e.pool.QueryRow(e.ctx, `SELECT manifest_bound FROM recovery_captures WHERE id = $1`, id).Scan(&bound); err != nil || bound {
		t.Fatalf("manifest_bound = %v (err %v), want false", bound, err)
	}
}

// TestRecoveryDiscardedCaptureStaysDiscardedLiveDB: a capture the owner discarded (or that
// retention expired) is not uploadable, and a failed upload attempt never flips it to needs_action,
// where a retry would revive it to available.
//
// CALIBRATION: remove ErrNotAvailable from the skip list in recordFailure and the discarded/expired
// case in its state switch; the second upload then finds needs_action and revives the capture.
func TestRecoveryDiscardedCaptureStaysDiscardedLiveDB(t *testing.T) {
	e := newSFEnv(t)
	svc := e.recovery(1000)
	body := bytes.Repeat([]byte("r"), 100)
	for _, state := range []string{"discarded", "expired"} {
		id := e.newCapture(t, svc)
		e.exec(`UPDATE recovery_captures SET state = $2 WHERE id = $1`, id, state)
		for attempt := 1; attempt <= 2; attempt++ {
			if _, err := svc.Upload(e.ctx, e.wkr, e.runID, id, manifestOf(body), bytes.NewReader(body)); !errors.Is(err, ErrNotAvailable) {
				t.Fatalf("%s, upload %d = %v, want ErrNotAvailable", state, attempt, err)
			}
			if got, _ := e.captureState(t, id); got != state {
				t.Fatalf("%s, after upload %d the capture is %q, want it untouched", state, attempt, got)
			}
		}
	}
}

// TestRecoveryRetryClearsStaleReasonLiveDB: a needs_action capture retried through admission gets
// its reservation counted again AND its stale failure reason cleared.
//
// CALIBRATION: remove `reason = NULL` from StampCaptureReservation; the reason then survives.
func TestRecoveryRetryClearsStaleReasonLiveDB(t *testing.T) {
	e := newSFEnv(t)
	svc := e.recovery(1000)
	body := bytes.Repeat([]byte("r"), 100)
	id := e.newCapture(t, svc)
	if _, err := svc.Upload(e.ctx, e.wkr, e.runID, id, manifestOf(body), bytes.NewReader(nil)); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("first upload = %v, want ErrIntegrity", err)
	}
	var reason *string
	if err := e.pool.QueryRow(e.ctx, `SELECT reason FROM recovery_captures WHERE id = $1`, id).Scan(&reason); err != nil || reason == nil {
		t.Fatalf("after the failure: reason = %v (err %v), want a recorded reason", reason, err)
	}
	if _, done, err := svc.admit(e.ctx, e.wkr, e.runID, id, manifestOf(body)); err != nil || done {
		t.Fatalf("retry admission = done %v, %v", done, err)
	}
	if err := e.pool.QueryRow(e.ctx, `SELECT reason FROM recovery_captures WHERE id = $1`, id).Scan(&reason); err != nil || reason != nil {
		t.Fatalf("after the retry's admission: reason = %v (err %v), want NULL", reason, err)
	}
}

// TestRecoveryStalledReservationCannotInflateReclaimLiveDB: the bind-time reclaim is sized from
// COMMITTED bytes, never from another capture's unverified in-flight reservation. A worker that
// admits a capture declaring a large size and never streams it must not let a later, tiny verified
// upload reclaim (expire) job files to make room for bytes that were never sent.
//
// CALIBRATION: size the excess in reclaimForBudget from store.SumStoredFiles (which counts the
// preparing/uploading reservations) instead of SumCommittedSharedBytes; both subtests then expire a
// job file and go red.
func TestRecoveryStalledReservationCannotInflateReclaimLiveDB(t *testing.T) {
	t.Run("another capture stalls after admission", func(t *testing.T) {
		e := newSFEnv(t)
		svc, jf := e.recovery(1000), e.jobFiles()
		var victims []uuid.UUID
		for i, name := range []string{"v1.txt", "v2.txt", "v3.txt", "v4.txt"} {
			victims = append(victims, e.availableJobFile(t, jf, name, 200, 10-i))
		}
		stalled := bytes.Repeat([]byte("b"), 800)
		capB := e.newCapture(t, svc)
		if _, done, err := svc.admit(e.ctx, e.wkr, e.runID, capB, manifestOf(stalled)); err != nil || done {
			t.Fatalf("admit B (800 declared, 800 reclaimable) = done %v, %v; want admitted", done, err)
		}

		one := []byte("c")
		capC := e.newCapture(t, svc)
		if _, err := svc.Upload(e.ctx, e.wkr, e.runID, capC, manifestOf(one), bytes.NewReader(one)); err != nil {
			t.Fatalf("1-byte upload: %v", err)
		}
		if st, _ := e.captureState(t, capC); st != "available" {
			t.Fatalf("capture C = %q, want available", st)
		}
		for i, v := range victims {
			if got := e.jobState(t, v); got != "available" {
				t.Errorf("victim %d = %q, want available: a 1-byte verified upload reclaimed a file for B's unsent reservation", i+1, got)
			}
		}
	})

	t.Run("same worker stalls one capture and uploads another", func(t *testing.T) {
		e := newSFEnv(t)
		svc, jf := e.recovery(1000), e.jobFiles()
		victim := e.availableJobFile(t, jf, "big.txt", 900, 5)
		stalled := bytes.Repeat([]byte("b"), 900)
		capB := e.newCapture(t, svc)
		if _, done, err := svc.admit(e.ctx, e.wkr, e.runID, capB, manifestOf(stalled)); err != nil || done {
			t.Fatalf("admit B (900 declared, 900 reclaimable) = done %v, %v; want admitted", done, err)
		}

		body := bytes.Repeat([]byte("a"), 100)
		capA := e.newCapture(t, svc)
		if _, err := svc.Upload(e.ctx, e.wkr, e.runID, capA, manifestOf(body), bytes.NewReader(body)); err != nil {
			t.Fatalf("100-byte upload (900 + 100 fits exactly): %v", err)
		}
		if got := e.jobState(t, victim); got != "available" {
			t.Fatalf("victim = %q, want available: 900 committed + 100 verified fits the 1000 budget with no reclaim", got)
		}
	})
}

// TestRecoveryStalledJobFileReservationCannotInflateReclaimLiveDB: a job file still in state
// 'reserved' (size declared, nothing written) is not committed bytes, so it cannot drive the
// bind-time reclaim, and the bind leaves its declared room free instead of reclaiming to cover it
// (a job-file Write never re-checks the budget).
//
// Budget 1000; a victim's 900-byte file is available. An attacker reserves a 100-byte job file and
// never writes, then uploads a verified 1-byte capture. Committed 900 + verified 1 fits, so nothing
// is reclaimed; but 900 committed + 100 reserved + 1 verified = 1001 exceeds the budget, so the
// upload fails ErrQuota (rolled back) and the victim's file stays available. Without the stalled
// reservation the same upload succeeds and the victim stays available.
//
// CALIBRATION: count 'reserved' job files in committed_bytes again (state <> 'expired' in
// SumCommittedSharedBytes); the excess becomes 1 and the victim is expired, so the first subtest
// goes red.
func TestRecoveryStalledJobFileReservationCannotInflateReclaimLiveDB(t *testing.T) {
	one := []byte("c")

	t.Run("stalled reservation refuses the upload and destroys nothing", func(t *testing.T) {
		e := newSFEnv(t)
		svc, jf := e.recovery(1000), e.jobFiles()
		victim := e.availableJobFile(t, jf, "big.txt", 900, 5)

		attacker := uuid.New()
		e.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, attacker, fmt.Sprintf("sf-%s@e2e", attacker))
		res, err := jf.Reserve(e.ctx, workersvc.ReserveParams{UserID: attacker, Direction: workersvc.JobFileInput, DisplayName: "stall.txt", DeclaredSize: 100})
		if err != nil {
			t.Fatalf("attacker Reserve: %v", err)
		}

		capID := e.newCapture(t, svc)
		if _, err := svc.Upload(e.ctx, e.wkr, e.runID, capID, manifestOf(one), bytes.NewReader(one)); !errors.Is(err, ErrQuota) {
			t.Fatalf("1-byte upload with 900 committed + 100 reserved = %v, want ErrQuota (1001 > 1000)", err)
		}
		if got := e.jobState(t, victim); got != "available" {
			t.Fatalf("victim = %q, want available: a stalled reservation must not expire another user's file", got)
		}
		if got := e.jobState(t, res.ID); got != "reserved" {
			t.Fatalf("attacker reservation = %q, want it untouched", got)
		}
		if st, _ := e.captureState(t, capID); st == "available" {
			t.Fatalf("capture = %q, want it not bound", st)
		}
	})

	t.Run("control without the reservation succeeds", func(t *testing.T) {
		e := newSFEnv(t)
		svc, jf := e.recovery(1000), e.jobFiles()
		victim := e.availableJobFile(t, jf, "big.txt", 900, 5)
		capID := e.newCapture(t, svc)
		if _, err := svc.Upload(e.ctx, e.wkr, e.runID, capID, manifestOf(one), bytes.NewReader(one)); err != nil {
			t.Fatalf("1-byte upload (900 + 1 fits): %v", err)
		}
		if st, _ := e.captureState(t, capID); st != "available" {
			t.Fatalf("capture = %q, want available", st)
		}
		if got := e.jobState(t, victim); got != "available" {
			t.Fatalf("victim = %q, want available", got)
		}
	})
}

// TestMarkCaptureFailedNeverOverwritesADiscardLiveDB: the upload-failure write is conditional on
// the capture still being in a state an upload can be in, so a discard or expiry that commits
// between recordFailure's read and its write is not overwritten (and later revived by a retry).
//
// CALIBRATION: drop the AND state IN (...) guard from the MarkCaptureFailed query; the discarded and
// expired captures then flip to needs_action and this test goes red.
func TestMarkCaptureFailedNeverOverwritesADiscardLiveDB(t *testing.T) {
	e := newSFEnv(t)
	svc := e.recovery(1000)
	reason := pgtype.Text{String: "upload failed; retry available", Valid: true}

	for _, state := range []string{"discarded", "expired", "available"} {
		id := e.newCapture(t, svc)
		e.exec(`UPDATE recovery_captures SET state = $2 WHERE id = $1`, id, state)
		if _, err := e.q.MarkCaptureFailed(e.ctx, store.MarkCaptureFailedParams{Reason: reason, ID: id}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("%s: MarkCaptureFailed = %v, want pgx.ErrNoRows (nothing written)", state, err)
		}
		if got, _ := e.captureState(t, id); got != state {
			t.Fatalf("%s capture became %q after a failure write", state, got)
		}
	}
	for _, state := range []string{"preparing", "uploading", "needs_action"} {
		id := e.newCapture(t, svc)
		e.exec(`UPDATE recovery_captures SET state = $2 WHERE id = $1`, id, state)
		if _, err := e.q.MarkCaptureFailed(e.ctx, store.MarkCaptureFailedParams{Reason: reason, ID: id}); err != nil {
			t.Fatalf("%s: MarkCaptureFailed = %v, want it recorded", state, err)
		}
		if got, _ := e.captureState(t, id); got != "needs_action" {
			t.Fatalf("%s capture = %q after a failure write, want needs_action", state, got)
		}
	}
}
