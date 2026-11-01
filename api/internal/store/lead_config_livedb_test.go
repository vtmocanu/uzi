package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/leadcfg/leadcfgtest"
)

// Live-database tests of the run_lead_configs table and store.RecordRunLeadConfig (PRD #3244). Every
// scene seeds its own user, repo, worker and runs under fresh ids and asserts only on its own run ids,
// because the database is shared with other tests (rows are counted per run_id).

type lcScene struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *Queries
	user   uuid.UUID
	repo   uuid.UUID
	worker uuid.UUID
	iid    int64
	gen    int64 // next claim_generation for raw inserts
}

func newLCScene(t *testing.T) *lcScene {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	s := &lcScene{t: t, ctx: ctx, pool: pool, q: New(pool), user: uuid.New(), repo: uuid.New(), worker: uuid.New(), gen: 1000}
	conn := uuid.New()
	s.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, s.user, "lc-"+s.user.String()+"@e2e")
	s.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, s.user, []byte{0x1})
	s.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	        VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`, s.repo, conn, time.Now().UnixNano()%1_000_000_000, "g/lc-"+s.repo.String())
	s.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
	        VALUES ($1, $2, 'w', $3, 'online', now())`, s.worker, s.user, s.worker[:])
	t.Cleanup(func() {
		// Best effort: the runs cascade removes the lead-config rows.
		_, _ = pool.Exec(ctx, `DELETE FROM runs WHERE user_id = $1`, s.user)
		_, _ = pool.Exec(ctx, `DELETE FROM egress_profiles WHERE name LIKE $1`, "lc-"+s.user.String()[:8]+"%")
		_, _ = pool.Exec(ctx, `DELETE FROM workers WHERE user_id = $1`, s.user)
		_, _ = pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, s.user)
	})
	return s
}

func (s *lcScene) exec(sql string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(s.ctx, sql, args...); err != nil {
		s.t.Fatalf("exec %q: %v", sql, err)
	}
}

