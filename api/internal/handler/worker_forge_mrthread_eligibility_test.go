package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Reply/resolve scope after author eligibility reached the MR review lane (issue #2347): a thread
// is writable only through a comment that is IN the run's assessed snapshot. Withheld comments are
// omitted from it, a thread can be mixed (an eligible comment keeps it reachable), and a snapshot
// captured before assessment existed (version 0) authorizes nothing.

func snapshotWith(t *testing.T, version, withheldNotEligible int, comments ...workersvc.ReviewCommentSnapshot) []byte {
	t.Helper()
	b, err := json.Marshal(workersvc.ReviewCommentsSnapshot{Version: version, Comments: comments, WithheldNotEligible: withheldNotEligible})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMRThreadWithheldCommentsThreadIsRefused(t *testing.T) {
	// The outsider's thread "disc-outsider" is not in the snapshot (its comment was withheld);
	// only the member's thread "disc-member" is. No driver route is registered, so reaching the
	// driver would 502 instead of 403.
	snap := snapshotWith(t, workersvc.ReviewSnapshotVersion, 1, snapComment("disc-member", "disc-member"))
	h := mrThreadMockHandler(t, "gitlab", mrThreadIID(), snap, nil)

	rec := httptest.NewRecorder()
	h.WorkerForgeReplyMRThread(rec, mrThreadReq("/x", true, apitypes.ForgeMRThreadReplyRequest{ReplyID: "disc-outsider", Body: "done"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reply to a withheld thread = %d, want 403, body %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.WorkerForgeResolveMRThread(rec, mrThreadReq("/x", true, apitypes.ForgeMRThreadResolveRequest{ResolveID: "disc-outsider"}))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("resolve of a withheld thread = %d, want 403, body %q", rec.Code, rec.Body.String())
	}
}

func TestMRThreadMixedThreadIsReachableThroughItsEligibleComment(t *testing.T) {
	// One thread holds an outsider's comment (withheld) and a member's reply (kept): the shared
	// thread anchor is in the snapshot through the member's comment, so the run may answer it.
	hit := false
	snap := snapshotWith(t, workersvc.ReviewSnapshotVersion, 1, snapComment("disc-mixed", "disc-mixed"))
	h := mrThreadMockHandler(t, "gitlab", mrThreadIID(), snap, map[string]http.HandlerFunc{
		"/api/v4/projects/4242/merge_requests/284/discussions/disc-mixed/notes": func(w http.ResponseWriter, _ *http.Request) {
			hit = true
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001})
		},
	})
	rec := httptest.NewRecorder()
	h.WorkerForgeReplyMRThread(rec, mrThreadReq("/x", true, apitypes.ForgeMRThreadReplyRequest{ReplyID: "disc-mixed", Body: "done"}))
	if rec.Code != http.StatusOK || !hit {
		t.Fatalf("reply to a mixed thread = %d (driver hit %t), want 200 through the eligible comment, body %q", rec.Code, hit, rec.Body.String())
	}
}

func TestMRThreadLegacySnapshotAuthorizesNothing(t *testing.T) {
	// Version 0 (no version key at all in a row written before this lane assessed authors) holds
	// a thread whose anchor would otherwise match.
	for name, snap := range map[string][]byte{
		"explicit version 0": snapshotWith(t, 0, 0, snapComment("disc-real", "disc-real")),
		"no version key":     []byte(`{"comments":[{"id":1,"author_username":"reviewer","body":"please fix","reply_id":"disc-real","resolve_id":"disc-real","review_state":"inline"}],"truncated":false}`),
		"unknown version":    snapshotWith(t, 1, 0, snapComment("disc-real", "disc-real")),
	} {
		t.Run(name, func(t *testing.T) {
			h := mrThreadMockHandler(t, "gitlab", mrThreadIID(), snap, nil)
			rec := httptest.NewRecorder()
			h.WorkerForgeReplyMRThread(rec, mrThreadReq("/x", true, apitypes.ForgeMRThreadReplyRequest{ReplyID: "disc-real", Body: "done"}))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("reply via a legacy snapshot = %d, want 403, body %q", rec.Code, rec.Body.String())
			}
			rec = httptest.NewRecorder()
			h.WorkerForgeResolveMRThread(rec, mrThreadReq("/x", true, apitypes.ForgeMRThreadResolveRequest{ResolveID: "disc-real"}))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("resolve via a legacy snapshot = %d, want 403, body %q", rec.Code, rec.Body.String())
			}
		})
	}
}
