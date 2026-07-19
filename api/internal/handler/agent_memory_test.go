package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// memoryStore is a minimal workersvc.Store for the agent-memory worker-protocol
// HTTP tests: it embeds the interface (unused methods panic) and overrides only the
// queries the save/list paths reach, capturing the params so a test can prove the
// (user_id, repo_id) came off the OWNED RUN and never the request body.
type memoryStore struct {
	workersvc.Store
	ownedRun store.Run
	ownedErr error

	runCount int64 // CountAgentMemoryForRun return

	insertParams store.InsertAgentMemoryParams
	inserted     store.AgentMemory
	insertErr    error

	evictParams store.EvictAgentMemoryOverCapParams
	evictCalled bool

	listParams store.ListAgentMemoryForUserRepoParams
	listRows   []store.AgentMemory
}

func (m *memoryStore) GetRunOwnedByWorker(_ context.Context, _ store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return m.ownedRun, m.ownedErr
}
func (m *memoryStore) CountAgentMemoryForRun(context.Context, pgtype.UUID) (int64, error) {
	return m.runCount, nil
}
func (m *memoryStore) InsertAgentMemory(_ context.Context, arg store.InsertAgentMemoryParams) (store.AgentMemory, error) {
	m.insertParams = arg
	if m.insertErr != nil {
		return store.AgentMemory{}, m.insertErr
	}
	m.inserted = store.AgentMemory{
		ID:        uuid.New(),
		UserID:    arg.UserID,
		RepoID:    arg.RepoID,
		RunID:     arg.RunID,
		Title:     arg.Title,
		Body:      arg.Body,
		CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}
	return m.inserted, nil
}
func (m *memoryStore) EvictAgentMemoryOverCap(_ context.Context, arg store.EvictAgentMemoryOverCapParams) error {
	m.evictCalled = true
	m.evictParams = arg
	return nil
}
func (m *memoryStore) ListAgentMemoryForUserRepo(_ context.Context, arg store.ListAgentMemoryForUserRepoParams) ([]store.AgentMemory, error) {
	m.listParams = arg
	return m.listRows, nil
}

// runWithRepo builds an owned run whose (user_id, repo_id) are the identity the
// server must derive — deliberately NOT anything a request body could set.
func runWithRepo(userID, repoID uuid.UUID) store.Run {
	return store.Run{
		ID:     uuid.New(),
		UserID: userID,
		RepoID: pgtype.UUID{Bytes: repoID, Valid: true},
	}
}

func TestWorkerSaveMemoryDerivesIdentityFromRunClaim(t *testing.T) {
	userID, repoID := uuid.New(), uuid.New()
	st := &memoryStore{ownedRun: runWithRepo(userID, repoID)}
	h := newProtocolHandler(t, st)
	runID := uuid.New()

	rec := httptest.NewRecorder()
	// The body carries ONLY {title, body} — no identity fields exist on the wire type.
	h.WorkerSaveMemory(rec, workerReq(http.MethodPost, `{"title":"build flag","body":"use -tags foo"}`, runID))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body %q", rec.Code, rec.Body.String())
	}
	if st.insertParams.UserID != userID {
		t.Errorf("insert user_id = %s, want the run's owner %s (must derive from the claim)", st.insertParams.UserID, userID)
	}
	if st.insertParams.RepoID != repoID {
		t.Errorf("insert repo_id = %s, want the run's repo %s (must derive from the claim)", st.insertParams.RepoID, repoID)
	}
	if !st.insertParams.RunID.Valid || uuid.UUID(st.insertParams.RunID.Bytes) != runID {
		t.Errorf("insert run_id = %v, want the claimed run %s (provenance)", st.insertParams.RunID, runID)
	}
	if !st.evictCalled || st.evictParams.KeepCount != workersvc.MemoryMaxPerUserRepo {
		t.Errorf("eviction must run keeping the newest %d, got called=%v keep=%d", workersvc.MemoryMaxPerUserRepo, st.evictCalled, st.evictParams.KeepCount)
	}
	// The write echo is the bare entry — no repo_id/repo_name/run_id keys.
	var echo map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &echo); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	for _, k := range []string{"id", "title", "body", "created_at"} {
		if _, ok := echo[k]; !ok {
			t.Errorf("write echo missing key %q, got %v", k, rec.Body.String())
		}
	}
	if _, ok := echo["repo_id"]; ok {
		t.Errorf("write echo must not leak repo_id, got %v", rec.Body.String())
	}
}

