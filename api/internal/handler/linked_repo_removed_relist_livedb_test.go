package handler

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD 4173 D13: a project that was linked to an issue tracker keeps that issue source after
// it is removed and listed again. The re-listed row is disabled and unlinked, and the removal
// marker clears its awaiting_first_enable, so the never-enabled gate alone lets it through.
// These tests drive the handlers and the run-creation seam over exactly that row.

// relist removes the fixture repo (the BEFORE DELETE trigger writes the removal marker) and
// lists the same forge project again, as the next membership listing does. It returns the new
// row's id; the fixture's own repo field is moved to it.
func (f *linkedHandlerFixture) relist(t *testing.T) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	var conn uuid.UUID
	var projectID int64
	if err := f.pool.QueryRow(ctx, `SELECT connection_id, forge_project_id FROM repos WHERE id = $1`, f.repo).Scan(&conn, &projectID); err != nil {
		t.Fatalf("read repo: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM repos WHERE id = $1`, f.repo); err != nil {
		t.Fatalf("remove repo: %v", err)
	}
	row, err := f.h.q.UpsertRepo(ctx, store.UpsertRepoParams{ConnectionID: conn, ForgeProjectID: projectID,
		PathWithNamespace: "g/relisted-" + uuid.NewString()[:8], WebUrl: "https://forge.test/g/l", DefaultBranch: pgtype.Text{String: "main", Valid: true}})
	if err != nil {
		t.Fatalf("relist: %v", err)
	}
	if row.AwaitingFirstEnable || row.Enabled || row.IssueTrackerConnectionID.Valid {
		t.Fatalf("re-listed row = awaiting %v enabled %v linked %v, want a disabled unlinked row the marker cleared", row.AwaitingFirstEnable, row.Enabled, row.IssueTrackerConnectionID.Valid)
	}
	f.repo = row.ID
	return row.ID
}

func TestRemovedLinkedRepoStaysSourceLockedAfterRelistLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	f.relist(t)
	ctx := context.Background()
	// A cached forge-numbered card, as a refresh would have imported it.
	if _, err := f.pool.Exec(ctx, `INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
		VALUES ($1, 5, 'forge card', 'opened', '["uzi"]', 'https://forge.test/i', now(), now())`, f.repo); err != nil {
		t.Fatalf("seed card: %v", err)
	}
	f.srv.ResetRequests()

	f.issueN = 5
	cases := []struct {
		name string
		do   http.HandlerFunc
		body any
	}{
		{"issue detail", f.h.GetIssueDetail, nil},
		{"board", f.h.GetBoard, nil},
		{"sync", f.h.SyncRepo, nil},
		{"configure columns", f.h.ConfigureColumns, map[string]any{"columns": []map[string]string{{"label_name": "x"}}}},
		{"promote", f.h.PromoteIssue, nil},
		{"create issue", f.h.CreateIssue, map[string]string{"title": "t", "description": "d"}},
		{"ensure labels", f.h.EnsureRepoLabels, map[string][]string{"labels": {"uzi"}}},
		{"move issue", f.h.MoveIssue, map[string]string{"to_column": "In Progress"}},
		{"set board order", f.h.SetBoardOrder, map[string][]int64{"iids": {5}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := f.call(tc.do, http.MethodPost, tc.body)
			if rr.Code != http.StatusConflict {
				t.Fatalf("status = %d body = %s, want the 409 source-lock refusal", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), store.ErrRepoSourceLocked.Error()) {
				t.Fatalf("body = %s, want it to carry store.ErrRepoSourceLocked's message", rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), "never been enabled") {
				t.Fatalf("body = %s, want the removed-linked wording, not the never-enabled one", rr.Body.String())
			}
		})
	}
	f.assertNothingSent(t)

	_, err := f.h.wsvc.CreateRun(ctx, f.user.ID, f.repo, 5, "", nil, nil, false, nil, nil, nil, nil, nil, nil)
	if !errors.Is(err, store.ErrRepoSourceLocked) {
		t.Fatalf("CreateRun err = %v, want store.ErrRepoSourceLocked", err)
	}
	f.assertNothingSent(t)
}

// The negative: a repo that never had an issue tracker link keeps today's behaviour when it
// is removed and listed again.
func TestRemovedUnlinkedRepoIsUsableAfterRelistLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	ctx := context.Background()
	// Make the fixture repo a plain forge repo before the removal.
	if _, err := f.pool.Exec(ctx, `UPDATE repos SET issue_tracker_connection_id = NULL, issue_tracker_project_id = NULL, issue_tracker_project_short = NULL,
		eligibility_tag_id = NULL, eligibility_tag_name = NULL, issue_tracker_linked_at = NULL WHERE id = $1`, f.repo); err != nil {
		t.Fatalf("unlink: %v", err)
	}
	f.relist(t)
	// Positive control for the "nothing sent" assertions of the locked test: an ungated
	// re-listed row does reach the forge recorder.
	for name, do := range map[string]http.HandlerFunc{"board": f.h.GetBoard, "sync": f.h.SyncRepo} {
		t.Run(name, func(t *testing.T) {
			rr := f.call(do, http.MethodGet, nil)
			if rr.Code != http.StatusOK && rr.Code != http.StatusAccepted {
				t.Fatalf("status = %d body = %s: a never-linked removed repo must keep working", rr.Code, rr.Body.String())
			}
		})
	}
	if len(f.rec.Calls()) == 0 {
		t.Fatal("the forge recorder saw no call: the positive control does not prove a re-listed row reaches it")
	}
}
