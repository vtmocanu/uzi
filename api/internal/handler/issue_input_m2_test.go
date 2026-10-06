package handler

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/store"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestM2WorkerIssueSavedFieldsFreshThread(t *testing.T) {
	for _, mode := range []string{"changed", "unchanged", "failed", "legacy", "other", "no_snapshot"} {
		t.Run(mode, func(t *testing.T) {
			body := "live APPROVED EDIT"
			_, srv := forgeMockHandler(t, map[string]http.HandlerFunc{
				"/api/v4/projects/4242/issues/11": func(w http.ResponseWriter, r *http.Request) {
					if mode == "failed" {
						http.Error(w, "RAW_SECRET_ERROR", 502)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"iid": 11, "title": "live title", "description": body, "state": "opened", "author": map[string]any{"id": 2, "username": "alice"}})
				},
				"/api/v4/projects/4242/issues/11/notes": func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewEncoder(w).Encode([]map[string]any{
						{"id": 1, "body": "late eligible", "created_at": "2026-10-06T12:00:00Z", "author": map[string]any{"id": 2, "username": "alice"}},
						{"id": 2, "body": "WITHHELD_LATE_RAW", "created_at": "2026-10-06T12:01:00Z", "author": map[string]any{"id": 3, "username": "outsider"}},
						{"id": 3, "body": "UNKNOWN_LATE_RAW", "created_at": "2026-10-06T12:02:00Z", "author": map[string]any{"id": 4, "username": "unknown"}},
						{"id": 4, "body": "OWN_BOT", "created_at": "2026-10-06T12:03:00Z", "author": map[string]any{"id": 1, "username": "bot"}},
					})
				},
				"/api/v4/users/": func(w http.ResponseWriter, r *http.Request) {
					id := strings.TrimPrefix(r.URL.Path, "/api/v4/users/")
					if id == "4" {
						http.Error(w, "RAW_SECRET_ERROR", 502)
						return
					}
					name := "alice"
					if id == "3" {
						name = "outsider"
					}
					fmt.Fprintf(w, `{"id":%s,"username":%q,"state":"active"}`, id, name)
				},
				"/api/v4/projects/4242/members/all/": func(w http.ResponseWriter, r *http.Request) {
					id := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/4242/members/all/")
					access, name := 30, "alice"
					if id == "3" {
						access = 10
						name = "outsider"
					}
					fmt.Fprintf(w, `{"id":%s,"username":%q,"state":"active","access_level":%d}`, id, name, access)
				},
			})
			box := newForgeBox(t)
			sealed, err := box.Seal([]byte("fixture-token"))
			if err != nil {
				t.Fatal(err)
			}
			st := &forgeHandlerStore{
				ownedRun: store.Run{ID: uuid.New(), RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Kind: "issue", IssueIid: pgtype.Int8{Int64: 11, Valid: true}, IssueTitle: "saved title", IssueDescription: "composed guidance", IssueSavedBody: pgtype.Text{String: "saved body", Valid: true}, IssueRawDigest: pgtype.Text{String: issueinput.Digest("saved title", "saved body"), Valid: true}},
				connRow:  store.GetRunForgeConnForWorkerRow{ForgeType: "gitlab", BaseUrl: srv.URL, TokenCiphertext: sealed, ForgeProjectID: 4242, BotForgeUserID: 1},
			}
			if mode == "unchanged" {
				st.ownedRun.IssueRawDigest.String = issueinput.Digest("live title", body)
			}
			if mode == "legacy" {
				st.ownedRun.IssueRawDigest = pgtype.Text{}
			}
			if mode == "other" {
				st.ownedRun.IssueIid.Int64 = 12
			}
			if mode == "no_snapshot" {
				st.ownedRun.Kind = "ci_fix"
				st.ownedRun.IssueIid = pgtype.Int8{}
			}
			h := newForgeHandler(t, st, box)
			rec := httptest.NewRecorder()
			h.WorkerForgeGetIssue(rec, forgeReq(http.MethodGet, "/x", true, map[string]string{"id": st.ownedRun.ID.String(), "iid": "11"}))
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var dto apitypes.ForgeIssueDTO
			if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
				t.Fatal(err)
			}
			own := mode != "other" && mode != "no_snapshot"
			if own {
				if dto.Title != "saved title" || dto.Description != "saved body" {
					t.Fatalf("saved fields replaced: %+v", dto)
				}
			} else if dto.Title != "live title" || dto.Description != body {
				t.Fatalf("eligible other content=%+v", dto)
			}
			if dto.ContentChanged != (mode == "changed") {
				t.Fatalf("changed metadata=%+v", dto)
			}
			if mode == "failed" {
				if !dto.MetadataUnavailable || !dto.CommentsUnknown || len(dto.Comments) != 0 || dto.SnapshotNote != "Current issue metadata unavailable." {
					t.Fatal(dto)
				}
			} else {
				if !dto.CommentsWithheld || !dto.CommentsUnknown || len(dto.Comments) != 3 || dto.Comments[0].Body != "late eligible" || dto.Comments[1].Reason != issueinput.NotEligible || dto.Comments[2].Reason != issueinput.Unknown {
					t.Fatalf("fresh comments=%+v", dto)
				}
			}
			for _, secret := range []string{"WITHHELD_LATE_RAW", "UNKNOWN_LATE_RAW", "RAW_SECRET_ERROR", "OWN_BOT"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Fatalf("leaked %s", secret)
				}
			}
			if mode == "legacy" && dto.SnapshotComparison != "unavailable" {
				t.Fatal(dto)
			}
		})
	}
}