// extraWorker seeds a second worker, so a run can be held by someone other than the scene's worker.
func (s *lcScene) extraWorker() uuid.UUID {
	s.t.Helper()
	id := uuid.New()
	s.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at)
	        VALUES ($1, $2, 'w2', $3, 'online', now())`, id, s.user, id[:])
	return id
}

func (s *lcScene) nextIID() int64 { s.iid++; return s.iid }

// lcRun describes a seeded run; zero values give a running Claude issue run at generation 1 held by the
// scene's worker.
type lcRun struct {
	kind     string
	status   string
	harness  string
	gen      int64
	worker   *uuid.UUID
	released bool
	review   bool // a task run that reviews another task run
	egress   bool // bound to an egress profile
}

func (s *lcScene) run(o lcRun) uuid.UUID {
	s.t.Helper()
	if o.kind == "" {
		o.kind = "issue"
	}
	if o.status == "" {
		o.status = "running"
	}
	if o.harness == "" {
		o.harness = "claude"
	}
	if o.gen == 0 {
		o.gen = 1
	}
	id := uuid.New()
	var worker any = s.worker
	if o.worker != nil {
		worker = *o.worker
	}
	var released any
	if o.released {
		released = time.Now()
	}
	var egress any
	if o.egress {
		egress = uuid.New()
		s.exec(`INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, ARRAY['example.test'])`,
			egress, "lc-"+s.user.String()[:8]+"-"+id.String()[:8])
	}
	cols := `id, user_id, kind, status, harness, issue_title, issue_description, worker_id, claim_generation, claim_released_at, egress_profile_id`
	vals := []any{id, s.user, o.kind, o.status, o.harness, "t", "d", worker, o.gen, released, egress}
	extra := func(col string, v any) {
		cols += ", " + col
		vals = append(vals, v)
	}
	switch o.kind {
	case "issue", "self_improve":
		extra("repo_id", s.repo)
		extra("issue_iid", s.nextIID())
	case "ci_fix":
		extra("repo_id", s.repo)
		extra("pipeline_id", s.nextIID())
		extra("pipeline_ref", "agent/issue-"+id.String())
	case "prompt":
		extra("repo_id", s.repo)
	case "task":
		extra("repo_id", s.repo)
		extra("branch", "uzi/task/"+id.String())
		if o.review {
			target := s.run(lcRun{kind: "task", status: "completed"})
			extra("review_target_run_id", target)
		}
	case "mr_rework":
		target := s.run(lcRun{status: "completed"})
		extra("repo_id", s.repo)
		extra("pipeline_ref", "agent/issue-"+id.String())
		extra("mr_iid", s.nextIID())
		extra("target_run_id", target)
	case "chat":
	case "judge":
		target := s.run(lcRun{status: "completed"})
		extra("target_run_id", target)
	default:
		s.t.Fatalf("lcScene.run: unsupported kind %q", o.kind)
	}
	ph := ""
	for i := range vals {
		if i > 0 {
			ph += ", "
		}
		ph += fmt.Sprintf("$%d", i+1)
	}
	s.exec(`INSERT INTO runs (`+cols+`) VALUES (`+ph+`)`, vals...)
	return id
}

func (s *lcScene) count(run uuid.UUID) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(s.ctx, `SELECT count(*) FROM run_lead_configs WHERE run_id = $1`, run).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// dump returns every stored row of the run as JSON text, ordered by key, for column-for-column comparison.
func (s *lcScene) dump(run uuid.UUID) []string {
	s.t.Helper()
	rows, err := s.pool.Query(s.ctx, `SELECT row_to_json(c)::text FROM run_lead_configs c WHERE run_id = $1 ORDER BY claim_generation, phase, seq`, run)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, j)
	}
	return out
}

func txt(v string) pgtype.Text { return pgtype.Text{String: v, Valid: true} }

// reqOnly is the requested-only report of the default Claude case.
func (s *lcScene) reqOnly(run uuid.UUID, gen int64, phase string, seq int16) RecordRunLeadConfigParams {
	return RecordRunLeadConfigParams{
		RunID: run, WorkerID: s.worker, ClaimGeneration: gen, Phase: phase, PhaseEntry: 1, Seq: seq,
		RequestedModel: txt("opus"), RequestedModelSource: "user_default",
		RequestedEffort: txt("medium"), RequestedEffortMode: "explicit", RequestedEffortSource: "builtin",
	}
}

// ---- raw table inserts: vocabulary and constraints ----

type rawRow struct {
	harness                           string
	phase                             string
	entry, seq                        int
	gen                               int64
	reqModel, obsModel, obsModelEv    *string
	reqModelSrc                       string
	reqEffort, obsEffort, obsEffortEv *string
	mode, effortSrc                   string
	truncated                         bool
}

func sp(v string) *string { return &v }

func baseRow() rawRow {
	return rawRow{harness: "claude", phase: "plan", entry: 1, seq: 1, reqModel: sp("opus"), reqModelSrc: "user_default",
		reqEffort: sp("medium"), mode: "explicit", effortSrc: "builtin"}
}

// insertRaw inserts row r into the run under a fresh claim_generation unless the row pins one.
func (s *lcScene) insertRaw(run uuid.UUID, r rawRow) error {
	if r.gen == 0 {
		s.gen++
		r.gen = s.gen
	}
	_, err := s.pool.Exec(s.ctx, `INSERT INTO run_lead_configs
		(run_id, claim_generation, phase, phase_entry, seq, harness, requested_model, requested_model_source,
		 observed_model, observed_model_evidence, requested_effort, requested_effort_mode, requested_effort_source,
		 observed_effort, observed_effort_evidence, truncated)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		run, r.gen, r.phase, r.entry, r.seq, r.harness, r.reqModel, r.reqModelSrc, r.obsModel, r.obsModelEv,
		r.reqEffort, r.mode, r.effortSrc, r.obsEffort, r.obsEffortEv, r.truncated)
	return err
}

