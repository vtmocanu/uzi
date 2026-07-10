package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
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

func newChatHandler(st workersvc.Store) *Handler {
	return &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{ChatMaxTurns: 50})}
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
