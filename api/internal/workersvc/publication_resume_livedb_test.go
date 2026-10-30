package workersvc

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	env.exec("UPDATE runs SET worker_id=$2, started_at=now() WHERE id=$1", id, f.workerID)
	return &publicationResumeFixture{custodyContinuationFixture: f, runID: id}
}

func (f *publicationResumeFixture) intent(branch, head string, candidate *string) json.RawMessage {
	body := map[string]any{
		"run_id": f.runID, "repo_id": f.repoID, "branch": branch, "claim_generation": 1,
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
	resp, err := f.svc.RegisterPublicationIntent(f.env.ctx, f.wkr, f.runID,
		PublicationIntentRequest{ClaimGeneration: 1, Intent: f.intent("agent/issue-1", pubHeadA, nil)})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	return resp
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
	_, err = f.env.pool.Exec(ctx, `INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, intent, state)
		VALUES ($1, 2, 1, '{}', 'active')`, f.runID)
	if err == nil {
		t.Fatal("insert without opt-in succeeded")
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
	// Identity is immutable even with the opt-in.
	sp, _ := tx.Begin(ctx)
	_, err = sp.Exec(ctx, `UPDATE run_publication_resume SET intent = jsonb_set(intent, '{branch}', '"other"') WHERE run_id=$1`, f.runID)
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
	next := f.enroll(t)
	if next.EnrollmentSeq != 2 || next.State != "active" {
		t.Fatalf("re-enroll=%+v", next)
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
	f.env.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", other, f.workerID)
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
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("err=%v", err)
	}
	// A stale claim generation is not this worker's run.
	f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, f.wkr.ProtocolCapabilities)
	body := f.intent("agent/issue-1", pubHeadA, nil)
	_, err = f.svc.RegisterPublicationIntent(f.env.ctx, f.wkr, f.runID, PublicationIntentRequest{ClaimGeneration: 2, Intent: body})
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("generation mismatch err=%v", err)
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