func constraintOf(err error) (code, name string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

func (s *lcScene) mustAccept(run uuid.UUID, label string, r rawRow) {
	s.t.Helper()
	if err := s.insertRaw(run, r); err != nil {
		s.t.Errorf("%s: want accepted, got %v", label, err)
	}
}

func (s *lcScene) mustReject(run uuid.UUID, label, constraint string, r rawRow) {
	s.t.Helper()
	err := s.insertRaw(run, r)
	code, name := constraintOf(err)
	if code != "23514" || name != constraint {
		s.t.Errorf("%s: want 23514 %s, got code=%q constraint=%q err=%v", label, constraint, code, name, err)
	}
}

func TestRunLeadConfigsVocabularyLiveDB(t *testing.T) {
	s := newLCScene(t)
	run := s.run(lcRun{})
	v := leadcfgtest.LoadContract(t).Vocabulary

	// Every allowed value of every enum column is accepted.
	for _, h := range v.Harnesses {
		r := baseRow()
		r.harness = h
		s.mustAccept(run, "harness "+h, r)
	}
	for _, p := range v.Phases {
		r := baseRow()
		r.phase = p
		s.mustAccept(run, "phase "+p, r)
	}
	for _, src := range v.ModelSources {
		r := baseRow()
		r.reqModelSrc = src
		s.mustAccept(run, "model source "+src, r)
	}
	for _, src := range v.EffortSources {
		r := baseRow()
		r.effortSrc = src
		s.mustAccept(run, "effort source "+src, r)
	}
	for _, lvl := range v.RequestedEfforts {
		r := baseRow()
		r.reqEffort = sp(lvl)
		s.mustAccept(run, "requested effort "+lvl, r)
	}
	r := baseRow()
	r.reqEffort, r.mode = nil, "runtime_default"
	s.mustAccept(run, "mode runtime_default", r)
	for _, ev := range v.ModelEvidence {
		r := baseRow()
		r.obsModelEv = sp(ev)
		if ev != "unknown" {
			r.obsModel = sp("claude-opus-4")
		}
		s.mustAccept(run, "model evidence "+ev, r)
	}
	for _, lvl := range v.ObservedEfforts {
		r := baseRow()
		r.obsEffort, r.obsEffortEv = sp(lvl), sp("claude_init")
		s.mustAccept(run, "observed effort "+lvl, r)
	}
	for _, ev := range v.EffortEvidence {
		r := baseRow()
		r.obsEffortEv = sp(ev)
		if ev != "unknown" {
			r.obsEffort = sp("high")
		}
		s.mustAccept(run, "effort evidence "+ev, r)
	}
	for _, b := range []int{1, 8} {
		r := baseRow()
		r.entry = b
		s.mustAccept(run, fmt.Sprintf("phase_entry %d", b), r)
	}
	for _, b := range []int{1, 31, 32} {
		r := baseRow()
		r.seq = b
		s.mustAccept(run, fmt.Sprintf("seq %d", b), r)
	}
	r = baseRow()
	r.seq, r.truncated = 32, true
	s.mustAccept(run, "truncated at seq 32", r)
	r = baseRow()
	r.reqModel = sp("m")
	s.mustAccept(run, "1 byte model", r)
	long := make([]byte, 100)
	for i := range long {
		long[i] = 'm'
	}
	r = baseRow()
	r.reqModel, r.obsModel, r.obsModelEv = sp(string(long)), sp(string(long)), sp("claude_init")
	s.mustAccept(run, "100 byte models", r)
	// Claude not-sent: a NULL observed effort with Claude evidence; requested-only; Claude SDK default.
	r = baseRow()
	r.obsEffortEv = sp("claude_init")
	s.mustAccept(run, "claude not sent (claude_init)", r)
	r.obsEffortEv = sp("claude_get_settings")
	s.mustAccept(run, "claude not sent (claude_get_settings)", r)
	r = baseRow()
	s.mustAccept(run, "requested only", r)
	r = baseRow()
	r.reqModel, r.reqModelSrc = nil, "builtin"
	s.mustAccept(run, "claude sdk default model", r)

	// One disallowed value per column is rejected by the column's named constraint.
	over := string(append(long, 'm'))
	for _, c := range []struct {
		label, constraint string
		mut               func(*rawRow)
	}{
		{"claim_generation -1", "run_lead_configs_claim_generation_check", func(r *rawRow) { r.gen = -1 }},
		{"phase", "run_lead_configs_phase_check", func(r *rawRow) { r.phase = "review" }},
		{"phase_entry 0", "run_lead_configs_phase_entry_check", func(r *rawRow) { r.entry = 0 }},
		{"phase_entry 9", "run_lead_configs_phase_entry_check", func(r *rawRow) { r.entry = 9 }},
		{"seq 0", "run_lead_configs_seq_check", func(r *rawRow) { r.seq = 0 }},
		{"seq 33", "run_lead_configs_seq_check", func(r *rawRow) { r.seq = 33 }},
		{"harness", "run_lead_configs_harness_check", func(r *rawRow) { r.harness = "gemini" }},
		{"requested_model empty", "run_lead_configs_requested_model_length_check", func(r *rawRow) { r.reqModel = sp("") }},
		{"requested_model 101 bytes", "run_lead_configs_requested_model_length_check", func(r *rawRow) { r.reqModel = sp(over) }},
		{"requested_model_source", "run_lead_configs_requested_model_source_check", func(r *rawRow) { r.reqModelSrc = "instance_default" }},
		{"observed_model empty", "run_lead_configs_observed_model_length_check", func(r *rawRow) { r.obsModel, r.obsModelEv = sp(""), sp("claude_init") }},
		{"observed_model 101 bytes", "run_lead_configs_observed_model_length_check", func(r *rawRow) { r.obsModel, r.obsModelEv = sp(over), sp("claude_init") }},
		{"observed_model_evidence", "run_lead_configs_observed_model_evidence_check", func(r *rawRow) { r.obsModel, r.obsModelEv = sp("m"), sp("claude_get_settings") }},
		{"requested_effort", "run_lead_configs_requested_effort_check", func(r *rawRow) { r.reqEffort = sp("none") }},
		{"requested_effort_mode", "run_lead_configs_requested_effort_mode_check", func(r *rawRow) { r.reqEffort, r.mode = nil, "auto" }},
		{"requested_effort_source", "run_lead_configs_requested_effort_source_check", func(r *rawRow) { r.effortSrc = "schedule_pin" }},
		{"observed_effort", "run_lead_configs_observed_effort_check", func(r *rawRow) { r.obsEffort, r.obsEffortEv = sp("persistent"), sp("claude_init") }},
		{"observed_effort_evidence", "run_lead_configs_observed_effort_evidence_check", func(r *rawRow) { r.obsEffort, r.obsEffortEv = sp("high"), sp("codex_thread_response") }},
		// The five table checks.
		{"effort_mode explicit without effort", "run_lead_configs_effort_mode_check", func(r *rawRow) { r.reqEffort = nil }},
		{"effort_mode runtime_default with effort", "run_lead_configs_effort_mode_check", func(r *rawRow) { r.mode = "runtime_default" }},
		{"requested_model NULL on codex", "run_lead_configs_requested_model_check", func(r *rawRow) { r.harness, r.reqModel, r.reqModelSrc = "codex", nil, "builtin" }},
		{"requested_model NULL, claude, not builtin", "run_lead_configs_requested_model_check", func(r *rawRow) { r.reqModel = nil }},
		{"observed model without evidence", "run_lead_configs_observed_model_pairing_check", func(r *rawRow) { r.obsModel = sp("m") }},
		{"observed model with unknown evidence", "run_lead_configs_observed_model_pairing_check", func(r *rawRow) { r.obsModel, r.obsModelEv = sp("m"), sp("unknown") }},
		{"claude_init evidence without model", "run_lead_configs_observed_model_pairing_check", func(r *rawRow) { r.obsModelEv = sp("claude_init") }},
		{"observed (high, NULL)", "run_lead_configs_observed_effort_pairing_check", func(r *rawRow) { r.obsEffort = sp("high") }},
		{"observed (high, unknown)", "run_lead_configs_observed_effort_pairing_check", func(r *rawRow) { r.obsEffort, r.obsEffortEv = sp("high"), sp("unknown") }},
		{"observed (NULL, codex_sent)", "run_lead_configs_observed_effort_pairing_check", func(r *rawRow) { r.obsEffortEv = sp("codex_sent") }},
		{"observed (NULL, codex_runtime_default)", "run_lead_configs_observed_effort_pairing_check", func(r *rawRow) { r.obsEffortEv = sp("codex_runtime_default") }},
		{"truncated with seq 31", "run_lead_configs_truncated_check", func(r *rawRow) { r.seq, r.truncated = 31, true }},
	} {
		r := baseRow()
		c.mut(&r)
		s.mustReject(run, c.label, c.constraint, r)
	}

	// A duplicate primary key is a 23505, not a check violation.
	pk := baseRow()
	pk.gen = 77
	s.mustAccept(run, "pk first", pk)
	if code, _ := constraintOf(s.insertRaw(run, pk)); code != "23505" {
		t.Errorf("duplicate key: want 23505, got %q", code)
	}
}

func TestRunLeadConfigsAppendOnlyAndCascadeLiveDB(t *testing.T) {
	s := newLCScene(t)
	run := s.run(lcRun{})
	other := s.run(lcRun{})
	s.mustAccept(run, "row", baseRow())
	s.mustAccept(other, "other run row", baseRow())

	_, err := s.pool.Exec(s.ctx, `UPDATE run_lead_configs SET observed_model = 'x', observed_model_evidence = 'claude_init' WHERE run_id = $1`, run)
	if code, _ := constraintOf(err); code != "55000" {
		t.Fatalf("UPDATE must be rejected by the append-only trigger with 55000, got %v", err)
	}
	_, err = s.pool.Exec(s.ctx, `UPDATE run_lead_configs SET truncated = truncated WHERE run_id = $1`, run)
	if code, _ := constraintOf(err); code != "55000" {
		t.Fatalf("a no-op UPDATE is rejected too, got %v", err)
	}
	if s.count(run) != 1 {
		t.Fatal("the rejected UPDATE must leave the row")
	}

	s.exec(`DELETE FROM runs WHERE id = $1`, run)
	if s.count(run) != 0 {
		t.Fatal("deleting the run must cascade to its lead-config rows")
	}
	if s.count(other) != 1 {
		t.Fatal("the cascade must not touch another run's rows")
	}
}

// ---- RecordRunLeadConfig: fence, duplicates ----

func TestRecordRunLeadConfigFenceMatrixLiveDB(t *testing.T) {
	s := newLCScene(t)
	parent := t
	stranger := s.extraWorker()
	tests := []struct {
		name   string
		run    lcRun
		mutate func(*RecordRunLeadConfigParams)
	}{
		{name: "stale generation", run: lcRun{gen: 2}, mutate: func(p *RecordRunLeadConfigParams) { p.ClaimGeneration = 1 }},
		{name: "future generation", run: lcRun{gen: 2}, mutate: func(p *RecordRunLeadConfigParams) { p.ClaimGeneration = 3 }},
		{name: "released claim", run: lcRun{released: true}},
		{name: "other worker", run: lcRun{}, mutate: func(p *RecordRunLeadConfigParams) { p.WorkerID = stranger }},
		{name: "run held by another worker", run: lcRun{worker: &stranger}},
		{name: "status completed", run: lcRun{status: "completed"}},
		{name: "status failed", run: lcRun{status: "failed"}},
		{name: "status cancelled", run: lcRun{status: "cancelled"}},
		{name: "status queued", run: lcRun{status: "queued"}},
		{name: "status paused", run: lcRun{status: "paused"}},
		{name: "kind chat", run: lcRun{kind: "chat"}},
		{name: "kind judge", run: lcRun{kind: "judge"}},
		{name: "review run", run: lcRun{kind: "task", review: true}},
		{name: "egress-bound run", run: lcRun{egress: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s.t = t
			defer func() { s.t = parent }()
			run := s.run(tc.run)
			gen := tc.run.gen
			if gen == 0 {
				gen = 1
			}
			p := s.reqOnly(run, gen, "plan", 1)
			if tc.mutate != nil {
				tc.mutate(&p)
			}
			_, err := s.q.RecordRunLeadConfig(s.ctx, p)
			if !errors.Is(err, ErrLeadConfigClaimNotCurrent) {
				t.Fatalf("want ErrLeadConfigClaimNotCurrent, got %v", err)
			}
			if n := s.count(run); n != 0 {
				t.Fatalf("nothing may be written, found %d rows", n)
			}
		})
	}

	// Admitted: every live status and every lead kind.
	for _, st := range []string{"claimed", "running", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
		run := s.run(lcRun{status: st})
		res, err := s.q.RecordRunLeadConfig(s.ctx, s.reqOnly(run, 1, "plan", 1))
		if err != nil || !res.Inserted || s.count(run) != 1 {
			t.Errorf("status %s: res=%+v err=%v", st, res, err)
		}
	}
	for _, kind := range leadcfgtest.LoadContract(t).Vocabulary.LeadLaneKinds {
		run := s.run(lcRun{kind: kind})
		res, err := s.q.RecordRunLeadConfig(s.ctx, s.reqOnly(run, 1, "plan", 1))
		if err != nil || !res.Inserted || s.count(run) != 1 {
			t.Errorf("kind %s: res=%+v err=%v", kind, res, err)
		}
	}
	// The harness is copied from the run, never taken from the request.
	cx := s.run(lcRun{harness: "codex"})
	p := s.reqOnly(cx, 1, "plan", 1)
	p.RequestedModel = txt("gpt-6.1-sol")
	if _, err := s.q.RecordRunLeadConfig(s.ctx, p); err != nil {
		t.Fatal(err)
	}
	var h string
	if err := s.pool.QueryRow(s.ctx, `SELECT harness FROM run_lead_configs WHERE run_id = $1`, cx).Scan(&h); err != nil || h != "codex" {
		t.Fatalf("harness copied from run: %q %v", h, err)
	}
}

func TestRecordRunLeadConfigExistingKeyAfterReleaseOrReclaimLiveDB(t *testing.T) {
	s := newLCScene(t)
	run := s.run(lcRun{})
	p := s.reqOnly(run, 1, "plan", 1)
	if res, err := s.q.RecordRunLeadConfig(s.ctx, p); err != nil || !res.Inserted {
		t.Fatalf("first write: %+v %v", res, err)
	}
	before := s.dump(run)

	s.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, run)
	if _, err := s.q.RecordRunLeadConfig(s.ctx, p); !errors.Is(err, ErrLeadConfigClaimNotCurrent) {
		t.Fatalf("identical retry after release: want claim_not_current, got %v", err)
	}
	s.exec(`UPDATE runs SET claim_released_at = NULL, claim_generation = 2 WHERE id = $1`, run)
	if _, err := s.q.RecordRunLeadConfig(s.ctx, p); !errors.Is(err, ErrLeadConfigClaimNotCurrent) {
		t.Fatalf("identical retry after reclaim: want claim_not_current, got %v", err)
	}
	after := s.dump(run)
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("stored row changed: %v -> %v", before, after)
	}
}

