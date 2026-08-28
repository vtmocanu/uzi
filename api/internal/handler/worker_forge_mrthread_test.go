package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ─────────────────────────────────────────────────────────────────────────────
// PRD #700 M4 — the mr_rework write-back endpoints (reply + resolve) and their
// Decision-11 server-side scope check. These are CONTAINMENT/no-op tests (SC2), not
// run-status tests: the load-bearing property is that a reply/resolve id NOT present
// in THIS run's review snapshot is server-rejected before any driver call, so an
// injected "resolve all open threads" is a no-op no matter what the model passes.
//
// The seam is the same one the read tests use (worker_forge_test.go): a real gitlab/
// forgejo driver over an httptest server, with the fake Store handing back an owned
// run carrying the mr_iid and the review-comments snapshot on ForgeConn. For the
// scope-REJECT cases no driver route is registered/hit at all — the rejection is
// purely server-side.
// ─────────────────────────────────────────────────────────────────────────────

const mrThreadMRIID = 284

// mrThreadMockHandler builds a Handler wired to a mock forge (forgeType) whose owned
// run carries mrIID + the review-comments snapshot. routes may be empty for a case that
// must reject before any driver call.
func mrThreadMockHandler(t *testing.T, forgeType string, mrIID *int64, snap []byte, routes map[string]http.HandlerFunc) *Handler {
	t.Helper()
	box := newForgeBox(t)
	mux := http.NewServeMux()
	for pattern, hh := range routes {
		mux.HandleFunc(pattern, hh)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	sealed, err := box.Seal([]byte("glpat-fake-forge-token-abcdef123456")) //gitleaks:allow fake fixture PAT (literal "fake"), sealed into a throwaway test secretbox; never a real credential
	if err != nil {
		t.Fatalf("seal token: %v", err)
	}
	run := store.Run{
		ID:             uuid.New(),
		UserID:         uuid.New(),
		RepoID:         pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ReviewComments: snap,
	}
	if mrIID != nil {
		run.MrIid = pgtype.Int8{Int64: *mrIID, Valid: true}
	}
	st := &forgeHandlerStore{
		ownedRun: run,
		connRow: store.GetRunForgeConnForWorkerRow{
			ForgeProjectID:  forgeTestProjectID,
			ForgeType:       forgeType,
			BaseUrl:         srv.URL,
			TokenCiphertext: sealed,
		},
	}
	return newForgeHandler(t, st, box)
}

// mrThreadReq builds a worker-authenticated POST carrying a JSON body and the {id} chi
// param, mirroring forgeReq (which carries no body).
func mrThreadReq(target string, withWorker bool, body any) *http.Request {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(http.MethodPost, target, rdr)
	ctx := req.Context()
	if withWorker {
		ctx = mw.ContextWithWorker(ctx, store.Worker{ID: uuid.New(), UserID: uuid.New()})
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	return req.WithContext(ctx)
}

func mrThreadIID() *int64 { v := int64(mrThreadMRIID); return &v }

// snapshotJSON marshals a review-comments snapshot to the raw JSONB shape stored in
// runs.review_comments.
func snapshotJSON(t *testing.T, comments ...workersvc.ReviewCommentSnapshot) []byte {
	t.Helper()
	b, err := json.Marshal(workersvc.ReviewCommentsSnapshot{Comments: comments})
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	return b
}

// snapComment builds one snapshot comment with the given reply/resolve anchors.
func snapComment(replyID, resolveID string) workersvc.ReviewCommentSnapshot {
	return workersvc.ReviewCommentSnapshot{
		ID:             1,
		AuthorUsername: "reviewer",
		Body:           "please fix",
		ReplyID:        replyID,
		ResolveID:      resolveID,
		ReviewState:    "inline",
	}
}

// ── Resolve-injection no-op: an out-of-snapshot id is rejected, no thread resolved ──

func TestResolveMRThreadRejectsIDNotInSnapshot(t *testing.T) {
	// The snapshot's only thread has resolve anchor "disc-real". An injected
	// "resolve open threads" can only pass an id; "disc-EVERYTHING" is not in the
	// snapshot, so the endpoint must 403 BEFORE any driver call — no route registered,
	// so a driver call would 502 and the assertion below would fail loudly instead.
	h := mrThreadMockHandler(t, "gitlab", mrThreadIID(),
		snapshotJSON(t, snapComment("disc-real", "disc-real")), nil)
	rec := httptest.NewRecorder()
	h.WorkerForgeResolveMRThread(rec, mrThreadReq("/x", true,
		apitypes.ForgeMRThreadResolveRequest{ResolveID: "disc-EVERYTHING"}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (out-of-snapshot resolve id rejected), body %q", rec.Code, rec.Body.String())
	}
	if got := errorField(t, rec.Body.Bytes()); got != forgeErrMRThreadScope {
		t.Errorf("error = %q, want %q", got, forgeErrMRThreadScope)
	}
}

func TestReplyMRThreadRejectsIDNotInSnapshot(t *testing.T) {
	h := mrThreadMockHandler(t, "gitlab", mrThreadIID(),
		snapshotJSON(t, snapComment("disc-real", "disc-real")), nil)
	rec := httptest.NewRecorder()
	h.WorkerForgeReplyMRThread(rec, mrThreadReq("/x", true,
		apitypes.ForgeMRThreadReplyRequest{ReplyID: "disc-EVERYTHING", Body: "done"}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (out-of-snapshot reply id rejected), body %q", rec.Code, rec.Body.String())
	}
	if got := errorField(t, rec.Body.Bytes()); got != forgeErrMRThreadScope {
		t.Errorf("error = %q, want %q", got, forgeErrMRThreadScope)
	}
}

// ── A valid (in-snapshot) id reaches the driver on the run's own mr_iid ──

func TestReplyMRThreadValidIDCallsDriver(t *testing.T) {
	hit := false
	h := mrThreadMockHandler(t, "gitlab", mrThreadIID(),
		snapshotJSON(t, snapComment("disc-real", "disc-real")),
		map[string]http.HandlerFunc{
			// go-gitlab AddMergeRequestDiscussionNote → POST /discussions/{id}/notes,
			// on the run's OWN mr_iid (284) — never a client-supplied one.
			"/api/v4/projects/4242/merge_requests/284/discussions/disc-real/notes": func(w http.ResponseWriter, _ *http.Request) {
				hit = true
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001})
			},
		})
	rec := httptest.NewRecorder()
	h.WorkerForgeReplyMRThread(rec, mrThreadReq("/x", true,
		apitypes.ForgeMRThreadReplyRequest{ReplyID: "disc-real", Body: "done in abc123"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !hit {
		t.Fatal("a valid in-snapshot reply id must reach the driver's reply endpoint")
	}
	var dto apitypes.ForgeMRThreadReplyDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dto.Replied {
		t.Errorf("replied = false, want true")
	}
}

func TestResolveMRThreadValidIDResolves(t *testing.T) {
	hit := false
	h := mrThreadMockHandler(t, "gitlab", mrThreadIID(),
		snapshotJSON(t, snapComment("disc-real", "disc-real")),
		map[string]http.HandlerFunc{
			// go-gitlab ResolveMergeRequestDiscussion → PUT /discussions/{id}.
			"/api/v4/projects/4242/merge_requests/284/discussions/disc-real": func(w http.ResponseWriter, _ *http.Request) {
				hit = true
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "disc-real"})
			},
		})
	rec := httptest.NewRecorder()
	h.WorkerForgeResolveMRThread(rec, mrThreadReq("/x", true,
		apitypes.ForgeMRThreadResolveRequest{ResolveID: "disc-real"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !hit {
		t.Fatal("a valid in-snapshot resolve id must reach the driver's resolve endpoint")
	}
	var dto apitypes.ForgeMRThreadResolveDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !dto.Resolved {
		t.Errorf("resolved = false, want true")
	}
}

// ── Forgejo swallow: ErrResolveUnsupported is tolerated (no-op success, no failure) ──

func TestResolveMRThreadForgejoSwallowsUnsupported(t *testing.T) {
	// The forgejo driver's ResolveMergeRequestThread returns ErrResolveUnsupported
	// WITHOUT a network call, so no resolve route is needed. The endpoint must tolerate
	// it: 200 with resolved=false, so the worker's reply still stands and the run does
	// not fail (reply-only is the documented Forgejo contract).
	h := mrThreadMockHandler(t, "forgejo", mrThreadIID(),
		snapshotJSON(t, snapComment("comment-7", "comment-7")), nil)
	rec := httptest.NewRecorder()
	h.WorkerForgeResolveMRThread(rec, mrThreadReq("/x", true,
		apitypes.ForgeMRThreadResolveRequest{ResolveID: "comment-7"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ErrResolveUnsupported tolerated), body %q", rec.Code, rec.Body.String())
	}
	var dto apitypes.ForgeMRThreadResolveDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.Resolved {
		t.Errorf("resolved = true, want false (Forgejo no-op)")
	}
}

// ── A run with no source MR cannot write back at all ──

func TestReplyMRThreadNoMRIs422(t *testing.T) {
	h := mrThreadMockHandler(t, "gitlab", nil,
		snapshotJSON(t, snapComment("disc-real", "disc-real")), nil)
	rec := httptest.NewRecorder()
	h.WorkerForgeReplyMRThread(rec, mrThreadReq("/x", true,
		apitypes.ForgeMRThreadReplyRequest{ReplyID: "disc-real", Body: "done"}))

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (run carries no mr_iid), body %q", rec.Code, rec.Body.String())
	}
}

// ── An empty snapshot rejects every id (fail closed) ──

func TestResolveMRThreadEmptySnapshotRejects(t *testing.T) {
	h := mrThreadMockHandler(t, "gitlab", mrThreadIID(), nil, nil)
	rec := httptest.NewRecorder()
	h.WorkerForgeResolveMRThread(rec, mrThreadReq("/x", true,
		apitypes.ForgeMRThreadResolveRequest{ResolveID: "disc-real"}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (no snapshot ⇒ no thread to resolve), body %q", rec.Code, rec.Body.String())
	}
}
