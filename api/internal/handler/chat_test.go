package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// chatStore is a minimal workersvc.Store for the chat endpoint authz tests: it
// owns one chat run and one pending proposal and answers the queries those paths
// reach; everything else panics (embedded interface), so a test can't silently pass
// through an unexercised path.
type chatStore struct {
	workersvc.Store
	ownerID   uuid.UUID
	chatRun   store.Run
	followUps int64
	propID    uuid.UUID
	dismissed *uuid.UUID
}

func (s *chatStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID == s.chatRun.ID && arg.UserID == s.ownerID {
		return s.chatRun, nil
	}
	return store.Run{}, pgx.ErrNoRows
}
func (s *chatStore) CreateChatRun(_ context.Context, arg store.CreateChatRunParams) (store.Run, error) {
	return store.Run{ID: uuid.New(), UserID: arg.UserID, Kind: workersvc.RunKindChat, Status: "queued", IssueTitle: arg.IssueTitle}, nil
}
func (s *chatStore) CountChatFollowUps(context.Context, uuid.UUID) (int64, error) {
	return s.followUps, nil
}
func (s *chatStore) CreateRunInput(_ context.Context, arg store.CreateRunInputParams) (store.RunUserInput, error) {
	return store.RunUserInput{ID: 1, RunID: arg.RunID, Kind: arg.Kind}, nil
}
func (s *chatStore) GetChatProposalForConfirm(_ context.Context, arg store.GetChatProposalForConfirmParams) (store.GetChatProposalForConfirmRow, error) {
	if arg.ID == s.propID && arg.RunID == s.chatRun.ID && arg.UserID == s.ownerID {
		return store.GetChatProposalForConfirmRow{ID: s.propID, RunID: s.chatRun.ID, RepoID: uuid.New(), Title: "t", Description: "d", Status: "pending"}, nil
	}
	return store.GetChatProposalForConfirmRow{}, pgx.ErrNoRows
}
func (s *chatStore) MarkProposalDismissed(_ context.Context, id uuid.UUID) (store.IssueProposal, error) {
	s.dismissed = &id
	return store.IssueProposal{}, nil
}
func (s *chatStore) CreateChatContinueRun(_ context.Context, arg store.CreateChatContinueRunParams) (store.Run, error) {
	return store.Run{ID: uuid.New(), UserID: arg.UserID, Kind: workersvc.RunKindChat, Status: "queued"}, nil
}
func (s *chatStore) ListChatRunsForUser(_ context.Context, userID uuid.UUID) ([]store.ListChatRunsForUserRow, error) {
	if userID != s.ownerID {
		return nil, nil
	}
	return []store.ListChatRunsForUserRow{{
		ID: s.chatRun.ID, Title: pgtype.Text{String: "How does the plan gate work?", Valid: true},
		Status: "running", TurnCount: 3,
		LastMessageAt: pgtype.Timestamptz{Time: time.Unix(1700000000, 0), Valid: true},
	}}, nil
}

func newChatHandler(st workersvc.Store) *Handler {
	return &Handler{
		wsvc: workersvc.New(st, nil, workersvc.Params{ChatMaxTurns: 50}),
		cfg:  config.Config{ChatMaxTurns: 50}, // the ListChats envelope reads max_turns from cfg
	}
}