func TestRecordRunLeadConfigDuplicatesLiveDB(t *testing.T) {
	s := newLCScene(t)

	t.Run("identical duplicate", func(t *testing.T) {
		run := s.run(lcRun{})
		p := s.reqOnly(run, 1, "plan", 1)
		p.ObservedModel, p.ObservedModelEvidence = txt("claude-opus-4"), txt("claude_init")
		p.ObservedEffort, p.ObservedEffortEvidence = txt("high"), txt("claude_get_settings")
		first, err := s.q.RecordRunLeadConfig(s.ctx, p)
		if err != nil || !first.Inserted {
			t.Fatalf("first: %+v %v", first, err)
		}
		second, err := s.q.RecordRunLeadConfig(s.ctx, p)
		if err != nil || second.Inserted {
			t.Fatalf("identical duplicate: want Inserted=false and no error, got %+v %v", second, err)
		}
		if s.count(run) != 1 {
			t.Fatal("exactly one row")
		}
	})

	t.Run("requested only posted twice", func(t *testing.T) {
		run := s.run(lcRun{})
		p := s.reqOnly(run, 1, "plan", 1)
		for i := 0; i < 2; i++ {
			res, err := s.q.RecordRunLeadConfig(s.ctx, p)
			if err != nil || res.Inserted != (i == 0) {
				t.Fatalf("post %d: %+v %v", i, res, err)
			}
		}
	})

	t.Run("claude not sent posted twice", func(t *testing.T) {
		run := s.run(lcRun{})
		p := s.reqOnly(run, 1, "plan", 1)
		p.ObservedModel, p.ObservedModelEvidence = txt("claude-opus-4"), txt("claude_init")
		p.ObservedEffortEvidence = txt("claude_init") // observed_effort stays NULL: "not sent"
		for i := 0; i < 2; i++ {
			res, err := s.q.RecordRunLeadConfig(s.ctx, p)
			if err != nil || res.Inserted != (i == 0) {
				t.Fatalf("post %d: %+v %v", i, res, err)
			}
		}
	})

	t.Run("sdk default model posted twice", func(t *testing.T) {
		run := s.run(lcRun{})
		p := s.reqOnly(run, 1, "implement", 1)
		p.RequestedModel, p.RequestedModelSource = pgtype.Text{}, "builtin"
		for i := 0; i < 2; i++ {
			res, err := s.q.RecordRunLeadConfig(s.ctx, p)
			if err != nil || res.Inserted != (i == 0) {
				t.Fatalf("post %d: %+v %v", i, res, err)
			}
		}
	})

	// A body that differs in any compared column is a key conflict and the first row is kept.
	diffs := map[string]func(*RecordRunLeadConfigParams){
		"phase_entry":     func(p *RecordRunLeadConfigParams) { p.PhaseEntry = 2 },
		"truncated":       func(p *RecordRunLeadConfigParams) { p.Truncated = true },
		"requested_model": func(p *RecordRunLeadConfigParams) { p.RequestedModel = txt("sonnet") },
		"requested_model NULL": func(p *RecordRunLeadConfigParams) {
			p.RequestedModel, p.RequestedModelSource = pgtype.Text{}, "builtin"
		},
		"requested_model_source": func(p *RecordRunLeadConfigParams) { p.RequestedModelSource = "template" },
		"requested_effort":       func(p *RecordRunLeadConfigParams) { p.RequestedEffort = txt("high") },
		"requested_effort_mode": func(p *RecordRunLeadConfigParams) {
			p.RequestedEffort, p.RequestedEffortMode = pgtype.Text{}, "runtime_default"
		},
		"requested_effort_source": func(p *RecordRunLeadConfigParams) { p.RequestedEffortSource = "user_default" },
		"observed_model": func(p *RecordRunLeadConfigParams) {
			p.ObservedModel, p.ObservedModelEvidence = txt("m"), txt("claude_init")
		},
		"observed_effort": func(p *RecordRunLeadConfigParams) {
			p.ObservedEffort, p.ObservedEffortEvidence = txt("high"), txt("claude_init")
		},
		"observed_effort_evidence":                func(p *RecordRunLeadConfigParams) { p.ObservedEffortEvidence = txt("claude_init") },
		"observed_model_evidence NULL vs unknown": func(p *RecordRunLeadConfigParams) { p.ObservedModelEvidence = txt("unknown") },
	}
	parent := t
	for name, mut := range diffs {
		t.Run("differing "+name, func(t *testing.T) {
			s.t = t
			defer func() { s.t = parent }()
			run := s.run(lcRun{})
			first := s.reqOnly(run, 1, "plan", 1)
			if name == "truncated" {
				first.Seq = 32
			}
			if _, err := s.q.RecordRunLeadConfig(s.ctx, first); err != nil {
				t.Fatal(err)
			}
			before := s.dump(run)
			second := first
			mut(&second)
			if _, err := s.q.RecordRunLeadConfig(s.ctx, second); !errors.Is(err, ErrLeadConfigKeyConflict) {
				t.Fatalf("want ErrLeadConfigKeyConflict, got %v", err)
			}
			after := s.dump(run)
			if len(after) != 1 || after[0] != before[0] {
				t.Fatalf("first row must be kept: %v -> %v", before, after)
			}
		})
	}

	t.Run("unknown vs NULL the other way round", func(t *testing.T) {
		run := s.run(lcRun{})
		first := s.reqOnly(run, 1, "plan", 1)
		first.ObservedModelEvidence = txt("unknown")
		if _, err := s.q.RecordRunLeadConfig(s.ctx, first); err != nil {
			t.Fatal(err)
		}
		second := first
		second.ObservedModelEvidence = pgtype.Text{}
		if _, err := s.q.RecordRunLeadConfig(s.ctx, second); !errors.Is(err, ErrLeadConfigKeyConflict) {
			t.Fatalf("want ErrLeadConfigKeyConflict, got %v", err)
		}
	})
}

