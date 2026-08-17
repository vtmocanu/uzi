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
	cancelled *uuid.UUID // the run id a server-side cancel flipped (nil until CancelRunServerSide runs)
	inputKind string     // the kind of the last enqueued run input (follow_up steer records here)
	inputBody string     // the body of the last enqueued run input (the steered follow-up message)
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
	// A follow_up steer (PRD #322 M3) enqueues here — record kind+body as the observable
	// that proves SubmitInput(follow_up) ran through with the human-edited message.
	s.inputKind = arg.Kind
	s.inputBody = arg.Body.String
	return store.RunUserInput{ID: 1, RunID: arg.RunID, Kind: arg.Kind}, nil
}

// CancelRunServerSide records the server-side cancel the queued-run (no live poller)
// path applies — the observable that proves SubmitInput(cancel) ran through.
func (s *chatStore) CancelRunServerSide(_ context.Context, arg store.CancelRunServerSideParams) (int64, error) {
	if arg.ID != s.chatRun.ID || arg.UserID != s.ownerID {
		return 0, nil
	}
	id := arg.ID
	s.cancelled = &id
	return 1, nil
}

// GetRunByID is the unscoped reload maybeEnqueueJudgeByID does after a committed
// terminal transition (best-effort; the judge gate then filters a cancelled run out).
func (s *chatStore) GetRunByID(_ context.Context, id uuid.UUID) (store.Run, error) {
	if id == s.chatRun.ID {
		return s.chatRun, nil
	}
	return store.Run{}, pgx.ErrNoRows
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

// TestCancelChatRunOwnerCancels asserts the happy path (PRD #322 M1): the owner
// cancels their own live (queued, no live poller) issue run → 202, and SubmitInput
// ran the cancel through server-side (CancelRunServerSide flipped the run).
func TestCancelChatRunOwnerCancels(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	// Queued → hasLivePoller is false without a worker lookup, so cancel applies
	// server-side (CancelRunServerSide), the observable we assert on.
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindIssue, Status: "queued"}}
	h := newChatHandler(st)

	rec := httptest.NewRecorder()
	h.CancelChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, `{"run_id":"`+runID.String()+`"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("owner CancelChatRun = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if st.cancelled == nil || *st.cancelled != runID {
		t.Fatalf("CancelChatRun must cancel the run via SubmitInput(cancel); cancelled=%v", st.cancelled)
	}
}

// TestCancelChatRunForeignRun asserts a forged/foreign run_id (a valid UUID the user
// does not own) is refused 404 — SubmitInput re-resolves ownership server-side, so the
// untrusted card value cannot cancel another user's run.
func TestCancelChatRunForeignRun(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindIssue, Status: "queued"}}
	h := newChatHandler(st)

	// A different, valid UUID the fake store does not own → GetRunByIDForUser 0 rows.
	foreign := uuid.New()
	rec := httptest.NewRecorder()
	h.CancelChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, `{"run_id":"`+foreign.String()+`"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-run CancelChatRun = %d, want 404", rec.Code)
	}
	if st.cancelled != nil {
		t.Fatal("a refused cancel must not touch any run")
	}
}

// TestCancelChatRunTerminalRun asserts an already-terminal run is refused 409.
func TestCancelChatRunTerminalRun(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindIssue, Status: "completed"}}
	h := newChatHandler(st)

	rec := httptest.NewRecorder()
	h.CancelChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, `{"run_id":"`+runID.String()+`"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("terminal-run CancelChatRun = %d, want 409", rec.Code)
	}
	if st.cancelled != nil {
		t.Fatal("a terminal run must not be cancelled")
	}
}

// TestCancelChatRunBadRequest asserts a missing/blank run_id (not a valid UUID) is a
// 400, and an unauthenticated caller is a 401 — both before any store call.
func TestCancelChatRunBadRequestAndAuth(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: uuid.New(), UserID: owner.ID}}
	h := newChatHandler(st)

	// Blank run_id → 400 (uuid.Parse fails on the empty string).
	rec := httptest.NewRecorder()
	h.CancelChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, `{"run_id":"   "}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank run_id CancelChatRun = %d, want 400", rec.Code)
	}

	// Unauthenticated → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chats/cancel-requests", strings.NewReader(`{"run_id":"x"}`))
	h.CancelChatRun(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated CancelChatRun = %d, want 401", rec.Code)
	}
}

