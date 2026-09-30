package recovery

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/uuid"

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