// ---- A3: the transaction seam ----

// leadConfigRecorder counts the calls a real querier receives and can fail statement 2.
type leadConfigRecorder struct {
	inner        leadConfigTxQuerier
	insertCalls  int
	sameCalls    int
	failSameWith error
}

func (r *leadConfigRecorder) InsertRunLeadConfigFenced(ctx context.Context, a InsertRunLeadConfigFencedParams) (InsertRunLeadConfigFencedRow, error) {
	r.insertCalls++
	return r.inner.InsertRunLeadConfigFenced(ctx, a)
}

func (r *leadConfigRecorder) RunLeadConfigSameBody(ctx context.Context, a RunLeadConfigSameBodyParams) (bool, error) {
	r.sameCalls++
	if r.failSameWith != nil {
		return false, r.failSameWith
	}
	return r.inner.RunLeadConfigSameBody(ctx, a)
}

// runWithRecorder runs recordRunLeadConfigTx on a fresh READ COMMITTED transaction.
func (s *lcScene) runWithRecorder(p RecordRunLeadConfigParams, rec *leadConfigRecorder) (RecordRunLeadConfigResult, error) {
	s.t.Helper()
	tx, err := s.pool.BeginTx(s.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		s.t.Fatal(err)
	}
	rec.inner = New(tx)
	return recordRunLeadConfigTx(s.ctx, tx, rec, p)
}

