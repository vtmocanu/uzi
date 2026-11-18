package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
)

// linkedDrivers hands out the BaseFake-backed driver; every write is refused by the
// guard before it could reach it.
type linkedDrivers struct{}

func (linkedDrivers) DriverFor(_ context.Context, _, _ uuid.UUID, _ issuetracker.Binding, _, _, _ string) (issuetracker.Driver, error) {
	return &itDriver{}, nil
}

// Every filing entry point on a repo linked to an issue tracker answers 409 (never 502 or
// an "uncertain" 202), releases what it claimed, and sends nothing to the forge.
func TestLinkedRepoFilingRoutesAreRefusedAndReleaseTheirClaimLiveDB(t *testing.T) {
	f := seedNeverEnabledFixture(t)
	ctx := context.Background()
	f.h.svc.SetIssueTrackerDrivers(linkedDrivers{})
	itConn := uuid.New()
	mustExecT(ctx, t, f.pool, `INSERT INTO issue_tracker_connections (id, user_id, kind, base_url, account_id, account_login, token_ciphertext)
	      VALUES ($1, $2, 'youtrack', $3, '1-1', 'bot', $4)`, itConn, f.admin.ID, "https://"+uuid.NewString()+".yt.test", []byte{1})
	// Link the fixture's repo (the marker only ever goes true to false).
	mustExecT(ctx, t, f.pool, `UPDATE repos SET enabled = true, awaiting_first_enable = false,
	        issue_tracker_connection_id = $2, issue_tracker_project_id = '0-1', issue_tracker_project_short = 'UZI',
	        eligibility_tag_id = '5-1', eligibility_tag_name = 'uzi', issue_tracker_linked_at = now() WHERE id = $1`, f.repoID, itConn)
	repoParam := map[string]string{"id": f.repoID.String()}

	cases := []struct {
		name string
		call func(w http.ResponseWriter)
	}{
		{"FileFinding", func(w http.ResponseWriter) {
			f.h.FileFinding(w, f.request(http.MethodPost, map[string]any{}, map[string]string{"id": f.findID.String()}))
		}},
		{"FileFindingGroup", func(w http.ResponseWriter) {
			f.h.FileFindingGroup(w, f.request(http.MethodPost, map[string]any{"ids": []string{f.dispID.String()}}, nil))
		}},
		{"FileIssue", func(w http.ResponseWriter) {
			f.h.FileIssue(w, f.request(http.MethodPost, map[string]string{"repo_id": f.repoID.String(), "title": "t", "description": "d"},
				map[string]string{"id": f.runID.String(), "recID": f.recID.String()}))
		}},
		{"AdminFileJudgeIssue", func(w http.ResponseWriter) {
			f.h.AdminFileJudgeIssue(w, f.request(http.MethodPost, adminFileIssueRequest{
				Category: "improve_agent", Target: "ne-" + f.repoID.String(), RepoID: f.repoID.String(), Title: "t", Description: "d",
			}, nil))
		}},
		{"ConfigureColumns", func(w http.ResponseWriter) {
			f.h.ConfigureColumns(w, f.request(http.MethodPut, map[string]any{"columns": []map[string]string{{"label_name": "a"}}}, repoParam))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			tc.call(rr)
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "not offered yet") {
				t.Fatalf("status = %d body = %s, want the 409 refusal", rr.Code, rr.Body.String())
			}
			if n := f.hits.Load(); n != 0 {
				t.Errorf("the forge saw %d request(s), want 0", n)
			}
		})
	}

	var status string
	if err := f.pool.QueryRow(ctx, `SELECT status FROM finding_dispositions WHERE repo_id = $1`, f.repoID).Scan(&status); err != nil || status != "open" {
		t.Errorf("finding disposition = %q (%v), want open: the group claim must be released, not left uncertain", status, err)
	}
	var filed int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM recommendation_filed_issues WHERE filed_repo_id = $1`, f.repoID).Scan(&filed); err != nil || filed != 0 {
		t.Errorf("recommendation filed rows = %d (%v), want 0", filed, err)
	}
}
