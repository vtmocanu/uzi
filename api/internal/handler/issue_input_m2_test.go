package handler

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/store"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func btoiM2(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestM2WorkerIssueSavedFieldsFreshThread(t *testing.T) {
	for _, mode := range []string{"changed", "unchanged", "failed", "legacy", "other", "no_snapshot", "other_outsider", "other_unknown", "no_snapshot_outsider", "no_snapshot_unknown"} {
		t.Run(mode, func(t *testing.T) {
			body := "live APPROVED EDIT"
			var userReads, memberReads atomic.Int64
			var promoted atomic.Bool
			authorID := int64(2)
			if strings.HasSuffix(mode, "_outsider") {
				authorID = 3
			}
			if strings.HasSuffix(mode, "_unknown") {
				authorID = 4
			}
			_, srv := forgeMockHandler(t, map[string]http.HandlerFunc{
				"/api/v4/projects/4242/issues": func(w http.ResponseWriter, r *http.Request) {
					_ = json.NewEncoder(w).Encode([]map[string]any{{"iid": 11, "title": "OWN_LIVE_TITLE"}, {"iid": 12, "title": "adjacent live title"}})
				},
				"/api/v4/projects/4242/issues/11": func(w http.ResponseWriter, r *http.Request) {
					if mode == "failed" {
						http.Error(w, "RAW_SECRET_ERROR", http.StatusBadGateway)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"iid": 11, "title": "live title", "description": body, "state": "opened", "author": map[string]any{"id": authorID, "username": "alice"}})
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
					userReads.Add(1)
					id := strings.TrimPrefix(r.URL.Path, "/api/v4/users/")
					numericID, err := strconv.ParseInt(id, 10, 64)
					if err != nil || numericID <= 0 {
						http.Error(w, "invalid id", http.StatusBadRequest)
						return
					}
					if id == "4" {
						http.Error(w, "RAW_SECRET_ERROR", http.StatusBadGateway)
						return
					}
					name := "alice"
					if id == "3" {
						name = "outsider"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": numericID, "username": name, "state": "active"})
				},
				"/api/v4/projects/4242/members/all/": func(w http.ResponseWriter, r *http.Request) {
					memberReads.Add(1)
					id := strings.TrimPrefix(r.URL.Path, "/api/v4/projects/4242/members/all/")
					numericID, err := strconv.ParseInt(id, 10, 64)
					if err != nil || numericID <= 0 {
						http.Error(w, "invalid id", http.StatusBadRequest)
						return
					}
					access, name := 30, "alice"
					if id == "3" {
						access = 10
						if promoted.Load() {
							access = 30
						}
						name = "outsider"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": numericID, "username": name, "state": "active", "access_level": access})
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
			if strings.HasPrefix(mode, "other") {
				st.ownedRun.IssueIid.Int64 = 12
			}
			if strings.HasPrefix(mode, "no_snapshot") {
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
			own := !strings.HasPrefix(mode, "other") && !strings.HasPrefix(mode, "no_snapshot")
			if own {
				if dto.Title != "saved title" || dto.Description != "saved body" {
					t.Fatalf("saved fields replaced: %+v", dto)
				}
			} else if authorID != 2 {
				want := issueinput.NotEligible
				if authorID == 4 {
					want = issueinput.Unknown
				}
				if dto.Title != issueinput.Placeholder || dto.Description != issueinput.Placeholder || dto.ContentReason != want || strings.Contains(rec.Body.String(), body) {
					t.Fatalf("untrusted other issue=%+v", dto)
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
			if own && mode == "changed" {
				list := httptest.NewRecorder()
				h.WorkerForgeListIssues(list, forgeReq(http.MethodGet, "/x", true, map[string]string{"id": st.ownedRun.ID.String()}))
				var listed apitypes.ForgeIssueListDTO
				if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
					t.Fatal(err)
				}
				if list.Code != 200 || len(listed.Items) != 2 || listed.Items[0].Title != "saved title" || listed.Items[1].Title != "adjacent live title" || strings.Contains(list.Body.String(), "OWN_LIVE_TITLE") {
					t.Fatalf("list projection=%s", list.Body.String())
				}
				digest := st.ownedRun.IssueRawDigest.String
				firstUsers, firstMembers := userReads.Load(), memberReads.Load()
				for _, approved := range []bool{false, true} {
					st.ownedRun.Status = "awaiting_approval"
					if approved {
						st.ownedRun.Status = "running"
					}
					promoted.Store(approved)
					again := httptest.NewRecorder()
					h.WorkerForgeGetIssue(again, forgeReq(http.MethodGet, "/x", true, map[string]string{"id": st.ownedRun.ID.String(), "iid": "11"}))
					var fresh apitypes.ForgeIssueDTO
					if err := json.Unmarshal(again.Body.Bytes(), &fresh); err != nil {
						t.Fatal(err)
					}
					if again.Code != 200 || fresh.Title != "saved title" || fresh.Description != "saved body" || !fresh.ContentChanged || st.ownedRun.IssueRawDigest.String != digest {
						t.Fatalf("repeated owned read=%+v", fresh)
					}
					if strings.Contains(again.Body.String(), body) || strings.Contains(again.Body.String(), "UNKNOWN_LATE_RAW") {
						t.Fatal("live issue/unknown body escaped")
					}
					if approved {
						if fresh.Comments[1].Body != "WITHHELD_LATE_RAW" || fresh.Comments[1].Reason != "" {
							t.Fatalf("permission promotion not freshly read: %+v", fresh.Comments)
						}
					} else if fresh.Comments[1].Body != issueinput.Placeholder || fresh.Comments[1].Reason != issueinput.NotEligible {
						t.Fatalf("outsider body escaped: %+v", fresh.Comments)
					}
					if userReads.Load() != firstUsers*int64(2+btoiM2(approved)) || memberReads.Load() != firstMembers*int64(2+btoiM2(approved)) {
						t.Fatalf("lookup counts users=%d members=%d first=%d/%d", userReads.Load(), memberReads.Load(), firstUsers, firstMembers)
					}
				}
			}
		})
	}
}
