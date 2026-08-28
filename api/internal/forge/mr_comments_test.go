package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestGitLabListMergeRequestComments pins the GitLab MR-comment read (PRD #700 M1):
// discussions are flattened to MRComments, System notes are dropped (D2), the list
// is oldest-first (D8), and the discussion id is BOTH the reply and resolve anchor.
// An inline note carries Path/Line/HeadSHA from its Position; a top-level note does
// not.
func TestGitLabListMergeRequestComments(t *testing.T) {
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/13/discussions": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			w.Header().Set("X-Next-Page", "")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "disc-top", "notes": []map[string]any{
					{"id": 701, "system": false, "body": "top-level thought",
						"author": map[string]any{"id": 42, "username": "carol"}, "created_at": "2026-07-04T09:00:00Z"},
				}},
				{"id": "disc-sys", "notes": []map[string]any{
					{"id": 702, "system": true, "body": "changed the milestone",
						"author": map[string]any{"id": 99, "username": "gitlab-bot"}, "created_at": "2026-07-04T09:15:00Z"},
				}},
				{"id": "disc-inline", "notes": []map[string]any{
					{"id": 703, "system": false, "body": "guard nil here",
						"author":     map[string]any{"id": 43, "username": "coderabbit"},
						"created_at": "2026-07-04T10:00:00Z",
						"position":   map[string]any{"head_sha": "headsha123", "new_path": "api/x.go", "new_line": 42, "position_type": "text"}},
				}},
			})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	comments, err := d.ListMergeRequestComments(context.Background(), 7, 13)
	if err != nil {
		t.Fatalf("ListMergeRequestComments: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments (system note dropped), got %d: %+v", len(comments), comments)
	}
	top := comments[0]
	if top.Body != "top-level thought" || top.AuthorUsername != "carol" {
		t.Fatalf("unexpected top-level comment: %+v", top)
	}
	if top.ReplyID != "disc-top" || top.ResolveID != "disc-top" {
		t.Errorf("top-level anchors: reply=%q resolve=%q, want both disc-top", top.ReplyID, top.ResolveID)
	}
	if top.ReviewState != ReviewCommentSummary || top.Path != nil || top.Line != nil {
		t.Errorf("top-level should be a summary with no diff anchor: %+v", top)
	}
	inline := comments[1]
	if inline.Body != "guard nil here" || inline.AuthorUsername != "coderabbit" {
		t.Fatalf("unexpected inline comment: %+v", inline)
	}
	if inline.ReplyID != "disc-inline" || inline.ResolveID != "disc-inline" {
		t.Errorf("inline anchors: reply=%q resolve=%q, want both disc-inline", inline.ReplyID, inline.ResolveID)
	}
	if inline.HeadSHA != "headsha123" {
		t.Errorf("inline HeadSHA = %q, want headsha123", inline.HeadSHA)
	}
	if inline.ReviewState != ReviewCommentInline || inline.Path == nil || *inline.Path != "api/x.go" || inline.Line == nil || *inline.Line != 42 {
		t.Errorf("inline diff anchor wrong: state=%q path=%v line=%v", inline.ReviewState, inline.Path, inline.Line)
	}
	if !comments[0].CreatedAt.Before(comments[1].CreatedAt) {
		t.Errorf("comments not oldest-first: %v then %v", comments[0].CreatedAt, comments[1].CreatedAt)
	}
}

// TestGitLabReplyMergeRequestComment pins the reply keyed on the reply anchor: a
// POST to the discussion's notes endpoint carrying the body.
func TestGitLabReplyMergeRequestComment(t *testing.T) {
	var gotBody, gotPath string
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/13/discussions/disc-inline/notes": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			gotPath = r.URL.Path
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotBody = body.Body
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9100, "body": body.Body})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	if err := d.ReplyMergeRequestComment(context.Background(), 7, 13, "disc-inline", "done in abc123"); err != nil {
		t.Fatalf("ReplyMergeRequestComment: %v", err)
	}
	if gotBody != "done in abc123" {
		t.Errorf("wrong reply body sent: %q", gotBody)
	}
	if !strings.Contains(gotPath, "/discussions/disc-inline/notes") {
		t.Errorf("reply not keyed on the reply anchor: %q", gotPath)
	}
}