func TestRecordRunLeadConfigStatementTwoFailureLiveDB(t *testing.T) {
	s := newLCScene(t)
	run := s.run(lcRun{})
	p := s.reqOnly(run, 1, "plan", 1)
	if res, err := s.q.RecordRunLeadConfig(s.ctx, p); err != nil || !res.Inserted {
		t.Fatalf("commit through the real path: %+v %v", res, err)
	}
	before := s.dump(run)

	injected := errors.New("injected statement-2 failure")
	rec := &leadConfigRecorder{failSameWith: injected}
	if _, err := s.runWithRecorder(p, rec); !errors.Is(err, injected) {
		t.Fatalf("want the injected error, got %v", err)
	}
	if rec.insertCalls != 1 || rec.sameCalls != 1 {
		t.Fatalf("calls: insert=%d same=%d", rec.insertCalls, rec.sameCalls)
	}
	after := s.dump(run)
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("stored row changed column-for-column: %v -> %v", before, after)
	}
	if s.count(run) != 1 {
		t.Fatal("still the only row for the run")
	}
	// The run-row lock was released: a fenced insert of a new seq by the same worker must not block.
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	next := s.reqOnly(run, 1, "plan", 2)
	res, err := s.q.RecordRunLeadConfig(ctx, next)
	if err != nil || !res.Inserted {
		t.Fatalf("lock must be released after the failure: %+v %v", res, err)
	}
	if s.count(run) != 2 {
		t.Fatal("the new seq must land")
	}

	// ErrNoRows from statement 2 is the invariant breach, not a conflict.
	rec = &leadConfigRecorder{failSameWith: pgx.ErrNoRows}
	if _, err := s.runWithRecorder(p, rec); !errors.Is(err, ErrLeadConfigInvariant) {
		t.Fatalf("want ErrLeadConfigInvariant, got %v", err)
	}
}