// TestSteerChatRunOwnerSteers asserts the happy path (PRD #322 M3): the owner steers
// their own live issue run → 202, and SubmitInput enqueued a follow_up carrying the
// human-edited message (the observable that proves it ran through).
func TestSteerChatRunOwnerSteers(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindIssue, Status: "running"}}
	h := newChatHandler(st)

	rec := httptest.NewRecorder()
	body := `{"run_id":"` + runID.String() + `","message":"focus on the auth path"}`
	h.SteerChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, body))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("owner SteerChatRun = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	if st.inputKind != "follow_up" {
		t.Fatalf("SteerChatRun must enqueue a follow_up via SubmitInput; inputKind=%q", st.inputKind)
	}
	if st.inputBody != "focus on the auth path" {
		t.Fatalf("SteerChatRun must carry the edited message; inputBody=%q", st.inputBody)
	}
}

// TestSteerChatRunChatTarget asserts a follow_up against a CHAT run is refused 409 with
// the issue-runs-only copy — the ErrChatInputNotAllowed guard surfaces on the card.
func TestSteerChatRunChatTarget(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindChat, Status: "running"}}
	h := newChatHandler(st)

	rec := httptest.NewRecorder()
	body := `{"run_id":"` + runID.String() + `","message":"steer me"}`
	h.SteerChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("chat-run SteerChatRun = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "issue runs") {
		t.Fatalf("chat-run SteerChatRun must carry the issue-runs-only copy; body=%s", rec.Body.String())
	}
	if st.inputKind != "" {
		t.Fatal("a refused steer must not enqueue any input")
	}
}

// TestSteerChatRunForeignRun asserts a forged/foreign run_id is refused 404 —
// SubmitInput re-resolves ownership server-side.
func TestSteerChatRunForeignRun(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindIssue, Status: "running"}}
	h := newChatHandler(st)

	foreign := uuid.New()
	rec := httptest.NewRecorder()
	body := `{"run_id":"` + foreign.String() + `","message":"steer me"}`
	h.SteerChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign-run SteerChatRun = %d, want 404", rec.Code)
	}
	if st.inputKind != "" {
		t.Fatal("a refused steer must not enqueue any input")
	}
}

// TestSteerChatRunTerminalRun asserts an already-terminal run is refused 409.
func TestSteerChatRunTerminalRun(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindIssue, Status: "completed"}}
	h := newChatHandler(st)

	rec := httptest.NewRecorder()
	body := `{"run_id":"` + runID.String() + `","message":"steer me"}`
	h.SteerChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("terminal-run SteerChatRun = %d, want 409", rec.Code)
	}
	if st.inputKind != "" {
		t.Fatal("a terminal run must not be steered")
	}
}

// TestSteerChatRunBadRequestAndAuth asserts a blank message → 400 (no SubmitInput call),
// a missing/blank run_id → 400, and an unauthenticated caller → 401.
func TestSteerChatRunBadRequestAndAuth(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &chatStore{ownerID: owner.ID, chatRun: store.Run{ID: runID, UserID: owner.ID, Kind: workersvc.RunKindIssue, Status: "running"}}
	h := newChatHandler(st)

	// Blank message → 400, before any SubmitInput call.
	rec := httptest.NewRecorder()
	h.SteerChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, `{"run_id":"`+runID.String()+`","message":"   "}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank-message SteerChatRun = %d, want 400", rec.Code)
	}
	if st.inputKind != "" {
		t.Fatal("a blank-message steer must not enqueue any input")
	}

	// Blank run_id → 400 (uuid.Parse fails).
	rec = httptest.NewRecorder()
	h.SteerChatRun(rec, chatReq(http.MethodPost, owner, uuid.Nil, uuid.Nil, `{"run_id":"   ","message":"go"}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank run_id SteerChatRun = %d, want 400", rec.Code)
	}

	// Unauthenticated → 401.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/chats/steer-requests", strings.NewReader(`{"run_id":"x","message":"go"}`))
	h.SteerChatRun(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated SteerChatRun = %d, want 401", rec.Code)
	}
}
