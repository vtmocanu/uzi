package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	pubHeadA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pubHeadB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pubHeadC = "cccccccccccccccccccccccccccccccccccccccc"
)

// publicationResumeFixture is one owner whose worker advertises publication_resume_v1 and one
// running issue run at claim generation 1, owned by that worker.
type publicationResumeFixture struct {
	*custodyContinuationFixture
	runID uuid.UUID
}

func newPublicationResumeFixture(t *testing.T) *publicationResumeFixture {
	t.Helper()
	env := setupCodexLiveDB(t)
	f := newCustodyContinuationFixture(t, env)
	f.svc.p.PublicationResume = true
	f.wkr.ProtocolCapabilities = []string{capability.RecoveryArchiveV1, capability.PublicationResumeV1}
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, f.wkr.ProtocolCapabilities)
	id := f.seedRun("running", 1)
	env.exec("UPDATE runs SET worker_id=$2, started_at=now(), branch='agent/issue-1' WHERE id=$1", id, f.workerID)
	return &publicationResumeFixture{custodyContinuationFixture: f, runID: id}
}

func (f *publicationResumeFixture) intent(branch, head string, candidate *string) json.RawMessage {
	return f.intentAt(1, branch, head, candidate)
}

// intentAt is an intent whose claim_generation (the enrollment generation) is gen.
func (f *publicationResumeFixture) intentAt(gen int64, branch, head string, candidate *string) json.RawMessage {
	body := map[string]any{
		"run_id": f.runID, "repo_id": f.repoID, "branch": branch, "claim_generation": gen,
		"original_head": head, "floor": map[string]any{"kind": "absent"}, "candidate": candidate,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

func (f *publicationResumeFixture) enroll(t *testing.T) PublicationIntentResponse {
	t.Helper()
	return f.enrollAt(t, 1)
}

func (f *publicationResumeFixture) enrollAt(t *testing.T, gen int64) PublicationIntentResponse {
	t.Helper()
	resp, err := f.svc.RegisterPublicationIntent(f.env.ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: gen, Intent: f.intentAt(gen, "agent/issue-1", pubHeadA, nil)})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return resp
}

// reclaim moves the run to generation gen the way a claim does: with the run-fence opt-ins.
func (f *publicationResumeFixture) reclaim(t *testing.T, gen int64) {
	t.Helper()
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(f.env.ctx) }()
	if err = store.New(tx).OptInRunFences(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(f.env.ctx, `UPDATE runs SET claim_generation=$2 WHERE id=$1`, f.runID, gen); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(f.env.ctx); err != nil {
		t.Fatal(err)
	}
}

func seq(n int32) *int32 { return &n }

func TestPublicationResumeEnrollmentLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	ctx := f.env.ctx
	first := f.enroll(t)
	if first.EnrollmentSeq != 1 || first.CandidateSeq != 0 || first.State != "active" {
		t.Fatalf("first=%+v", first)
	}
	// Idempotent identity compare returns the existing enrollment.
	again := f.enroll(t)
	if again != first {
		t.Fatalf("repeat=%+v want %+v", again, first)
	}
	var rows int
	if err := f.env.pool.QueryRow(ctx, `SELECT count(*) FROM run_publication_resume WHERE run_id=$1`, f.runID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
	// Identity mismatch on the same run.
	_, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, Intent: f.intent("agent/issue-1", pubHeadB, nil)})
	if !errors.Is(err, ErrPublicationIntentMismatch) {
		t.Fatalf("mismatch err=%v", err)
	}
	// Candidate CAS: current+1 stores, repeat is idempotent, a skip or a different repeat is a mismatch.
	c := pubHeadC
	upd, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, EnrollmentSeq: seq(1), CandidateSeq: seq(1), Intent: f.intent("agent/issue-1", pubHeadA, &c)})
	if err != nil || upd.CandidateSeq != 1 {
		t.Fatalf("update=%+v err=%v", upd, err)
	}
	repeat, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, EnrollmentSeq: seq(1), CandidateSeq: seq(1), Intent: f.intent("agent/issue-1", pubHeadA, &c)})
	if err != nil || repeat.CandidateSeq != 1 {
		t.Fatalf("lost-ACK repeat=%+v err=%v", repeat, err)
	}
	other := pubHeadB
	for name, req := range map[string]PublicationIntentRequest{
		"skip":               {ClaimGeneration: 1, EnrollmentSeq: seq(1), CandidateSeq: seq(3), Intent: f.intent("agent/issue-1", pubHeadA, &c)},
		"different repeat":   {ClaimGeneration: 1, EnrollmentSeq: seq(1), CandidateSeq: seq(1), Intent: f.intent("agent/issue-1", pubHeadA, &other)},
		"stale lower":        {ClaimGeneration: 1, EnrollmentSeq: seq(1), CandidateSeq: seq(1), Intent: f.intent("agent/issue-1", pubHeadA, nil)},
		"identity in update": {ClaimGeneration: 1, EnrollmentSeq: seq(1), CandidateSeq: seq(2), Intent: f.intent("agent/issue-1", pubHeadB, &c)},
	} {
		if _, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID, req); !errors.Is(err, ErrPublicationIntentMismatch) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}
}

func TestPublicationResumeRetireNeedsOptInLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	ctx := f.env.ctx
	f.enroll(t)
	_, err := f.env.pool.Exec(ctx, `UPDATE run_publication_resume SET state='retired', retired_reason='x' WHERE run_id=$1`, f.runID)
	requireCoordinateSQLState(t, err, "23514")
	// A valid intent, so the refusal is the missing opt-in and not a shape or key problem.
	_, err = f.env.pool.Exec(ctx, `INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, intent, state)
		VALUES ($1, 2, 1, $2, 'active')`, f.runID, f.intent("agent/issue-1", pubHeadA, nil))
	requireCoordinateSQLState(t, err, "23514")
	if err == nil || !strings.Contains(err.Error(), "enrollment requires updated API") {
		t.Fatalf("insert without opt-in err=%v, want the enrollment opt-in refusal", err)
	}
	// A direct delete needs the opt-in too.
	_, err = f.env.pool.Exec(ctx, `DELETE FROM run_publication_resume WHERE run_id=$1`, f.runID)
	requireCoordinateSQLState(t, err, "23514")
	tx, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err = q.OptInPublicationResume(ctx); err != nil {
		t.Fatal(err)
	}
	// Identity is immutable even with the opt-in.
	sp, _ := tx.Begin(ctx)
	_, err = sp.Exec(ctx, `UPDATE run_publication_resume SET intent = jsonb_set(intent::jsonb, '{branch}', '"other"')::text::json WHERE run_id=$1`, f.runID)
	requireCoordinateSQLState(t, err, "23514")
	_ = sp.Rollback(ctx)
	retired, err := q.RetirePublicationResume(ctx, store.RetirePublicationResumeParams{RunID: f.runID, Reason: "owner_completion_continue"})
	if err != nil || retired.State != "retired" {
		t.Fatalf("retire=%+v err=%v", retired, err)
	}
	// Retired is terminal.
	sp, _ = tx.Begin(ctx)
	_, err = sp.Exec(ctx, `UPDATE run_publication_resume SET state='active' WHERE run_id=$1`, f.runID)
	requireCoordinateSQLState(t, err, "23514")
	_ = sp.Rollback(ctx)
	// A retired seq refuses candidate writes with the retired error; a new enrollment gets seq 2.
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	c := pubHeadC
	_, err = f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, EnrollmentSeq: seq(1), CandidateSeq: seq(1), Intent: f.intent("agent/issue-1", pubHeadA, &c)})
	if !errors.Is(err, ErrPublicationEnrollmentRetired) {
		t.Fatalf("retired write err=%v", err)
	}
	// No resurrection: a fresh enrollment at the retired generation is refused, and so is one at a
	// lower generation than the retired row.
	_, err = f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, Intent: f.intent("agent/issue-1", pubHeadA, nil)})
	if !errors.Is(err, ErrPublicationEnrollmentRetired) {
		t.Fatalf("re-enroll at the retired generation err=%v", err)
	}
	// A later claim generation may enrol afresh and gets the next sequence.
	f.reclaim(t, 2)
	next := f.enrollAt(t, 2)
	if next.EnrollmentSeq != 2 || next.State != "active" {
		t.Fatalf("re-enroll=%+v", next)
	}
}