// TestGitLabResolveMergeRequestThread pins resolve keyed on the resolve anchor: a
// PUT to the discussion endpoint with resolved=true.
func TestGitLabResolveMergeRequestThread(t *testing.T) {
	var gotPath, gotMethod string
	gotResolved := false
	m := newMockGitLab(t, map[string]http.HandlerFunc{
		"/api/v4/projects/7/merge_requests/13/discussions/disc-inline": func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), "\"resolved\":true") || r.URL.Query().Get("resolved") == "true" {
				gotResolved = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "disc-inline"})
		},
	})
	d := newTestDriver(t, m, "glpat-abcdefabcdef")

	if err := d.ResolveMergeRequestThread(context.Background(), 7, 13, "disc-inline"); err != nil {
		t.Fatalf("ResolveMergeRequestThread: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("expected PUT, got %s", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/discussions/disc-inline") {
		t.Errorf("resolve not keyed on the resolve anchor: %q", gotPath)
	}
	if !gotResolved {
		t.Error("resolve did not request resolved=true")
	}
}

// TestGitHubListMergeRequestComments is the seam-critical two-anchor case: the read
// STITCHES three REST sources with the GraphQL reviewThreads query so an inline
// comment carries BOTH ReplyID (REST databaseId) and ResolveID (thread node id),
// plus HeadSHA — not just the REST fields. It also folds in the top-level note and
// the review-summary body, oldest-first.
func TestGitHubListMergeRequestComments(t *testing.T) {
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/13/comments": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 901, "body": "top-level note", "created_at": "2026-07-04T09:00:00Z",
					"user": map[string]any{"id": 42, "login": "carol"}},
			})
		},
		"/repos/acme/widgets/pulls/13/comments": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 5001, "body": "guard nil here", "path": "api/x.go", "line": 42,
					"commit_id": "headsha999", "created_at": "2026-07-04T10:00:00Z",
					"user": map[string]any{"id": 43, "login": "coderabbit"}},
			})
		},
		"/repos/acme/widgets/pulls/13/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 6001, "body": "overall looks good, one nit", "state": "COMMENTED",
					"commit_id": "headsha999", "submitted_at": "2026-07-04T09:30:00Z",
					"user": map[string]any{"id": 43, "login": "coderabbit"}},
				{"id": 6002, "body": "", "state": "COMMENTED", "submitted_at": "2026-07-04T09:45:00Z",
					"user": map[string]any{"id": 43, "login": "coderabbit"}},
			})
		},
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			_ = readGQL(t, r) // drain + assert decodable
			writeGQLData(w, map[string]any{
				"repository": map[string]any{
					"pullRequest": map[string]any{
						"reviewThreads": map[string]any{
							"nodes": []map[string]any{
								{"id": "PRRT_thread1", "isResolved": false, "isOutdated": false,
									"path": "api/x.go", "line": 42,
									"comments": map[string]any{"nodes": []map[string]any{{"databaseId": 5001}}}},
							},
						},
					},
				},
			})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	comments, err := d.ListMergeRequestComments(context.Background(), 7, 13)
	if err != nil {
		t.Fatalf("ListMergeRequestComments: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("expected 3 comments (top-level + review summary + inline; empty-body review dropped), got %d: %+v", len(comments), comments)
	}
	// oldest-first: top-level (09:00), review summary (09:30), inline (10:00).
	if comments[0].Body != "top-level note" || comments[0].ReviewState != ReviewCommentSummary {
		t.Errorf("comment[0] should be the top-level note: %+v", comments[0])
	}
	// ID is populated from the REST databaseId of each source (PRD #700 (a) — the
	// high-water anchor M3 keys on): top-level note 901, review summary 6001, inline 5001.
	if comments[0].ID != 901 {
		t.Errorf("top-level note ID = %d, want the REST databaseId 901", comments[0].ID)
	}
	if comments[0].ReplyID != "" || comments[0].ResolveID != "" {
		t.Errorf("top-level note should carry no thread anchors: %+v", comments[0])
	}
	summary := comments[1]
	if summary.Body != "overall looks good, one nit" || summary.ReviewState != ReviewCommentSummary {
		t.Errorf("comment[1] should be the review summary: %+v", summary)
	}
	if summary.HeadSHA != "headsha999" {
		t.Errorf("review summary HeadSHA = %q, want headsha999", summary.HeadSHA)
	}
	if summary.ID != 6001 {
		t.Errorf("review summary ID = %d, want the REST databaseId 6001", summary.ID)
	}
	inline := comments[2]
	if inline.Body != "guard nil here" || inline.ReviewState != ReviewCommentInline {
		t.Fatalf("comment[2] should be the inline review comment: %+v", inline)
	}
	if inline.ID != 5001 {
		t.Errorf("inline ID = %d, want the REST databaseId 5001", inline.ID)
	}
	// The two-anchor stitch — the whole point of the GitHub read.
	if inline.ReplyID != "5001" {
		t.Errorf("inline ReplyID = %q, want the REST databaseId 5001", inline.ReplyID)
	}
	if inline.ResolveID != "PRRT_thread1" {
		t.Errorf("inline ResolveID = %q, want the GraphQL thread node id PRRT_thread1 (STITCH FAILED)", inline.ResolveID)
	}
	if inline.HeadSHA != "headsha999" {
		t.Errorf("inline HeadSHA = %q, want the REST commit_id headsha999", inline.HeadSHA)
	}
	if inline.Path == nil || *inline.Path != "api/x.go" || inline.Line == nil || *inline.Line != 42 {
		t.Errorf("inline diff anchor wrong: path=%v line=%v", inline.Path, inline.Line)
	}
}

