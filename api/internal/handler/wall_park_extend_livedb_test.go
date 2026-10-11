package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #2734: POST /api/runs/{id}/inputs kind=extend on a wall-parked run must answer 409 (with the
// refusal's own sentence) whenever ExtendAndResumeWallPark matches no row, never 202 resumed:true.
// Each workersvc sentinel needs its own arm in CreateRunInput's error switch (an unmapped one is a
// 500), so one test per sentinel catches a missing arm.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

type wallParkExtendCapFake struct {
	workersvc.Settings
	cap int
}

func (f wallParkExtendCapFake) RunExtensionCapSeconds(context.Context) (int, error) {
	return f.cap, nil
}

// wallParkRaceStore wraps the real queries and runs beforeExtend immediately before the
// ExtendAndResumeWallPark statement, i.e. after SubmitInput has already read the run. It stages the
// read-then-write race the 0-row refusal exists for, without touching production code.
type wallParkRaceStore struct {
	*store.Queries
	beforeExtend func()
}

func (s *wallParkRaceStore) ExtendAndResumeWallPark(ctx context.Context, arg store.ExtendAndResumeWallParkParams) (store.ExtendAndResumeWallParkRow, error) {
	if s.beforeExtend != nil {
		s.beforeExtend()
	}
	return s.Queries.ExtendAndResumeWallPark(ctx, arg)
}

type wallParkExtendHTTPEnv struct {
	t      *testing.T
	h      *Handler
	race   *wallParkRaceStore
	exec   func(sql string, args ...any)
	owner  store.User
	repoID uuid.UUID
	worker uuid.UUID
	iid    int64
	query  func(sql string, args ...any) rowScanner
}

type rowScanner interface{ Scan(dest ...any) error }