// The stored intent's claim_generation is the ENROLLMENT generation. A candidate update after a
// re-claim at a higher generation succeeds under the live fence (request generation == run
// generation); the stale generation is refused and identity stays immutable.
func TestPublicationResumeCandidateAfterReclaimLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	ctx := f.env.ctx
	f.enroll(t)
	f.reclaim(t, 2)
	c := pubHeadC
	upd, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 2, EnrollmentSeq: seq(1), CandidateSeq: seq(1), Intent: f.intentAt(1, "agent/issue-1", pubHeadA, &c)})
	if err != nil || upd.CandidateSeq != 1 || upd.EnrollmentSeq != 1 {
		t.Fatalf("update at generation 2 = %+v err=%v", upd, err)
	}
	var stored int64
	if err = f.env.pool.QueryRow(ctx, `SELECT (intent->>'claim_generation')::bigint FROM run_publication_resume WHERE run_id=$1`, f.runID).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("stored enrollment generation=%d err=%v", stored, err)
	}
	// The stale request generation is refused at the live fence.
	_, err = f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, EnrollmentSeq: seq(1), CandidateSeq: seq(2), Intent: f.intentAt(1, "agent/issue-1", pubHeadA, &c)})
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("stale generation err=%v, want ErrStaleClaim", err)
	}
	// Rewriting the enrollment generation to the live one is an identity change.
	_, err = f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 2, EnrollmentSeq: seq(1), CandidateSeq: seq(2), Intent: f.intentAt(2, "agent/issue-1", pubHeadA, &c)})
	if !errors.Is(err, ErrPublicationIntentMismatch) {
		t.Fatalf("identity rewrite err=%v, want mismatch", err)
	}
}

// The intent is validated from exact, unique, allowlisted keys and bound to the run's own branch.
func TestPublicationResumeIntentValidationLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	ctx := f.env.ctx
	id := f.runID.String()
	repo := f.repoID.String()
	base := func(extra string) json.RawMessage {
		return json.RawMessage(`{"run_id":"` + id + `","repo_id":"` + repo + `","branch":"agent/issue-1","claim_generation":1,` +
			`"original_head":"` + pubHeadA + `","floor":{"kind":"absent"},"candidate":null` + extra + `}`)
	}
	for name, raw := range map[string]json.RawMessage{
		"unknown member":      base(`,"extra":1`),
		"case variant":        base(`,"Branch":"x"`),
		"duplicate key":       base(`,"branch":"agent/issue-1"`),
		"branch not the run":  json.RawMessage(strings.Replace(string(base("")), "agent/issue-1", "agent/other", 1)),
		"ansi in branch":      json.RawMessage(strings.Replace(string(base("")), "agent/issue-1", `agent/\u001b[31m`, 1)),
		"bidi in branch":      json.RawMessage(strings.Replace(string(base("")), "agent/issue-1", "agent/\\u202e", 1)),
		"nested floor casing": json.RawMessage(strings.Replace(string(base("")), `"kind"`, `"Kind"`, 1)),
		"trailing data":       append(base(""), []byte(` {}`)...),
		"unicode nul":         base(`,"mr":{"title":"a\u0000b"}`),
		"lone surrogate":      base(`,"mr":{"title":"a\ud800b"}`),
		"uppercase run id":    json.RawMessage(strings.Replace(string(base("")), id, strings.ToUpper(id), 1)),
		"braced repo id":      json.RawMessage(strings.Replace(string(base("")), `"`+repo+`"`, `"{`+repo+`}"`, 1)),
		"absent floor oid":    json.RawMessage(strings.Replace(string(base("")), `{"kind":"absent"}`, `{"kind":"absent","oid":null}`, 1)),
		"pinned floor no oid": json.RawMessage(strings.Replace(string(base("")), `{"kind":"absent"}`, `{"kind":"pinned","oid":null}`, 1)),
		"deep nesting":        base(`,"mr":` + strings.Repeat("[", 64) + strings.Repeat("]", 64)),
	} {
		_, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID, PublicationIntentRequest{ClaimGeneration: 1, Intent: raw})
		if !errors.Is(err, ErrInvalidState) {
			t.Errorf("%s: err=%v, want ErrInvalidState", name, err)
		}
	}
	// Allowlisted opaque members are accepted.
	if _, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, Intent: base(`,"v":1,"mr":{"title":"t"},"guard":{},"mac":"m"`)}); err != nil {
		t.Fatalf("allowlisted members: %v", err)
	}
	// Body bounds: the raw intent is capped at 64 KiB in Go.
	big := base(`,"mr":{"pad":"` + strings.Repeat("x", MaxPublicationIntentBytes) + `"}`)
	if _, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID, PublicationIntentRequest{ClaimGeneration: 1, Intent: big}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("oversize err=%v", err)
	}
}

func TestPublicationResumeKindUnsupportedLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	// A repo-backed kind that never publishes code: a report-only cross-check child of the run.
	f.env.exec(`UPDATE runs SET kind='cross_check', issue_iid=NULL, target_run_id=$1, harness='codex',
		report_only=true, budget_wall_seconds=3600 WHERE id=$1`, f.runID)
	_, err := f.svc.RegisterPublicationIntent(f.env.ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, Intent: f.intent("agent/issue-1", pubHeadA, nil)})
	if !errors.Is(err, ErrPublicationKindUnsupported) {
		t.Fatalf("err=%v, want ErrPublicationKindUnsupported", err)
	}
}

// The state report that re-enters a live status (awaiting_input to running) is an authority write
// on an enrolled run: it takes the bound worker's row lock before the run row. Another
// transaction holding that worker lock must therefore stall the report until it is released.
func TestPublicationResumeSetStateAuthorityLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	f.enroll(t)
	f.env.exec(`UPDATE runs SET status='awaiting_input', status_since=now(), open_question_id='q1' WHERE id=$1`, f.runID)
	f.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body, question_id, applied_at) VALUES ($1, 'answer', 'yes', 'q1', now())`, f.runID)
	holder, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(f.env.ctx) }()
	var locked uuid.UUID
	if err = holder.QueryRow(f.env.ctx, `SELECT id FROM workers WHERE id=$1 FOR UPDATE`, f.workerID).Scan(&locked); err != nil {
		t.Fatal(err)
	}
	type result struct {
		status  string
		applied bool
		err     error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(f.env.ctx, 30*time.Second)
		defer cancel()
		g := int64(1)
		run, applied, err := f.svc.SetState(ctx, f.wkr, f.runID, StateRequest{State: "running", ClaimGeneration: &g})
		done <- result{run.Status, applied, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("SetState completed while the worker row was locked elsewhere: %+v", r)
	case <-time.After(750 * time.Millisecond):
	}
	if err = holder.Rollback(f.env.ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || !r.applied || r.status != "running" {
			t.Fatalf("awaiting_input to running after the lock was released: %+v", r)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("SetState did not complete after the worker lock was released")
	}
}

// A credential_disabled run with a pending pause settles into the owner pause, which reopens the
// released claim. For an enrolled run whose bound worker is gone or lacks the capability that
// would raise the run guard on every promoter pass; it must be refused cleanly instead. Each case
// has its own fixture, so one case's mutation cannot mask or cause another's outcome.
func TestPublicationResumeCredentialDisabledSettleLiveDB(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *publicationResumeFixture)
	}{
		{"worker row without the capability", func(f *publicationResumeFixture) {
			f.env.exec(`UPDATE workers SET protocol_capabilities='{}' WHERE id=$1`, f.workerID)
		}},
		{"worker unbound", func(f *publicationResumeFixture) {
			f.env.exec(`UPDATE runs SET worker_id=NULL WHERE id=$1`, f.runID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationResumeFixture(t)
			ctx := f.env.ctx
			f.enroll(t)
			f.env.exec(`UPDATE runs SET status='paused', hold_reason='credential_disabled', status_since=now(),
				claim_released_at=now(), pause_requested_at=now(), pause_mode='milestone' WHERE id=$1`, f.runID)
			tc.setup(f)
			status, err := f.svc.promoteCredentialDisabledRun(ctx, f.userID, f.runID)
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) {
				t.Fatalf("promoter raised %s: %v", pgErr.Code, err)
			}
			if err != nil || status != "" {
				t.Fatalf("status=%q err=%v, want a clean refusal", status, err)
			}
			var held string
			if err := f.env.pool.QueryRow(ctx, `SELECT status||'/'||COALESCE(hold_reason,'') FROM runs WHERE id=$1`, f.runID).Scan(&held); err != nil || held != "paused/credential_disabled" {
				t.Fatalf("run=%q err=%v, want it left held", held, err)
			}
		})
	}
}

// Numbers are stored as posted. A near-cap body of 1e131071 literals must not expand in the
// table CHECK (as jsonb, each literal becomes 131 KB of digits and the cast took Postgres down):
// it is stored byte for byte, quickly, with no database error. A literal past the length bound
// is refused as a client error.
func TestPublicationResumeNumberLiteralLiveDB(t *testing.T) {
	cases := []struct {
		name   string
		extra  string
		stored bool
	}{
		{"exponent flood near the cap", `,"mr":{"n":[` + strings.Repeat("1e131071,", (MaxPublicationIntentBytes-600)/9) + `1]}`, true},
		{"short literals", `,"mr":{"n":[1e5,1.50,-0.0]}`, true},
		{"literal past the bound", `,"mr":{"n":` + strings.Repeat("9", 65) + `}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationResumeFixture(t)
			ctx := f.env.ctx
			raw := json.RawMessage(`{"run_id":"` + f.runID.String() + `","repo_id":"` + f.repoID.String() + `","branch":"agent/issue-1","claim_generation":1,` +
				`"original_head":"` + pubHeadA + `","floor":{"kind":"absent"},"candidate":null` + tc.extra + `}`)
			if len(raw) > MaxPublicationIntentBytes {
				t.Fatalf("fixture is %d bytes, past the cap", len(raw))
			}
			start := time.Now()
			_, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID, PublicationIntentRequest{ClaimGeneration: 1, Intent: raw})
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("took %s", d)
			}
			if !tc.stored {
				if !errors.Is(err, ErrInvalidState) {
					t.Fatalf("err=%v, want ErrInvalidState", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			var got string
			if err = f.env.pool.QueryRow(ctx, `SELECT intent::text FROM run_publication_resume WHERE run_id=$1`, f.runID).Scan(&got); err != nil || got != string(raw) {
				t.Fatalf("stored differs from the posted bytes (len %d vs %d) err=%v", len(got), len(raw), err)
			}
		})
	}
}