// TestGitHubReplyMergeRequestComment pins the reply keyed on the REST databaseId
// (the reply anchor): CreateCommentInReplyTo POSTs with in_reply_to set.
func TestGitHubReplyMergeRequestComment(t *testing.T) {
	var gotInReplyTo int64
	var gotBody string
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/13/comments": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			var body struct {
				Body      string `json:"body"`
				InReplyTo int64  `json:"in_reply_to"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotBody = body.Body
			gotInReplyTo = body.InReplyTo
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5002, "body": body.Body})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	if err := d.ReplyMergeRequestComment(context.Background(), 7, 13, "5001", "skipped because stale"); err != nil {
		t.Fatalf("ReplyMergeRequestComment: %v", err)
	}
	if gotInReplyTo != 5001 {
		t.Errorf("reply not keyed on the REST databaseId: in_reply_to=%d, want 5001", gotInReplyTo)
	}
	if gotBody != "skipped because stale" {
		t.Errorf("wrong reply body: %q", gotBody)
	}
}

// TestGitHubResolveMergeRequestThread pins resolve keyed on the GraphQL thread node
// id: the resolveReviewThread mutation is POSTed with threadId set.
func TestGitHubResolveMergeRequestThread(t *testing.T) {
	var gotQuery, gotThreadID string
	m := newMockGitHub(t, map[string]http.HandlerFunc{
		graphqlRoute: func(w http.ResponseWriter, r *http.Request) {
			req := readGQL(t, r)
			gotQuery = req.Query
			if v, ok := req.Variables["threadId"].(string); ok {
				gotThreadID = v
			}
			writeGQLData(w, map[string]any{
				"resolveReviewThread": map[string]any{
					"thread": map[string]any{"id": "PRRT_thread1", "isResolved": true},
				},
			})
		},
	})
	d := newGitHubDriver(t, m, "ghp_classicTokenValue1234567890")

	if err := d.ResolveMergeRequestThread(context.Background(), 7, 13, "PRRT_thread1"); err != nil {
		t.Fatalf("ResolveMergeRequestThread: %v", err)
	}
	if !strings.Contains(gotQuery, "resolveReviewThread") {
		t.Errorf("resolve did not issue the resolveReviewThread mutation: %q", gotQuery)
	}
	if gotThreadID != "PRRT_thread1" {
		t.Errorf("resolve not keyed on the resolve anchor: threadId=%q, want PRRT_thread1", gotThreadID)
	}
}

// TestForgejoListMergeRequestComments pins the Forgejo read: top-level notes +
// review-summary bodies + inline review comments, oldest-first, with the
// review-comment id as the reply anchor and an EMPTY resolve anchor (Forgejo has no
// resolvable-thread concept).
func TestForgejoListMergeRequestComments(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/issues/13/comments": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 801, "body": "top-level note", "created_at": "2026-07-04T09:00:00Z", "updated_at": "2026-07-04T09:00:00Z",
					"user": map[string]any{"id": 42, "login": "carol"}},
			})
		},
		"/repos/acme/widgets/pulls/13/reviews": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 6001, "body": "overall nit", "commit_id": "fjhead", "submitted_at": "2026-07-04T09:30:00Z",
					"user": map[string]any{"id": 43, "login": "coderabbit"}},
			})
		},
		"/repos/acme/widgets/pulls/13/reviews/6001/comments": func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 7001, "body": "guard nil here", "path": "api/x.go", "position": 42,
					"commit_id": "fjhead", "created_at": "2026-07-04T10:00:00Z", "updated_at": "2026-07-04T10:00:00Z",
					"user": map[string]any{"id": 43, "login": "coderabbit"}},
			})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	comments, err := d.ListMergeRequestComments(context.Background(), 7, 13)
	if err != nil {
		t.Fatalf("ListMergeRequestComments: %v", err)
	}
	if len(comments) != 3 {
		t.Fatalf("expected 3 comments (top-level + review summary + inline), got %d: %+v", len(comments), comments)
	}
	if comments[0].Body != "top-level note" || comments[0].ReviewState != ReviewCommentSummary {
		t.Errorf("comment[0] should be the top-level note: %+v", comments[0])
	}
	if comments[1].Body != "overall nit" || comments[1].ReviewState != ReviewCommentSummary || comments[1].HeadSHA != "fjhead" {
		t.Errorf("comment[1] should be the review summary with HeadSHA fjhead: %+v", comments[1])
	}
	inline := comments[2]
	if inline.Body != "guard nil here" || inline.ReviewState != ReviewCommentInline {
		t.Fatalf("comment[2] should be the inline review comment: %+v", inline)
	}
	if inline.ReplyID != "7001" {
		t.Errorf("inline ReplyID = %q, want the review-comment id 7001", inline.ReplyID)
	}
	if inline.ResolveID != "" {
		t.Errorf("inline ResolveID = %q, want EMPTY on Forgejo (no resolvable-thread concept)", inline.ResolveID)
	}
	if inline.HeadSHA != "fjhead" || inline.Path == nil || *inline.Path != "api/x.go" || inline.Line == nil || *inline.Line != 42 {
		t.Errorf("inline anchors wrong: headsha=%q path=%v line=%v", inline.HeadSHA, inline.Path, inline.Line)
	}
	if !comments[0].CreatedAt.Before(comments[1].CreatedAt) || !comments[1].CreatedAt.Before(comments[2].CreatedAt) {
		t.Errorf("comments not oldest-first: %v, %v, %v", comments[0].CreatedAt, comments[1].CreatedAt, comments[2].CreatedAt)
	}
}

// TestForgejoReplyMergeRequestComment pins the reply keyed on the review-comment id:
// a POST to the review-comment replies endpoint.
func TestForgejoReplyMergeRequestComment(t *testing.T) {
	var gotBody, gotPath string
	m := newMockForgejo(t, map[string]http.HandlerFunc{
		"/repos/acme/widgets/pulls/13/comments/7001/replies": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("expected POST, got %s", r.Method)
			}
			gotPath = r.URL.Path
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotBody = body.Body
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7002, "body": body.Body})
		},
	})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	if err := d.ReplyMergeRequestComment(context.Background(), 7, 13, "7001", "done in abc123"); err != nil {
		t.Fatalf("ReplyMergeRequestComment: %v", err)
	}
	if gotBody != "done in abc123" {
		t.Errorf("wrong reply body: %q", gotBody)
	}
	if !strings.Contains(gotPath, "/comments/7001/replies") {
		t.Errorf("reply not keyed on the review-comment id: %q", gotPath)
	}
}

// TestForgejoResolveMergeRequestThreadUnsupported pins the documented Forgejo
// contract: resolve is a no-op that returns ErrResolveUnsupported for the worker to
// swallow (reply-only).
func TestForgejoResolveMergeRequestThreadUnsupported(t *testing.T) {
	m := newMockForgejo(t, map[string]http.HandlerFunc{})
	d := newForgejoDriver(t, m, "forgejo-abcdefabcdef")

	err := d.ResolveMergeRequestThread(context.Background(), 7, 13, "")
	if !errors.Is(err, ErrResolveUnsupported) {
		t.Fatalf("expected ErrResolveUnsupported, got %v", err)
	}
}
