package workersvc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1909 M4 rework 2: the generated-output storage runs detached from the ingest request, so a
// blocked stored-files lock neither delays nor fails the reply, and the outcome is either the files
// stored eventually or a recorded refusal. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres.

func (e jfEnv) refusalReasons(t *testing.T, run uuid.UUID, name string) []string {
	t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT reason FROM job_output_refusals WHERE run_id = $1 AND display_name = $2 ORDER BY created_at`, run, name)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// TestSubmitJobResultBlockedStorageDoesNotDelayReplyLiveDB: with the stored-files lock held
// elsewhere, SubmitJobResult still answers within the reply bound and the result is stored. If the
// lock is released before the storage deadline the files are stored; if not, a generation_timeout
// refusal is recorded for each.
//
// MUTATION CHECK: running storeJobResultOutputs inline again (waiting for it) makes the reply take
// the whole storage timeout and turns the elapsed assertion red.
func TestSubmitJobResultBlockedStorageDoesNotDelayReplyLiveDB(t *testing.T) {
	sub := JobResultSubmission{Status: "completed", ReportMD: "# report\n", Findings: []JobFindingSubmission{{Severity: "info", MessageMD: "a"}}}

	hold := func(t *testing.T, e jfEnv, owner uuid.UUID) func() {
		t.Helper()
		tx, err := e.pool.Begin(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.LockStoredFiles(e.ctx, tx, owner); err != nil {
			t.Fatal(err)
		}
		return func() { _ = tx.Rollback(e.ctx) }
	}

	t.Run("released before the storage deadline: files stored", func(t *testing.T) {
		e := newJFEnv(t, wide())
		e.svc.SetJobFiles(e.jf)
		e.svc.genReplyBound = 200 * time.Millisecond
		e.svc.genTimeout = 30 * time.Second
		u := e.seedJobUser(t)
		wkr, run := e.heldJob(t, u)
		release := hold(t, e, u)
		defer release()

		start := time.Now()
		if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, sub); err != nil {
			t.Fatalf("submit: %v", err)
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Fatalf("the reply took %s with the storage blocked, want about the %s reply bound", d, e.svc.genReplyBound)
		}
		var report string
		if err := e.pool.QueryRow(e.ctx, `SELECT report_md FROM job_results WHERE run_id = $1`, run).Scan(&report); err != nil || report != sub.ReportMD {
			t.Fatalf("stored result = %q, %v", report, err)
		}
		release()
		e.svc.WaitForGeneratedOutputs()
		if n := len(e.outputsNamed(t, run, "report.md")); n != 1 {
			t.Fatalf("report.md files = %d, want 1 once the lock cleared", n)
		}
		if n := len(e.outputsNamed(t, run, "findings.json")); n != 1 {
			t.Fatalf("findings.json files = %d, want 1 once the lock cleared", n)
		}
		if n := e.refusalCount(t, run); n != 0 {
			t.Fatalf("%d refusal rows, want none", n)
		}
	})

	t.Run("still blocked at the storage deadline: refusals recorded", func(t *testing.T) {
		e := newJFEnv(t, wide())
		e.svc.SetJobFiles(e.jf)
		e.svc.genReplyBound = 100 * time.Millisecond
		e.svc.genTimeout = 600 * time.Millisecond
		u := e.seedJobUser(t)
		wkr, run := e.heldJob(t, u)
		release := hold(t, e, u)
		defer release()

		start := time.Now()
		if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, sub); err != nil {
			t.Fatalf("submit must not fail: %v", err)
		}
		if d := time.Since(start); d > 500*time.Millisecond {
			t.Fatalf("the reply took %s, want it back at the %s reply bound, before the %s storage deadline", d, e.svc.genReplyBound, e.svc.genTimeout)
		}
		e.svc.WaitForGeneratedOutputs()
		for _, name := range []string{"report.md", "findings.json"} {
			got := e.refusalReasons(t, run, name)
			if len(got) != 1 || got[0] != RefusalGenerationTimeout {
				t.Fatalf("%s refusals = %v, want [%s]", name, got, RefusalGenerationTimeout)
			}
		}
		if n := len(e.outputsNamed(t, run, "report.md")); n != 0 {
			t.Fatalf("report.md stored despite the timeout")
		}
	})
}

// TestSubmitJobResultFindingsJSONNotHTMLEscapedLiveDB: markup in a finding is stored as written, not
// as < escapes, and a findings.json over the per-file cap is refused (recorded), never failing
// the ingest.
func TestSubmitJobResultFindingsJSONNotHTMLEscapedLiveDB(t *testing.T) {
	l := wide()
	l.OutputFileMaxBytes = 400
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	// 60 '<' is 60 bytes as written (well under 400 with the field names) but 360 HTML-escaped.
	msg := strings.Repeat("<", 60)
	sub := JobResultSubmission{Status: "completed", Findings: []JobFindingSubmission{{Severity: "info", MessageMD: msg}}}
	if err := e.svc.SubmitJobResult(e.ctx, wkr, run, 1, sub); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	ids := e.outputsNamed(t, run, "findings.json")
	if len(ids) != 1 {
		t.Fatalf("findings.json files = %d, want 1 (refusals: %v)", len(ids), e.refusalReasons(t, run, "findings.json"))
	}
	if got := string(e.outputBytes(t, ids[0], u)); !strings.Contains(got, msg) || strings.Contains(got, "\\"+"u003c") {
		t.Fatalf("findings.json = %q, want unescaped markup", got)
	}

	// Over the cap even unescaped: refused and recorded, ingest fine.
	wkr2, run2 := e.heldJob(t, u)
	big := JobResultSubmission{Status: "completed", Findings: []JobFindingSubmission{{Severity: "info", MessageMD: strings.Repeat("x", 500)}}}
	if err := e.svc.SubmitJobResult(e.ctx, wkr2, run2, 1, big); err != nil {
		t.Fatal(err)
	}
	e.svc.WaitForGeneratedOutputs()
	if got := e.refusalReasons(t, run2, "findings.json"); len(got) != 1 || got[0] != RefusalFileTooLarge {
		t.Fatalf("over-cap findings.json refusals = %v, want [%s]", got, RefusalFileTooLarge)
	}
}

// TestPerJobCapExemptionIsExactLiveDB: only the exact reserved names are exempt from the per-job
// caps. "fİndings.json" (dotted capital I) is not reserved for strings.EqualFold, and the SQL
// predicate must agree even where a libc UTF-8 collation lower-cases U+0130 to "i".
//
// MUTATION CHECK: restoring lower(display_name) in SumRunJobFiles lets the second upload through on
// a UTF-8 libc database (the exemption then leaves the first file uncounted).
func TestPerJobCapExemptionIsExactLiveDB(t *testing.T) {
	l := wide()
	l.OutputsMaxFiles = 1
	e := newJFEnv(t, l)
	e.svc.SetJobFiles(e.jf)
	u := e.seedJobUser(t)
	wkr, run := e.heldJob(t, u)
	if IsReservedJobOutputName("f\u0130ndings.json") {
		t.Fatal("test premise: f\u0130ndings.json is not a reserved name")
	}
	// The dotted-I name is stored as the job's own first output and must fill the one-file cap.
	if _, err := e.store(t, wkr, run, "f\u0130ndings.json", []byte("not the server's file")); err != nil {
		t.Fatal(err)
	}
	_, err := e.store(t, wkr, run, "data.txt", []byte("the second output"))
	var ref *JobFileRefusedError
	if !errors.As(err, &ref) || ref.Reason != RefusalTooManyFiles {
		t.Fatalf("second output err = %v, want a %s refusal (f\u0130ndings.json must count against the per-job cap)", err, RefusalTooManyFiles)
	}
}