// Deleting a run cascades its enrollment without the opt-in, while a direct delete is refused.
func TestPublicationResumeRunDeleteCascadesLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	f.enroll(t)
	f.env.exec(`DELETE FROM runs WHERE id=$1`, f.runID)
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_publication_resume WHERE run_id=$1`, f.runID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after run delete=%d err=%v", n, err)
	}
}

func TestPublicationResumeFeatureOffRefusesNewEnrollmentLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	ctx := f.env.ctx
	f.enroll(t)
	f.svc.p.PublicationResume = false
	// Existing enrollment keeps its meaning: idempotent compare and candidate writes still work.
	if got := f.enroll(t); got.EnrollmentSeq != 1 {
		t.Fatalf("existing=%+v", got)
	}
	c := pubHeadC
	if _, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, EnrollmentSeq: seq(1), CandidateSeq: seq(1), Intent: f.intent("agent/issue-1", pubHeadA, &c)}); err != nil {
		t.Fatalf("candidate update with feature off: %v", err)
	}
	// A run with no active enrollment is refused.
	other := f.seedRun("running", 1)
	f.env.exec("UPDATE runs SET worker_id=$2, branch='agent/issue-2' WHERE id=$1", other, f.workerID)
	body, _ := json.Marshal(map[string]any{"run_id": other, "repo_id": f.repoID, "branch": "agent/issue-2", "claim_generation": 1,
		"original_head": pubHeadA, "floor": map[string]any{"kind": "absent"}, "candidate": nil})
	_, err := f.svc.RegisterPublicationIntent(ctx, f.wkr, other, PublicationIntentRequest{ClaimGeneration: 1, Intent: body})
	if !errors.Is(err, ErrPublicationResumeDisabled) {
		t.Fatalf("err=%v", err)
	}
	var n int
	if err := f.env.pool.QueryRow(ctx, `SELECT count(*) FROM run_publication_resume WHERE run_id=$1`, other).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows=%d err=%v", n, err)
	}
}

func TestPublicationResumeIncapableWorkerRefusedLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, []string{capability.RecoveryArchiveV1})
	_, err := f.svc.RegisterPublicationIntent(f.env.ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, Intent: f.intent("agent/issue-1", pubHeadA, nil)})
	if !errors.Is(err, ErrPublicationResumeUnsupported) {
		t.Fatalf("incapable worker err=%v, want ErrPublicationResumeUnsupported", err)
	}
	// A request generation the run is not at is stale. The body is consistent (generation 2 in both),
	// so only the live fence against runs.claim_generation can refuse it.
	f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, f.wkr.ProtocolCapabilities)
	_, err = f.svc.RegisterPublicationIntent(f.env.ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 2, Intent: f.intentAt(2, "agent/issue-1", pubHeadA, nil)})
	if !errors.Is(err, ErrStaleClaim) {
		t.Fatalf("generation mismatch err=%v, want ErrStaleClaim", err)
	}
	var rows int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_publication_resume WHERE run_id=$1`, f.runID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows=%d err=%v", rows, err)
	}
}

