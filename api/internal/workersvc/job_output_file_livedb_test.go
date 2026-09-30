package workersvc

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M4: the worker output upload and the re-claim reset. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// heldJob seeds a running job held by a fresh worker at generation 1 and returns the worker.
func (e jfEnv) heldJob(t *testing.T, u uuid.UUID) (store.Worker, uuid.UUID) {
	t.Helper()
	workerID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	run := e.seedRawJob(t, u, "running", &workerID, 0, 600)
	return store.Worker{ID: workerID, UserID: u, Name: "w", Status: "online", ProtocolCapabilities: []string{jobCap, jobFilesCap}}, run
}

func (e jfEnv) store(t *testing.T, wkr store.Worker, run uuid.UUID, name string, body []byte) (JobOutputResult, error) {
	t.Helper()
	return e.svc.StoreJobOutput(e.ctx, wkr, run, JobOutputParams{
		ClaimGeneration: 1, DisplayName: name, Size: int64(len(body)), SHA256: sumOf(body),
	}, bytes.NewReader(body), WriteOptions{ContentType: "text/plain"})
}

func (e jfEnv) refusalCount(t *testing.T, run uuid.UUID) int {
	t.Helper()
	return e.countRows(t, "job_output_refusals", run)
}

// TestStoreJobOutputSharedQuotasRecordRefusalsLiveDB: the instance quota and the shared stored-file
// budget refuse an output with a Quota refusal and record it; the file is not stored and the
// reservation is not left behind.
func TestStoreJobOutputSharedQuotasRecordRefusalsLiveDB(t *testing.T) {
	for _, c := range []struct {
		name   string
		limits JobFileLimits
		reason string
	}{
		{"instance", func() JobFileLimits { l := wide(); l.InstanceBytes = 10; return l }(), RefusalInstanceQuota},
		{"shared budget", func() JobFileLimits { l := wide(); l.StoredFilesBudgetBytes = 10; return l }(), RefusalStoredBudget},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newJFEnv(t, c.limits)
			e.svc.SetJobFiles(e.jf)
			u := e.seedJobUser(t)
			wkr, run := e.heldJob(t, u)
			if _, err := e.store(t, wkr, run, "a.txt", []byte("12345678")); err != nil {
				t.Fatalf("first: %v", err)
			}
			_, err := e.store(t, wkr, run, "b.txt", []byte("1234"))
			if !refusedWith(err, RefusalQuota, c.reason) {
				t.Fatalf("second = %v, want a %s quota refusal", err, c.reason)
			}
			if n := e.refusalCount(t, run); n != 1 {
				t.Fatalf("refusal rows = %d, want 1", n)
			}
			if got := e.sums(t, u).OwnerJobBytes; got != 8 {
				t.Fatalf("owner job bytes = %d, want only the first file's 8", got)
			}
		})
	}
}

// TestStoreJobOutputStaleAfterWriteDropsFileLiveDB: a claim that goes stale while the body streams
// (a re-claim, here simulated from inside the body read) drops the file it just stored and answers
// ErrStaleClaim, so an old flight's output never survives into the next.
func TestStoreJobOutputStaleAfterWriteDropsFileLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	body := []byte("late output")
	r := &onFirstRead{r: bytes.NewReader(body), fn: func() {
		e.exec(`UPDATE runs SET claim_generation = 2 WHERE id = $1`, run)
	}}
	_, err := e.svc.StoreJobOutput(e.ctx, wkr, run, JobOutputParams{ClaimGeneration: 1, DisplayName: "late.txt", Size: int64(len(body)), SHA256: sumOf(body)}, r, WriteOptions{ContentType: "text/plain"})
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("err = %v, want ErrStaleClaim", err)
	}
	if n := e.countRows(t, "job_files", run); n != 0 {
		t.Fatalf("%d job_files rows survive a stale upload", n)
	}
}

type onFirstRead struct {
	r    io.Reader
	fn   func()
	done bool
}

func (o *onFirstRead) Read(p []byte) (int, error) {
	if !o.done {
		o.done = true
		o.fn()
	}
	return o.r.Read(p)
}

// TestJobReclaimClearsEarlierOutputsLiveDB: a requeued job that is claimed again starts with none
// of the earlier flight's OUTPUT files or refusal rows (its input files stay), so a new flight's
// caps and result list only its own uploads.
//
// MUTATION CHECK: removing the ClearRunOutputs call from assembleJobClaim leaves the earlier
// flight's output and refusal in place and this test goes red.
func TestJobReclaimClearsEarlierOutputsLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	e.makeTokenDefault(t, u)
	v, err := e.svc.CreateJobRun(e.ctx, jobReq(cliCaller(u)))
	if err != nil {
		t.Fatal(err)
	}
	runID := v.ID
	input := e.put(t, ReserveParams{UserID: u, RunID: &runID, DisplayName: "in.txt"}, []byte("an input"))
	workerID := e.seedWorkerRow(t, u, false, nil, jobCap, jobFilesCap)
	wkr := store.Worker{ID: workerID, UserID: u, Name: "w", Status: "online", ProtocolCapabilities: []string{jobCap, jobFilesCap}}

	pl, err := e.svc.Claim(e.ctx, wkr, nil)
	if err != nil || pl == nil || pl.ClaimGeneration != 1 {
		t.Fatalf("first claim = %+v, %v", pl, err)
	}
	e.exec(`UPDATE runs SET status = 'running', started_at = now() WHERE id = $1`, runID)
	out, err := e.store(t, wkr, runID, "flight-one.txt", []byte("flight one output"))
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason) VALUES ($1, 'refused.exe', 9, 'unsupported_file_type')`, runID)
	// A reserved output whose upload is still in flight is cleared too.
	reserved, err := e.jf.Reserve(e.ctx, ReserveParams{UserID: u, RunID: &runID, Direction: JobFileOutput, ClaimGeneration: i64Ptr(1), DisplayName: "half.txt", DeclaredSize: 4})
	if err != nil {
		t.Fatal(err)
	}

	e.exec(`UPDATE runs SET status = 'queued', claim_released_at = NULL WHERE id = $1`, runID)
	pl, err = e.svc.Claim(e.ctx, wkr, nil)
	if err != nil || pl == nil || pl.ClaimGeneration != 2 {
		t.Fatalf("second claim = %+v, %v; want generation 2", pl, err)
	}
	if e.fileExists(t, out.File.ID) || e.fileExists(t, reserved.ID) {
		t.Fatal("an earlier flight's output survived the re-claim")
	}
	if e.chunkCount(t, out.File.ID) != 0 {
		t.Fatal("the earlier output's chunks survived (the cascade did not run)")
	}
	if n := e.refusalCount(t, runID); n != 0 {
		t.Fatalf("%d refusal rows survived the re-claim", n)
	}
	if !e.fileExists(t, input.ID) {
		t.Fatal("the re-claim deleted an INPUT file")
	}
	if len(pl.Job.Files) != 1 || pl.Job.Files[0].ID != input.ID.String() {
		t.Fatalf("second claim's files = %+v, want the one input", pl.Job.Files)
	}
	// The new flight's caps start from zero: its own upload is admitted at the new generation.
	if _, err := e.svc.StoreJobOutput(e.ctx, wkr, runID, JobOutputParams{ClaimGeneration: 2, DisplayName: "flight-two.txt", Size: 3, SHA256: sumOf([]byte("two"))}, bytes.NewReader([]byte("two")), WriteOptions{ContentType: "text/plain"}); err != nil {
		t.Fatalf("second flight upload: %v", err)
	}
}