func newWallParkExtendHTTPEnv(t *testing.T, capSeconds int) *wallParkExtendHTTPEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	e := &wallParkExtendHTTPEnv{t: t, owner: store.User{ID: uuid.New()}, repoID: uuid.New(), worker: uuid.New()}
	e.exec = func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	e.query = func(sql string, args ...any) rowScanner { return pool.QueryRow(ctx, sql, args...) }
	connID := uuid.New()
	e.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, e.owner.ID, fmt.Sprintf("wallpark-%s@e2e", e.owner.ID))
	e.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, e.owner.ID, []byte{0x1})
	e.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	        VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, e.repoID, connID)
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`,
		e.worker, e.owner.ID, "w-"+e.worker.String(), e.worker[:])

	e.race = &wallParkRaceStore{Queries: store.New(pool)}
	svc := workersvc.New(e.race, nil, workersvc.Params{RunTimeout: 2 * time.Hour, RunWallCeiling: 8 * time.Hour})
	svc.SetHealthSettings(wallParkExtendCapFake{cap: capSeconds})
	e.h = &Handler{q: e.race.Queries, wsvc: svc}
	return e
}

// wallPark seeds a run parked at its wall budget (3600s budget, started 5h ago, parked 30m ago).
func (e *wallParkExtendHTTPEnv) wallPark(extension int) uuid.UUID {
	e.t.Helper()
	e.iid++
	id := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status,
		worker_id, hold_reason, budget_wall_seconds, budget_extension_seconds, started_at, status_since, claim_generation)
		VALUES ($1,$2,$3,'issue',$4,'t','d','paused',$5,'budget_exhausted',3600,$6,
		now()-interval '5 hours',now()-interval '30 minutes',1)`, id, e.owner.ID, e.repoID, e.iid, e.worker, extension)
	e.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'pause','wall')`, id)
	return id
}

func (e *wallParkExtendHTTPEnv) postExtend(id uuid.UUID, secs int) *httptest.ResponseRecorder {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.h.CreateRunInput(rec, inputReq(e.owner, id, fmt.Sprintf(`{"kind":"extend","body":"%d"}`, secs)))
	return rec
}

// assertRefused409 checks a 409 carrying the sentinel's sentence and that nothing was extended.
func (e *wallParkExtendHTTPEnv) assertRefused409(rec *httptest.ResponseRecorder, id uuid.UUID, wantMsg string, wantStatus string, wantExt int32) {
	e.t.Helper()
	if rec.Code != http.StatusConflict {
		e.t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if msg, _ := body["error"].(string); !strings.Contains(msg, wantMsg) {
		e.t.Fatalf("error = %q, want it to contain %q", msg, wantMsg)
	}
	var status string
	var ext int32
	if err := e.query(`SELECT status, budget_extension_seconds FROM runs WHERE id=$1`, id).Scan(&status, &ext); err != nil {
		e.t.Fatal(err)
	}
	if status != wantStatus || ext != wantExt {
		e.t.Fatalf("run after refusal = %s/ext %d, want %s/%d", status, ext, wantStatus, wantExt)
	}
	var audits int
	if err := e.query(`SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind IN ('extend','resume')`, id).Scan(&audits); err != nil {
		e.t.Fatal(err)
	}
	if audits != 0 {
		e.t.Fatalf("refused extend wrote %d extend/resume audit rows", audits)
	}
}

// TestWallParkExtendCapExhausted409LiveDB: the cap is per request (#2735): a request above it (here
// 57601 > 57600) answers 409 naming the per-extension maximum while the run stays paused (a refused
// extend once answered 202 resumed:true with the run still paused).
func TestWallParkExtendCapExhausted409LiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	id := e.wallPark(50000)
	rec := e.postExtend(id, 57601)
	e.assertRefused409(rec, id, "maximum per extension", "paused", 50000)
}

// TestWallParkExtendParkChanged409LiveDB drives workersvc.ErrWallParkChanged over HTTP: the owner
// pauses the run between SubmitInput's read and the extend statement (staged by the store wrapper).
func TestWallParkExtendParkChanged409LiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	id := e.wallPark(0)
	e.race.beforeExtend = func() { e.exec(`UPDATE runs SET hold_reason=NULL WHERE id=$1`, id) }
	rec := e.postExtend(id, 3600)
	e.assertRefused409(rec, id, workersvc.ErrWallParkChanged.Error(), "paused", 0)
}

// TestWallParkExtendCrossCheckIneligible409LiveDB drives workersvc.ErrWallParkCrossCheckIneligible
// over HTTP: a wall-parked plan cross-check whose cross_checks row has expired cannot resume.
func TestWallParkExtendCrossCheckIneligible409LiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	leadID, checkerID := uuid.New(), uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, repo_id, worker_id, kind, issue_iid, issue_title, issue_description,
		status, harness, plan_cross_check_required, claim_generation)
		VALUES ($1,$2,$3,$4,'issue',1,'t','d','running','claude',true,1)`, leadID, e.owner.ID, e.repoID, e.worker)
	e.exec(`INSERT INTO runs (id, user_id, repo_id, worker_id, kind, target_run_id, harness, report_only,
		budget_wall_seconds, status, hold_reason, claim_generation, issue_title, issue_description,
		started_at, status_since)
		VALUES ($1,$2,$3,$4,'cross_check',$5,'codex',true,1800,'paused','budget_exhausted',1,'check','check',
		now()-interval '5 hours',now()-interval '30 minutes')`, checkerID, e.owner.ID, e.repoID, e.worker, leadID)
	e.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
		required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,
		checker_run_id,checker_harness,created_at,deadline_at)
		VALUES ($1,'plan',1,1,'plan','[]','{}','{}','s',$2,'',$3,$4,'codex',
		now()-interval '2 hours',now()-interval '1 hour')`, leadID, strings.Repeat("a", 40), []byte("digest"), checkerID)
	e.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'pause','wall')`, checkerID)
	rec := e.postExtend(checkerID, 3600)
	e.assertRefused409(rec, checkerID, workersvc.ErrWallParkCrossCheckIneligible.Error(), "paused", 0)
}

// Issue #2750: kind=stop on a wall park that cannot Stop returns workersvc.ErrWallParkStopUnavailable,
// which used to fall through CreateRunInput's error switch to a 500.
func (e *wallParkExtendHTTPEnv) assertStopRefused409(id uuid.UUID) {
	e.t.Helper()
	rec := httptest.NewRecorder()
	e.h.CreateRunInput(rec, inputReq(e.owner, id, `{"kind":"stop"}`))
	e.assertRefused409(rec, id, workersvc.ErrWallParkStopUnavailable.Error(), "paused", 0)
	var stops int
	if err := e.query(`SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='stop'`, id).Scan(&stops); err != nil {
		e.t.Fatal(err)
	}
	if stops != 0 {
		e.t.Fatalf("refused stop wrote %d stop audit rows", stops)
	}
}

// TestWallParkStopNonMilestone409LiveDB: a non-milestone issue run has nothing to finalize.
func TestWallParkStopNonMilestone409LiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	e.assertStopRefused409(e.wallPark(0))
}

// TestWallParkStopFinalizeUsed409LiveDB: a milestone run with a completed milestone whose finalize
// allowance was already granted cannot Stop again.
func TestWallParkStopFinalizeUsed409LiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	id := e.wallPark(0)
	e.exec(`UPDATE runs SET milestones_frozen='[{"id":"m1","title":"one"},{"id":"m2","title":"two"}]',
		milestones_completed='["m1"]', budget_finalize_seconds=1800 WHERE id=$1`, id)
	e.assertStopRefused409(id)
}