// The claim of an enrolled run: a capable worker on the updated API succeeds and receives the
// intent; a worker without the capability is filtered; an API that never set the opt-in aborts.
func TestPublicationResumeClaimFenceLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	ctx := f.env.ctx
	f.enroll(t)
	f.env.exec("UPDATE runs SET status='queued' WHERE id=$1", f.runID)
	capable := append([]string{capability.TaskCheckpointCoordinatesV1}, f.wkr.ProtocolCapabilities...)
	paramsFor := func(caps []string) store.ClaimRunParams {
		p := coordinateClaimParams(f.custodyContinuationFixture)
		p.WorkerProtocolCaps = caps
		return p
	}
	begin := func() pgx.Tx {
		tx, err := f.env.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(ctx) })
		if _, err := store.New(tx).GetWorkerForUpdate(ctx, f.workerID); err != nil {
			t.Fatal(err)
		}
		return tx
	}

	t.Run("incapable worker is filtered", func(t *testing.T) {
		tx := begin()
		defer func() { _ = tx.Rollback(ctx) }()
		_, err := store.New(tx).ClaimRun(ctx, paramsFor([]string{capability.RecoveryArchiveV1}))
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("claim by incapable worker err=%v, want no rows", err)
		}
	})
	t.Run("old API without the opt-in aborts", func(t *testing.T) {
		tx := begin()
		defer func() { _ = tx.Rollback(ctx) }()
		_, err := store.New(tx).ClaimRun(ctx, paramsFor(capable))
		requireCoordinateSQLState(t, err, "23514")
	})
	t.Run("opt-in without persisted capability aborts", func(t *testing.T) {
		f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, []string{capability.RecoveryArchiveV1})
		defer f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, f.wkr.ProtocolCapabilities)
		tx := begin()
		defer func() { _ = tx.Rollback(ctx) }()
		_, err := f.svc.claimCheckpointRun(ctx, tx, paramsFor(capable))
		requireCoordinateSQLState(t, err, "23514")
	})
	t.Run("bodyless claim skips an enrolled run", func(t *testing.T) {
		p := paramsFor(capable)
		p.SkipPublicationResume = true
		_, err := f.env.q.ClaimRun(ctx, p)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("bodyless claim err=%v, want no rows", err)
		}
	})
	t.Run("capable worker on the updated API claims and receives the intent", func(t *testing.T) {
		tx := begin()
		run, err := f.svc.claimCheckpointRun(ctx, tx, paramsFor(capable))
		if err != nil || run.ID != f.runID || run.ClaimGeneration != 2 {
			t.Fatalf("claim=%+v err=%v", run, err)
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		payload, err := f.svc.publicationResumePayload(ctx, f.wkr, run)
		if err != nil || payload == nil || payload.EnrollmentSeq != 1 {
			t.Fatalf("payload=%+v err=%v", payload, err)
		}
		var intent map[string]any
		if err = json.Unmarshal(payload.Intent, &intent); err != nil || intent["original_head"] != pubHeadA {
			t.Fatalf("intent=%s err=%v", payload.Intent, err)
		}
		if string(payload.Handoff) != "" {
			t.Fatalf("handoff=%s", payload.Handoff)
		}
		wire, err := json.Marshal(ClaimPayload{PublicationResume: payload})
		if err != nil || !strings.Contains(string(wire), `"publication_resume":{"enrollment_seq":1,"candidate_seq":0,"intent":{`) ||
			!strings.Contains(string(wire), `"handoff":null`) {
			t.Fatalf("wire=%s err=%v", wire, err)
		}
		// An incapable worker never sees it.
		none, err := f.svc.publicationResumePayload(ctx, store.Worker{ProtocolCapabilities: []string{capability.RecoveryArchiveV1}}, run)
		if err != nil || none != nil {
			t.Fatalf("incapable payload=%+v err=%v", none, err)
		}
	})
}

func TestPublicationResumeNormalizerLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	f.enroll(t)
	f.env.exec(`UPDATE runs SET status='recovery_wait', recovery_wait_cause='forge_unreachable',
		recovery_wait_phase='publication', status_since=now() WHERE id=$1`, f.runID)
	f.env.exec(`UPDATE run_publication_resume SET hold_reason='publication_retry_exhausted' WHERE run_id=$1`, f.runID)
	var phase *string
	var hold *string
	read := func() {
		t.Helper()
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT r.recovery_wait_phase, p.hold_reason FROM runs r
			JOIN run_publication_resume p ON p.run_id=r.id AND p.state='active' WHERE r.id=$1`, f.runID).Scan(&phase, &hold); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if phase == nil || hold == nil {
		t.Fatalf("setup phase=%v hold=%v", phase, hold)
	}
	// An older writer leaves recovery_wait without knowing either column.
	f.env.exec(`UPDATE runs SET status='queued' WHERE id=$1`, f.runID)
	read()
	if phase != nil || hold != nil {
		t.Fatalf("after leaving recovery_wait phase=%v hold=%v", phase, hold)
	}
	// A different cause also clears them.
	f.env.exec(`UPDATE runs SET status='recovery_wait', recovery_wait_cause='forge_unreachable', recovery_wait_phase='publication' WHERE id=$1`, f.runID)
	f.env.exec(`UPDATE run_publication_resume SET hold_reason='publication_model_required' WHERE run_id=$1`, f.runID)
	f.env.exec(`UPDATE runs SET recovery_wait_cause='vault_locked' WHERE id=$1`, f.runID)
	read()
	if phase != nil || hold != nil {
		t.Fatalf("after cause change phase=%v hold=%v", phase, hold)
	}
	// The CHECK admits only the publication phase.
	if _, err := f.env.pool.Exec(f.env.ctx, `UPDATE runs SET status='recovery_wait', recovery_wait_cause='forge_unreachable', recovery_wait_phase='x' WHERE id=$1`, f.runID); err == nil {
		t.Fatal("unknown phase accepted")
	}
}

func TestPublicationResumeDownRefusesEnrolledLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	ctx := f.env.ctx
	f.enroll(t)
	migration, err := os.ReadFile("../store/migrations/00386_publication_resume.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(migration), "-- +goose Down\n")
	if !ok {
		t.Fatal("missing production Down script")
	}
	run := func(iso pgx.TxIsoLevel) error {
		tx, err := f.env.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err = tx.Exec(ctx, "SELECT count(*) FROM runs"); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, down)
		return err
	}
	if err := run(pgx.ReadCommitted); err == nil || !strings.Contains(err.Error(), "cannot downgrade with enrolled publication resume") {
		t.Fatalf("active row Down err=%v", err)
	}
	if err := run(pgx.RepeatableRead); err == nil || !strings.Contains(err.Error(), "requires READ COMMITTED") {
		t.Fatalf("strict-snapshot Down err=%v", err)
	}
	var retained bool
	if err := f.env.pool.QueryRow(ctx, `SELECT to_regclass('run_publication_resume') IS NOT NULL
		AND to_regprocedure('guard_publication_resume()') IS NOT NULL`).Scan(&retained); err != nil || !retained {
		t.Fatalf("refused Down changed protected schema: retained=%v err=%v", retained, err)
	}
}

func TestPublicationResumeCompleteLiveDB(t *testing.T) {
	f := newPublicationResumeFixture(t)
	ctx := f.env.ctx
	f.enroll(t)
	if _, err := f.env.q.CompletePublicationResume(ctx, f.runID); err == nil {
		t.Fatal("completion without the opt-in succeeded")
	}
	tx, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err = q.OptInPublicationResume(ctx); err != nil {
		t.Fatal(err)
	}
	done, err := q.CompletePublicationResume(ctx, f.runID)
	if err != nil || done.State != "completed" {
		t.Fatalf("complete=%+v err=%v", done, err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.env.q.GetActivePublicationResume(ctx, f.runID); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("active row after completion: %v", err)
	}
}