func TestRecordRunLeadConfigStatementOneCheckViolationLiveDB(t *testing.T) {
	s := newLCScene(t)
	run := s.run(lcRun{harness: "codex"})
	p := s.reqOnly(run, 1, "plan", 1)
	p.RequestedModel, p.RequestedModelSource = pgtype.Text{}, "builtin" // a Codex root always sends a model
	rec := &leadConfigRecorder{}
	_, err := s.runWithRecorder(p, rec)
	if code, name := constraintOf(err); code != "23514" || name != "run_lead_configs_requested_model_check" {
		t.Fatalf("want a wrapped 23514 on run_lead_configs_requested_model_check, got %v", err)
	}
	if rec.sameCalls != 0 {
		t.Fatalf("statement 2 must never run after an insert error, ran %d times", rec.sameCalls)
	}
	if s.count(run) != 0 {
		t.Fatal("zero rows for the key")
	}
	// The same through the public method.
	if _, err := s.q.RecordRunLeadConfig(s.ctx, p); err == nil {
		t.Fatal("want the violation from the public path too")
	} else if code, name := constraintOf(err); code != "23514" || name != "run_lead_configs_requested_model_check" {
		t.Fatalf("public path: %v", err)
	}
	// The failed write released the lock.
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	ok := s.reqOnly(run, 1, "plan", 1)
	ok.RequestedModel = txt("gpt-6.1-sol")
	if res, err := s.q.RecordRunLeadConfig(ctx, ok); err != nil || !res.Inserted {
		t.Fatalf("after the violation: %+v %v", res, err)
	}
}

