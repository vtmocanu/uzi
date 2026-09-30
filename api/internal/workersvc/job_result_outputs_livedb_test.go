package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// PRD #1909 M4 rework: the server stores report.md and findings.json from the stored result, the
// worker-reported dropped outputs become refusal rows, the output index refuses duplicates, and a
// re-claim / stale refusal is fenced. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres.

// outputBytes reads the stored bytes of an output row.
func (e jfEnv) outputBytes(t *testing.T, id uuid.UUID, owner uuid.UUID) []byte {
	t.Helper()
	_, r, err := e.jf.Open(e.ctx, id, owner)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// outputByName returns the ids of the run's live output files named name.
func (e jfEnv) outputsNamed(t *testing.T, run uuid.UUID, name string) []uuid.UUID {
	t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT id FROM job_files WHERE run_id = $1 AND direction = 'output' AND display_name = $2 AND state <> 'expired'`, run, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// TestSubmitJobResultStoresGeneratedOutputsLiveDB: after the result is stored the SERVER stores
// report.md (the stored, scrubbed report) and findings.json (the stored findings) as output files;
// a re-post with the same content stores nothing new and one with different content replaces them;
// they do not count against the per-job output caps.
//
// MUTATION CHECK: removing the startJobResultOutputs call from SubmitJobResult leaves no file and
// turns this red.
func TestSubmitJobResultStoresGeneratedOutputsLiveDB(t *testing.T) {
	l := wide()
	l.OutputsMaxFiles = 1
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)

	// The job's one allowed output fills the per-job file cap; the generated files still fit.
	if _, err := e.store(t, wkr, run, "data.txt", []byte("the one allowed output")); err != nil {
		t.Fatal(err)
	}
	sub := JobResultSubmission{Status: "completed", ReportMD: "# Report\n\nbody\n", Findings: []JobFindingSubmission{
		{Severity: "info", MessageMD: "a"},
		{Severity: "error", MessageMD: "b", File: strp("x.go"), Line: i32p(3)},
	}}
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, sub); err != nil {
		t.Fatalf("submit: %v", err)
	}
	reports, findings := e.outputsNamed(t, run, "report.md"), e.outputsNamed(t, run, "findings.json")
	if len(reports) != 1 || len(findings) != 1 {
		t.Fatalf("report.md files = %d, findings.json files = %d, want 1 and 1", len(reports), len(findings))
	}
	if got := e.outputBytes(t, reports[0], u); string(got) != sub.ReportMD {
		t.Fatalf("report.md = %q, want the stored report", got)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(e.outputBytes(t, findings[0], u), &decoded); err != nil || len(decoded) != 2 || decoded[1]["file"] != "x.go" || decoded[1]["message_md"] != "b" {
		t.Fatalf("findings.json = %v, %v", decoded, err)
	}
	var ct string
	if err := e.pool.QueryRow(e.ctx, `SELECT content_type FROM job_files WHERE id = $1`, reports[0]).Scan(&ct); err != nil || ct != "text/markdown" {
		t.Fatalf("report.md content type = %q, %v", ct, err)
	}
	if n := e.refusalCount(t, run); n != 0 {
		t.Fatalf("%d refusal rows after a clean submit", n)
	}

	// Same content again: nothing new, the same files.
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, sub); err != nil {
		t.Fatalf("re-post: %v", err)
	}
	if again := e.outputsNamed(t, run, "report.md"); len(again) != 1 || again[0] != reports[0] {
		t.Fatalf("a same-content re-post changed the report files: %v", again)
	}
	// Different content: replaced, still one of each.
	sub.ReportMD = "# Report v2\n"
	sub.Findings = nil
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, sub); err != nil {
		t.Fatalf("replace: %v", err)
	}
	reports, findings = e.outputsNamed(t, run, "report.md"), e.outputsNamed(t, run, "findings.json")
	if len(reports) != 1 || len(findings) != 1 {
		t.Fatalf("after a replace: %d report.md and %d findings.json, want 1 and 1", len(reports), len(findings))
	}
	if got := e.outputBytes(t, reports[0], u); string(got) != "# Report v2\n" {
		t.Fatalf("replaced report.md = %q", got)
	}
	if got := e.outputBytes(t, findings[0], u); string(bytes.TrimSpace(got)) != "[]" {
		t.Fatalf("replaced findings.json = %q, want []", got)
	}
}

// TestSubmitJobResultGeneratedOutputRefusalNeverFailsIngestLiveDB: a quota refusal of a generated
// file is recorded in job_output_refusals and the result is still stored.
func TestSubmitJobResultGeneratedOutputRefusalNeverFailsIngestLiveDB(t *testing.T) {
	l := wide()
	l.PerOwnerBytes = 4 // too small for any report
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, JobResultSubmission{Status: "completed", ReportMD: "a report over four bytes"}); err != nil {
		t.Fatalf("submit must not fail on a refused output: %v", err)
	}
	var report string
	if err := e.pool.QueryRow(e.ctx, `SELECT report_md FROM job_results WHERE run_id = $1`, run).Scan(&report); err != nil || report != "a report over four bytes" {
		t.Fatalf("stored result = %q, %v", report, err)
	}
	var reason string
	if err := e.pool.QueryRow(e.ctx, `SELECT reason FROM job_output_refusals WHERE run_id = $1 AND display_name = 'report.md'`, run).Scan(&reason); err != nil || reason != RefusalOwnerQuota {
		t.Fatalf("report.md refusal = %q, %v; want %s", reason, err, RefusalOwnerQuota)
	}
	if n := len(e.outputsNamed(t, run, "report.md")); n != 0 {
		t.Fatalf("%d report.md files stored past the quota", n)
	}
}

// TestStoreJobOutputRefusesReservedNamesLiveDB: a worker upload named report.md or findings.json
// (any case) is refused and recorded, so the agent cannot spoof the server-generated files.
//
// MUTATION CHECK: removing the IsReservedJobOutputName check in StoreJobOutput stores the upload.
func TestStoreJobOutputRefusesReservedNamesLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	for _, name := range []string{"report.md", "findings.json", "Report.MD"} {
		_, err := e.store(t, wkr, run, name, []byte("spoofed"))
		if !refusedWith(err, RefusalInvalid, RefusalReservedName) {
			t.Fatalf("%s: err = %v, want a reserved_name refusal", name, err)
		}
	}
	if n := e.countRows(t, "job_files", run); n != 0 {
		t.Fatalf("%d files stored under reserved names", n)
	}
	if n := e.refusalCount(t, run); n != 3 {
		t.Fatalf("refusal rows = %d, want 3", n)
	}
}

// TestSubmitJobResultRecordsWorkerRefusedOutputsLiveDB: the outputs the worker reports dropping
// become refusal rows, deduped on a re-post and bounded by OutputsMaxFiles. The bound is on all the
// run's refusal rows, so the two generation_pending markers written first take two of the five
// slots while the generation runs (the worker's rows fill the other three); once the files are
// stored the markers are gone.
//
// MUTATION CHECK: dropping the RefusedOutputs loop in SubmitJobResult leaves no rows.
func TestSubmitJobResultRecordsWorkerRefusedOutputsLiveDB(t *testing.T) {
	l := wide()
	l.OutputsMaxFiles = 5
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	sub := JobResultSubmission{Status: "completed", ReportMD: "r", RefusedOutputs: []JobRefusedOutput{
		{DisplayName: "empty.csv", Reason: WorkerRefusalEmpty},
		{DisplayName: "huge.pdf", Reason: WorkerRefusalTooLarge},
		{DisplayName: "gone.txt", Reason: WorkerRefusalUnreadable},
		{DisplayName: "late.txt", Reason: WorkerRefusalUploadFailed},
		{DisplayName: "busy.txt", Reason: WorkerRefusalBusy},
	}}
	for i := 0; i < 2; i++ { // the re-post must not double the rows
		if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, sub); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
		e.svc.WaitForGeneratedOutputs()
	}
	if n := e.refusalCount(t, run); n != 3 {
		t.Fatalf("refusal rows = %d, want 3 (OutputsMaxFiles 5 less the 2 generation markers), none doubled", n)
	}
	var reason string
	if err := e.pool.QueryRow(e.ctx, `SELECT reason FROM job_output_refusals WHERE run_id = $1 AND display_name = 'huge.pdf'`, run).Scan(&reason); err != nil || reason != WorkerRefusalTooLarge {
		t.Fatalf("huge.pdf refusal = %q, %v", reason, err)
	}
}

// TestStoreJobOutputConcurrentDuplicateLiveDB: N concurrent uploads of the same content under the
// same name store ONE file. The others are answered with the stored file or, while the first is
// still streaming, ErrJobFileUploadsBusy; a retry then finds the stored file. The unique output
// index (not the lookup) is what holds this under concurrency.
//
// MUTATION CHECK: dropping uq_job_files_output_content from migration 00276 stores more than one.
func TestStoreJobOutputConcurrentDuplicateLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	body := bytes.Repeat([]byte("same content\n"), 64)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = e.svc.StoreJobOutput(e.ctx, wkr, run, JobOutputParams{ClaimGeneration: 1, DisplayName: "dup.txt", Size: int64(len(body)), SHA256: sumOf(body)}, bytes.NewReader(body), WriteOptions{ContentType: "text/plain"})
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil && !errors.Is(err, ErrJobFileUploadsBusy) {
			t.Fatalf("upload %d: unexpected error %v", i, err)
		}
	}
	if got := len(e.outputsNamed(t, run, "dup.txt")); got != 1 {
		t.Fatalf("%d dup.txt files stored, want exactly 1", got)
	}
	res, err := e.store(t, wkr, run, "dup.txt", body)
	if err != nil || !res.Existing {
		t.Fatalf("retry = %+v, %v; want the stored file (Existing)", res, err)
	}
	if got := e.sums(t, u).OwnerJobBytes; got != int64(len(body)) {
		t.Fatalf("owner job bytes = %d, want one copy (%d)", got, len(body))
	}
}

// TestJobOutputRefusalFencedOnClaimLiveDB: a refusal insert is dropped when its claim generation is
// no longer the run's (a stale flight after a re-claim) or the claim is released.
//
// MUTATION CHECK: removing the EXISTS fence from InsertJobOutputRefusal records the stale refusal.
func TestJobOutputRefusalFencedOnClaimLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	_, run := e.heldJob(t, u)
	e.jf.insertRefusal(e.ctx, run, 1, "a.exe", 1, "unsupported_type")
	if n := e.refusalCount(t, run); n != 1 {
		t.Fatalf("current generation: %d rows, want 1", n)
	}
	e.exec(`UPDATE runs SET claim_generation = 2 WHERE id = $1`, run)
	e.jf.insertRefusal(e.ctx, run, 1, "b.exe", 1, "unsupported_type")
	if n := e.refusalCount(t, run); n != 1 {
		t.Fatalf("stale generation: %d rows, want the stale refusal dropped", n)
	}
	e.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, run)
	e.jf.insertRefusal(e.ctx, run, 2, "c.exe", 1, "unsupported_type")
	if n := e.refusalCount(t, run); n != 1 {
		t.Fatalf("released claim: %d rows, want the refusal dropped", n)
	}
}

// TestClearRunOutputsSkipsLockedRowsLiveDB: the re-claim reset never waits on a row an old flight's
// write holds; it deletes the rest at once and the locked row is left for that write's own fence.
//
// MUTATION CHECK: replacing the FOR UPDATE SKIP LOCKED subselects with a plain DELETE makes
// ClearRunOutputs block until the row lock is released and this test times out.
func TestClearRunOutputsSkipsLockedRowsLiveDB(t *testing.T) {
	e := newJFEnv(t, wide())
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	free, err := e.store(t, wkr, run, "free.txt", []byte("free"))
	if err != nil {
		t.Fatal(err)
	}
	held, err := e.store(t, wkr, run, "held.txt", []byte("held"))
	if err != nil {
		t.Fatal(err)
	}
	e.exec(`INSERT INTO job_output_refusals (run_id, display_name, byte_size, reason) VALUES ($1, 'free.exe', 1, 'x'), ($1, 'held.exe', 1, 'y')`, run)

	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM job_files WHERE id = $1 FOR UPDATE`, held.File.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM job_output_refusals WHERE run_id = $1 AND display_name = 'held.exe' FOR UPDATE`, run); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	begin := time.Now()
	if err := e.jf.ClearRunOutputs(cctx, run); err != nil {
		t.Fatalf("ClearRunOutputs while a row is locked: %v", err)
	}
	if d := time.Since(begin); d > 3*time.Second {
		t.Fatalf("ClearRunOutputs waited %s on a locked row", d)
	}
	if e.fileExists(t, free.File.ID) {
		t.Fatal("the unlocked output survived")
	}
	if !e.fileExists(t, held.File.ID) {
		t.Fatal("the locked output was deleted under its holder")
	}
	if n := e.refusalCount(t, run); n != 1 {
		t.Fatalf("refusal rows = %d, want only the locked one left", n)
	}
}
