package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise the production drivers, capture projection, and worker HTTP view together.
func TestM2RealDriverOverflowProjection(t *testing.T) {
	for _, kind := range []string{"gitlab", "github"} {
		for _, mode := range []string{"target", "comments", "comments_page2", "user", "collaborators"} {
			if mode == "collaborators" && kind != "github" {
				continue
			}
			t.Run(kind+"/"+mode, func(t *testing.T) {
				var page2 atomic.Int64
				var srv *httptest.Server
				srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					path := r.URL.Path
					w.Header().Set("Content-Type", "application/json")
					write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
					large := func(n int, collection bool) {
						w.(http.Flusher).Flush()
						value := map[string]any{
							"id": 2, "username": "alice", "login": "alice", "state": "active",
							"body": "SOURCE_COMMENT", "created_at": "2026-10-01T00:00:00Z",
							"author":      map[string]any{"id": 2, "username": "alice"},
							"user":        map[string]any{"id": 2, "login": "alice"},
							"permissions": map[string]bool{"pull": true, "triage": true, "push": true, "maintain": false, "admin": false},
							"padding":     strings.Repeat("x", n),
						}
						if collection {
							write([]map[string]any{value})
						} else {
							write(value)
						}
					}
					switch {
					case strings.Contains(path, "/repositories/"):
						write(map[string]any{"id": 4242, "name": "repo", "owner": map[string]any{"login": "acme"}})
					case strings.Contains(path, "/collaborators"):
						if mode == "collaborators" {
							large(1<<20, true)
							return
						}
						write([]map[string]any{{"id": 2, "login": "alice", "permissions": map[string]bool{"pull": true, "triage": true, "push": true, "maintain": false, "admin": false}}})
					case strings.Contains(path, "/users/") || strings.Contains(path, "/user/"):
						if mode == "user" {
							large(1<<20, false)
							return
						}
						write(map[string]any{"id": 2, "username": "alice", "login": "alice", "state": "active"})
					case strings.Contains(path, "/members/all/"):
						write(map[string]any{"id": 2, "username": "alice", "state": "active", "access_level": 30})
					case strings.HasSuffix(path, "/notes") || strings.HasSuffix(path, "/comments"):
						if mode == "comments" || (mode == "comments_page2" && r.URL.Query().Get("page") == "2") {
							if r.URL.Query().Get("page") == "2" {
								page2.Add(1)
							}
							large(8<<20, true)
							return
						}
						if mode == "comments_page2" {
							next := srv.URL + path + "?page=2"
							w.Header().Set("Link", fmt.Sprintf("<%s>; rel=\"next\"", next))
							w.Header().Set("X-Next-Page", "2")
						}
						write([]map[string]any{{"id": 1, "body": "SOURCE_COMMENT", "created_at": "2026-10-01T00:00:00Z", "author": map[string]any{"id": 2, "username": "alice"}, "user": map[string]any{"id": 2, "login": "alice"}}})
					case strings.Contains(path, "/issues/"):
						body := "SOURCE_BODY"
						if mode == "target" {
							body = strings.Repeat("x", 2<<20)
							w.(http.Flusher).Flush()
						}
						write(map[string]any{"id": 1, "iid": 11, "number": 11, "title": "SOURCE_TITLE", "description": body, "body": body, "state": "opened", "author": map[string]any{"id": 2, "username": "alice"}, "user": map[string]any{"id": 2, "login": "alice"}})
					default:
						t.Errorf("unexpected route %s", path)
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(srv.Close)
				box := newForgeBox(t)
				sealed, err := box.Seal([]byte("fixture"))
				if err != nil {
					t.Fatal(err)
				}
				st := &forgeHandlerStore{
					ownedRun: store.Run{ID: uuid.New(), RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Kind: "issue", IssueIid: pgtype.Int8{Int64: 11, Valid: true}, IssueTitle: "saved title", IssueSavedBody: pgtype.Text{String: "saved body", Valid: true}, IssueRawDigest: pgtype.Text{String: issueinput.Digest("saved title", "saved body"), Valid: true}},
					connRow:  store.GetRunForgeConnForWorkerRow{ForgeProjectID: 4242, ForgeType: kind, BaseUrl: srv.URL, TokenCiphertext: sealed, BotForgeUserID: 1},
				}
				h := newForgeHandler(t, st, box)
				driver, err := h.svc.ForgeForConnection(kind, srv.URL, sealed)
				if err != nil {
					t.Fatal(err)
				}
				capture, err := issueinput.Fetch(context.Background(), driver, 4242, 11, 1)
				if mode == "target" {
					var concrete *forge.ResponseTooLargeError
					if capture != nil || !errors.Is(err, forge.ErrResponseTooLarge) || !errors.As(err, &concrete) {
						t.Fatalf("raw capture present=%t err=%v", capture != nil, err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					if strings.HasPrefix(mode, "comments") {
						if !capture.Thread.Unknown || len(capture.Thread.Comments) != 0 || capture.Reason != "" {
							t.Fatalf("failed comments were retained: %+v", capture)
						}
					} else if capture.Reason != issueinput.Unknown || !capture.Unknown || len(capture.Thread.Comments) != 1 || capture.Thread.Comments[0].Body != issueinput.Placeholder {
						t.Fatalf("permission failure not closed: %+v", capture)
					}
				}
				for _, own := range []bool{true, false} {
					if !own {
						st.ownedRun.IssueIid.Int64 = 12
					}
					rec := httptest.NewRecorder()
					h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true, map[string]string{"id": st.ownedRun.ID.String(), "iid": "11"}))
					if mode == "target" && !own {
						if rec.Code != 502 || strings.Contains(rec.Body.String(), "SOURCE_") {
							t.Fatalf("other failed target: %d %s", rec.Code, rec.Body.String())
						}
						continue
					}
					if rec.Code != 200 {
						t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
					}
					var dto apitypes.ForgeIssueDTO
					if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
						t.Fatal(err)
					}
					if own && (dto.Title != "saved title" || dto.Description != "saved body") {
						t.Fatalf("saved content replaced: %+v", dto)
					}
					switch {
					case mode == "target":
						if !dto.MetadataUnavailable || !dto.CommentsUnknown || len(dto.Comments) != 0 {
							t.Fatalf("unavailable metadata=%+v", dto)
						}
					case strings.HasPrefix(mode, "comments"):
						if !dto.CommentsUnknown || len(dto.Comments) != 0 || strings.Contains(rec.Body.String(), "SOURCE_COMMENT") {
							t.Fatalf("partial comments retained: %+v", dto)
						}
					default:
						if !dto.CommentsUnknown || len(dto.Comments) != 1 || dto.Comments[0].Body != issueinput.Placeholder || strings.Contains(rec.Body.String(), "SOURCE_COMMENT") {
							t.Fatalf("comment leaked: %+v", dto)
						}
						if !own && (dto.Title != issueinput.Placeholder || dto.Description != issueinput.Placeholder || dto.ContentReason != issueinput.Unknown || strings.Contains(rec.Body.String(), "SOURCE_BODY")) {
							t.Fatalf("unknown source leaked: %+v", dto)
						}
					}
				}
				if mode == "comments_page2" && page2.Load() != 3 {
					t.Fatalf("page2 not exercised for capture and both HTTP views: %d", page2.Load())
				}
			})
		}
	}
}