// ---- ListLatestRunLeadConfig ----

func TestListLatestRunLeadConfigLiveDB(t *testing.T) {
	s := newLCScene(t)
	run := s.run(lcRun{})
	other := s.run(lcRun{})
	write := func(r uuid.UUID, gen int64, phase string, entry, seq int16, model string) {
		t.Helper()
		p := s.reqOnly(r, gen, phase, seq)
		p.PhaseEntry, p.RequestedModel = entry, txt(model)
		if res, err := s.q.RecordRunLeadConfig(s.ctx, p); err != nil || !res.Inserted {
			t.Fatalf("write %s/%d/%d: %+v %v", phase, entry, seq, res, err)
		}
	}
	if rows, err := s.q.ListLatestRunLeadConfig(s.ctx, run); err != nil || len(rows) != 0 {
		t.Fatalf("no rows yet: %v %v", rows, err)
	}
	// Insertion order is deliberately implement before plan.
	write(run, 1, "implement", 1, 1, "impl-a")
	write(run, 1, "plan", 1, 1, "plan-a")
	write(run, 1, "plan", 1, 2, "plan-b")
	write(run, 1, "plan", 2, 3, "plan-c")
	write(other, 1, "plan", 1, 1, "other-run")

	rows, err := s.q.ListLatestRunLeadConfig(s.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Phase != "plan" || rows[1].Phase != "implement" {
		t.Fatalf("plan first, one row per phase: %+v", rows)
	}
	if rows[0].Seq != 3 || rows[0].PhaseEntry != 2 || rows[0].RequestedModel.String != "plan-c" {
		t.Fatalf("highest seq of the plan phase: %+v", rows[0])
	}
	if rows[1].Seq != 1 || rows[1].RequestedModel.String != "impl-a" {
		t.Fatalf("implement row: %+v", rows[1])
	}

	// A later generation hides the earlier one entirely, even for a phase it has no row for.
	s.exec(`UPDATE runs SET claim_generation = 2 WHERE id = $1`, run)
	write(run, 2, "implement", 1, 1, "impl-gen2")
	rows, err = s.q.ListLatestRunLeadConfig(s.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ClaimGeneration != 2 || rows[0].Phase != "implement" || rows[0].RequestedModel.String != "impl-gen2" {
		t.Fatalf("latest generation only: %+v", rows)
	}
	// The other run is unaffected.
	if rows, err = s.q.ListLatestRunLeadConfig(s.ctx, other); err != nil || len(rows) != 1 || rows[0].RequestedModel.String != "other-run" {
		t.Fatalf("other run: %+v %v", rows, err)
	}
	// The vocabulary a reader may see is exactly the leadcfg one (guards a drifted SQL list).
	for _, r := range rows {
		if !contains(leadcfgPhases(), r.Phase) {
			t.Errorf("phase %q outside the vocabulary", r.Phase)
		}
	}
}

func leadcfgPhases() []string {
	var out []string
	for _, p := range leadcfg.Phases() {
		out = append(out, string(p))
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