// chatReq builds a request authenticated as user with optional chi {id}/{pid}
// params and a JSON body.
func chatReq(method string, user store.User, id, pid uuid.UUID, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, "/api/chats/x", nil)
	} else {
		r = httptest.NewRequest(method, "/api/chats/x", strings.NewReader(body))
	}
	rctx := chi.NewRouteContext()
	if id != uuid.Nil {
		rctx.URLParams.Add("id", id.String())
	}
	if pid != uuid.Nil {
		rctx.URLParams.Add("pid", pid.String())
	}
	ctx := context.WithValue(mw.ContextWithUser(r.Context(), user), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

func TestListChatsShapeAndEnvelope(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: uuid.New(), UserID: owner.ID}}
	h := newChatHandler(st)

	rec := httptest.NewRecorder()
	h.ListChats(rec, chatReq(http.MethodGet, owner, uuid.Nil, uuid.Nil, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListChats = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Envelope carries the instance-wide turn cap.
	if !strings.Contains(body, `"max_turns":50`) {
		t.Fatalf("ListChats envelope must carry max_turns; got %s", body)
	}
	// The chat list DTO carries turn_count + last_message_at (the list's sort key).
	for _, want := range []string{`"turn_count":3`, `"last_message_at":`, `"title":"How does the plan gate work?"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("ListChats body missing %s; got %s", want, body)
		}
	}
}

func TestCreateChatRequiresAuthAndMessage(t *testing.T) {
	h := newChatHandler(&chatStore{})

	// Unauthenticated → 401.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chats", strings.NewReader(`{"message":"hi"}`))
	h.CreateChat(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated CreateChat = %d, want 401", rec.Code)
	}

	// Authenticated, empty message → 400.
	user := store.User{ID: uuid.New()}
	rec = httptest.NewRecorder()
	h.CreateChat(rec, chatReq(http.MethodPost, user, uuid.Nil, uuid.Nil, `{"message":"   "}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty-message CreateChat = %d, want 400", rec.Code)
	}

	// Authenticated, real message → 201.
	rec = httptest.NewRecorder()
	h.CreateChat(rec, chatReq(http.MethodPost, user, uuid.Nil, uuid.Nil, `{"message":"how does the plan gate work?"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("CreateChat = %d, want 201", rec.Code)
	}
}

func TestPostChatMessageOwnerOnly(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindChat, Status: "running"}}
	h := newChatHandler(st)

	// Owner posts a turn → 202.
	rec := httptest.NewRecorder()
	h.PostChatMessage(rec, chatReq(http.MethodPost, owner, runID, uuid.Nil, `{"message":"keep going"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("owner PostChatMessage = %d, want 202", rec.Code)
	}

	// A non-owner is denied (404, indistinguishable from an unknown chat).
	rec = httptest.NewRecorder()
	h.PostChatMessage(rec, chatReq(http.MethodPost, store.User{ID: uuid.New()}, runID, uuid.Nil, `{"message":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner PostChatMessage = %d, want 404", rec.Code)
	}
}

func TestPostChatMessageTurnCapConflict(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindChat, Status: "running"}, followUps: 50}
	h := newChatHandler(st)

	rec := httptest.NewRecorder()
	h.PostChatMessage(rec, chatReq(http.MethodPost, owner, runID, uuid.Nil, `{"message":"one more"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("turn-capped PostChatMessage = %d, want 409", rec.Code)
	}
}

func TestPostChatMessageTerminalConflict(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindChat, Status: "completed"}}
	h := newChatHandler(st)

	rec := httptest.NewRecorder()
	h.PostChatMessage(rec, chatReq(http.MethodPost, owner, runID, uuid.Nil, `{"message":"hi"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("terminal PostChatMessage = %d, want 409 (client shows Continue)", rec.Code)
	}
}

// TestContinueChatIsRateLimited asserts POST /api/chats/:id/continue rides the
// per-user chat limiter (it mints a new queued chat run, so repeated Continue must
// not bypass the create/messages spend guard). Drives the real chi route + real
// PerUserMiddleware with a 1-request budget: the second Continue is 429.
func TestContinueChatIsRateLimited(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	// A terminal source so ContinueChat proceeds to mint a run (201) rather than 409.
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindChat, Status: "completed"}}
	h := newChatHandler(st)

	lim := mw.NewLimiter(1, time.Minute, nil)
	router := chi.NewRouter()
	router.Route("/api/chats", func(r chi.Router) {
		r.With(lim.PerUserMiddleware).Post("/{id}/continue", h.ContinueChat)
	})

	do := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/chats/"+runID.String()+"/continue", nil)
		req = req.WithContext(mw.ContextWithUser(req.Context(), owner))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do(); code != http.StatusCreated {
		t.Fatalf("first continue = %d, want 201", code)
	}
	if code := do(); code != http.StatusTooManyRequests {
		t.Fatalf("second continue = %d, want 429 (chat limiter must apply to /continue)", code)
	}
}

func TestDismissProposalOwnerOnlyNeverForge(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID, propID := uuid.New(), uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindChat, Status: "running"}, propID: propID}
	h := newChatHandler(st)

	// Owner dismiss → 204, and the status-only flip ran (no forge — DismissProposal
	// holds no forge dependency).
	rec := httptest.NewRecorder()
	h.DismissProposal(rec, chatReq(http.MethodPost, owner, runID, propID, ""))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner DismissProposal = %d, want 204", rec.Code)
	}
	if st.dismissed == nil || *st.dismissed != propID {
		t.Fatalf("DismissProposal must flip the proposal status, dismissed=%v", st.dismissed)
	}

	// A non-owner (proposal scoped by user through the run) → 404.
	st.dismissed = nil
	rec = httptest.NewRecorder()
	h.DismissProposal(rec, chatReq(http.MethodPost, store.User{ID: uuid.New()}, runID, propID, ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner DismissProposal = %d, want 404", rec.Code)
	}
	if st.dismissed != nil {
		t.Fatal("a denied dismiss must not touch the proposal")
	}
}