func TestWorkerSaveMemoryStripsControlChars(t *testing.T) {
	// A prompt-injected ANSI escape (ESC 0x1b, e.g. 0x1b then [31m) or a bare
	// control char (BEL 0x07) in the title/body must be stripped server-side BEFORE
	// storage, so it can never render raw when the owner runs `uzi memory list` (the CLI table
	// printer writes cell values verbatim). The body preserves its real \n and \t; the
	// single-line title keeps neither. Control bytes ride the wire JSON-escaped (a
	// strict decoder rejects raw C0 bytes in a string), as a hostile worker must encode
	// them.
	st := &memoryStore{ownedRun: runWithRepo(uuid.New(), uuid.New())}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	body := `{"title":"\u001b[31mred\u0007","body":"line1\nline2\t\u001b[0mtail\u0007"}`
	h.WorkerSaveMemory(rec, workerReq(http.MethodPost, body, uuid.New()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body %q", rec.Code, rec.Body.String())
	}
	if got, want := st.insertParams.Title, "[31mred"; got != want {
		t.Errorf("stored title = %q, want %q (ANSI escape + BEL stripped)", got, want)
	}
	if got, want := st.insertParams.Body, "line1\nline2\t[0mtail"; got != want {
		t.Errorf("stored body = %q, want %q (real newline + tab kept, ANSI escape + BEL stripped)", got, want)
	}
}

func TestWorkerSaveMemoryRejectsIdentityInBody(t *testing.T) {
	// A body that tries to smuggle a user_id/repo_id must be rejected outright (the
	// decoder forbids unknown fields), so identity can never be body-driven.
	st := &memoryStore{ownedRun: runWithRepo(uuid.New(), uuid.New())}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerSaveMemory(rec, workerReq(http.MethodPost, `{"title":"t","body":"b","user_id":"`+uuid.New().String()+`"}`, uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (a body carrying identity must be rejected)", rec.Code)
	}
}

func TestWorkerSaveMemoryRepolessRun409(t *testing.T) {
	// A repo-less run (chat/self-improve: runs.repo_id NULL) has no memory scope.
	st := &memoryStore{ownedRun: store.Run{ID: uuid.New(), UserID: uuid.New()}} // RepoID zero-value = invalid
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerSaveMemory(rec, workerReq(http.MethodPost, `{"title":"t","body":"b"}`, uuid.New()))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a repo-less run, body %q", rec.Code, rec.Body.String())
	}
}

func TestWorkerSaveMemoryOversizeBody400(t *testing.T) {
	st := &memoryStore{ownedRun: runWithRepo(uuid.New(), uuid.New())}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	big := strings.Repeat("x", workersvc.MemoryMaxBodyBytes+1)
	h.WorkerSaveMemory(rec, workerReq(http.MethodPost, `{"title":"t","body":"`+big+`"}`, uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an oversize body", rec.Code)
	}
}

func TestWorkerSaveMemoryWriteCap429(t *testing.T) {
	st := &memoryStore{ownedRun: runWithRepo(uuid.New(), uuid.New()), runCount: workersvc.MemoryMaxPerRun}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerSaveMemory(rec, workerReq(http.MethodPost, `{"title":"t","body":"b"}`, uuid.New()))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 at the per-run write cap", rec.Code)
	}
}

func TestWorkerSaveMemoryNotOwned404(t *testing.T) {
	st := &memoryStore{ownedErr: pgx.ErrNoRows}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerSaveMemory(rec, workerReq(http.MethodPost, `{"title":"t","body":"b"}`, uuid.New()))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when the worker does not own the run", rec.Code)
	}
}

func TestWorkerListMemoryScopedToRunUserRepo(t *testing.T) {
	userID, repoID := uuid.New(), uuid.New()
	writerRun := uuid.New()
	st := &memoryStore{
		ownedRun: runWithRepo(userID, repoID),
		listRows: []store.AgentMemory{{
			ID:        uuid.New(),
			UserID:    userID,
			RepoID:    repoID,
			RunID:     pgtype.UUID{Bytes: writerRun, Valid: true},
			Title:     "t",
			Body:      "b",
			CreatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}},
	}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerListMemory(rec, workerReq(http.MethodGet, "", uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if st.listParams.UserID != userID || st.listParams.RepoID != repoID {
		t.Errorf("list scoped to (%s,%s), want the run's (%s,%s)", st.listParams.UserID, st.listParams.RepoID, userID, repoID)
	}
	var env struct {
		Memories []map[string]json.RawMessage `json:"memories"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(env.Memories) != 1 {
		t.Fatalf("want 1 memory, got %d", len(env.Memories))
	}
	m := env.Memories[0]
	for _, k := range []string{"id", "title", "body", "run_id", "created_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("worker list entry missing key %q, got %v", k, rec.Body.String())
		}
	}
	if _, ok := m["repo_id"]; ok {
		t.Errorf("worker list entry must omit repo_id (the worker already knows the repo), got %v", rec.Body.String())
	}
}
