package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issueinput"
)

// PRD #4173 M5: runs.origin_column of a run created on a linked repo is the card's cached
// issue tracker column, never a label, so the lifecycle's manual-drag guard compares like with
// like. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

func (f *crFix) createLinkedIssueRun(t *testing.T, iid int64, cachedLabels, cachedColumn string) string {
	t.Helper()
	f.env.exec(`INSERT INTO board_columns(repo_id, label_name, position) VALUES ($1, 'In Progress', 0), ($1, 'Later', 1)`, f.repo)
	f.env.exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at, issue_tracker_column)
	            VALUES ($1, $2, $3, 'linked card', 'opened', $4::jsonb, 'https://yt.example.com/i', false, now(), now(), NULLIF($5, ''))`,
		uuid.New(), f.repo, iid, cachedLabels, cachedColumn)
	// The fresh issue read a linked run needs is handed in on the context, as a schedule
	// fire hands its capture; it carries the eligibility tag.
	capture := &issueinput.Capture{ProjectID: 77, Issue: forge.Issue{IID: iid, Title: "linked card", Labels: []string{"uzi"}}, Thread: &issueinput.Thread{}}
	ctx := issueinput.WithCapture(f.env.ctx, capture)
	run, err := f.svc.CreateRun(ctx, f.user, f.repo, iid, "desc", nil, nil, false, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	var origin *string
	if err := f.env.pool.QueryRow(context.Background(), `SELECT origin_column FROM runs WHERE id = $1`, run.ID).Scan(&origin); err != nil {
		t.Fatal(err)
	}
	if origin == nil {
		return "<NULL>"
	}
	return *origin
}

func TestLinkedRunOriginColumnIsTheCachedColumnLiveDB(t *testing.T) {
	t.Run("a card in a mapped column", func(t *testing.T) {
		f := newCRFix(t)
		f.linkFixtureRepo(t)
		f.env.exec(`UPDATE repos SET forge_project_id = 77 WHERE id = $1`, f.repo)
		if got := f.createLinkedIssueRun(t, 93201, `["uzi"]`, "In Progress"); got != "In Progress" {
			t.Fatalf("origin_column = %q, want In Progress", got)
		}
	})
	t.Run("a card in the Open lane ignores a stray column label", func(t *testing.T) {
		f := newCRFix(t)
		f.linkFixtureRepo(t)
		f.env.exec(`UPDATE repos SET forge_project_id = 77 WHERE id = $1`, f.repo)
		if got := f.createLinkedIssueRun(t, 93202, `["uzi","Later"]`, ""); got != "" {
			t.Fatalf("origin_column = %q, want the Open lane (empty)", got)
		}
	})
	t.Run("an unlinked repo still reads the labels", func(t *testing.T) {
		f := newCRFix(t)
		f.env.exec(`INSERT INTO board_columns(repo_id, label_name, position) VALUES ($1, 'In Progress', 0), ($1, 'Later', 1)`, f.repo)
		seedEligibleIssue(t, f.env, f.repo, 93203)
		f.env.exec(`UPDATE issues SET labels = '["uzi","Later"]'::jsonb WHERE repo_id = $1 AND forge_issue_iid = 93203`, f.repo)
		run, err := f.svc.CreateRun(f.env.ctx, f.user, f.repo, 93203, "desc", nil, nil, false, nil, nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var origin string
		if err := f.env.pool.QueryRow(context.Background(), `SELECT origin_column FROM runs WHERE id = $1`, run.ID).Scan(&origin); err != nil || origin != "Later" {
			t.Fatalf("origin_column = %q (%v), want Later", origin, err)
		}
	})
}
