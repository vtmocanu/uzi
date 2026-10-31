package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestForgeParkRefusalReasonPublicationEnrolled(t *testing.T) {
	for _, err := range []error{workersvc.ErrPublicationEnrolled, fmt.Errorf("park: %w", workersvc.ErrPublicationEnrolled)} {
		if reason, ok := forgeParkRefusalReason(err); !ok || reason != "publication_enrolled" {
			t.Errorf("forgeParkRefusalReason(%v) = (%q,%v), want (publication_enrolled,true)", err, reason, ok)
		}
	}
	if reason, _ := forgeParkRefusalReason(workersvc.ErrForgeParkCustodyUnsettled); reason != "custody_unsettled" {
		t.Errorf("custody token changed: %q", reason)
	}
	if errors.Is(workersvc.ErrPublicationEnrolled, workersvc.ErrForgeParkCustodyUnsettled) {
		t.Error("the sentinels must be distinct")
	}
}

func TestShortObjectID(t *testing.T) {
	if got := shortObjectID("0123456789abcdef0123"); got != "0123456789ab" {
		t.Fatalf("long id: %q", got)
	}
	if got := shortObjectID("abc"); got != "abc" {
		t.Fatalf("short id: %q", got)
	}
}

// The owner Resume of a publication-phase hold dispatches to its own writer, and the run DTO
// carries the phase and the intent summary while the intent is active.
func TestPublicationHoldResumeAndDTOLiveDB(t *testing.T) {
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
	defer pool.Close()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	owner := store.User{ID: uuid.New()}
	connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, owner.ID, fmt.Sprintf("pubresume-%s@e2e", owner.ID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner.ID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	exec(`INSERT INTO workers (id,user_id,name,token_hash,protocol_capabilities) VALUES ($1,$2,'pub-resume',$3,$4)`,
		workerID, owner.ID, workerID[:], []string{capability.PublicationResumeV1})
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, started_at, worker_id, claim_generation, budget_wall_seconds, mr_iid, branch)
	      VALUES ($1,$2,$3,7,'t','d','running','issue', now() - interval '1 hour', $4, 1, 86400, 42, 'agent/issue-7')`, runID, owner.ID, repoID, workerID)
	intent, _ := json.Marshal(map[string]any{
		"run_id": runID, "repo_id": repoID, "branch": "agent/issue-7", "claim_generation": 1,
		"original_head": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "floor": map[string]any{"kind": "absent"},
		"candidate": "cccccccccccccccccccccccccccccccccccccccc",
	})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.New(tx).OptInPublicationResume(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, intent, state) VALUES ($1,1,1,$2,'active')`, runID, intent); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// An owner hold of the publication phase, as the park writer leaves it.
	exec(`UPDATE runs SET status='recovery_wait', status_since=now(), recovery_wait_cause='forge_unreachable',
	      recovery_wait_phase='publication', recovery_retry_not_before=NULL WHERE id=$1`, runID)
	exec(`UPDATE run_publication_resume SET wait_started_at=now()-interval '2 hours', first_wait_at=now()-interval '3 hours',
	      park_total=4, hold_reason='publication_retry_exhausted' WHERE run_id=$1`, runID)

	q := store.New(pool)
	svc := workersvc.New(q, nil, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), RunTimeout: 2 * time.Hour})
	svc.SetTxBeginner(pool)
	h := &Handler{q: q, wsvc: svc}
	h.cfg.RunProviderParkMaxDuration = time.Hour
	h.cfg.RunTimeout = 2 * time.Hour

	// The DTO of the held run.
	run, err := q.GetRunByID(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	dto := runToDTO(run, "", h.cfg.RunTimeout, 0, 0, time.Now(), 0)
	h.overlayRunPublicationIntents(mw.ContextWithUser(ctx, owner), &dto)
	if dto.RecoveryWaitPhase == nil || *dto.RecoveryWaitPhase != "publication" {
		t.Fatalf("phase=%v", dto.RecoveryWaitPhase)
	}
	if dto.PublicationHoldReason == nil || *dto.PublicationHoldReason != "publication_retry_exhausted" ||
		dto.PublicationParkTotal == nil || *dto.PublicationParkTotal != 4 || dto.PublicationEpisode == nil || *dto.PublicationEpisode != 0 ||
		dto.PublicationWaitStartedAt == nil || dto.PublicationFirstWaitAt == nil ||
		dto.PublicationWaitLimitSeconds == nil || *dto.PublicationWaitLimitSeconds != 3600 {
		t.Fatalf("diagnostics: %+v", dto)
	}
	in := dto.PublicationIntent
	if in == nil || in.Branch != "agent/issue-7" || in.OriginalHead != "aaaaaaaaaaaa" || in.Candidate == nil || *in.Candidate != "cccccccccccc" ||
		in.State != "active" || in.EnrollmentSeq != 1 || in.MrIID == nil || *in.MrIID != 42 {
		t.Fatalf("intent=%+v", in)
	}
	// Another viewer sees none of it.
	foreign := runToDTO(run, "", h.cfg.RunTimeout, 0, 0, time.Now(), 0)
	h.overlayRunPublicationIntents(mw.ContextWithUser(ctx, store.User{ID: uuid.New()}), &foreign)
	if foreign.PublicationIntent != nil || foreign.PublicationHoldReason != nil {
		t.Fatalf("foreign viewer got publication data: %+v", foreign.PublicationIntent)
	}

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/runs/"+runID.String()+"/resume-now", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), owner), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.ResumeRunNow(rec, req)
		return rec
	}
	rec := call()
	if rec.Code != http.StatusOK {
		t.Fatalf("resume=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Run.Status != "queued" || body.Run.RecoveryWaitPhase != nil || body.Run.RecoveryWaitCause != nil {
		t.Fatalf("resumed run: %+v err=%v", body.Run, err)
	}
	var episode int
	var waitStarted *time.Time
	var state string
	var parkTotal int
	if err := pool.QueryRow(ctx, `SELECT episode, wait_started_at, state, park_total FROM run_publication_resume WHERE run_id=$1`, runID).Scan(&episode, &waitStarted, &state, &parkTotal); err != nil ||
		episode != 1 || waitStarted != nil || state != "active" || parkTotal != 4 {
		t.Fatalf("side: episode=%d waitStarted=%v state=%s total=%d err=%v", episode, waitStarted, state, parkTotal, err)
	}
	// It is not a hold any more: a second Resume is a conflict.
	if rec := call(); rec.Code != http.StatusConflict {
		t.Fatalf("second resume=%d", rec.Code)
	}
}
