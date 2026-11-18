package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/issueinput"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
)

// PRD #4173 M4 D11 through the real run capture: comment and author mapping on a linked repo.

func (f *linkedHandlerFixture) wireRuns(t *testing.T) {
	t.Helper()
	f.h.wsvc.SetForges(f.h.svc)
	f.h.wsvc.SetIssueSources(f.h.svc)
	f.seedRunCredentials(t)
}

// Acceptance 2: a comment by an unmapped author is withheld from the stored run snapshot;
// the connection account's own comment is kept.
func TestLinkedRunSnapshotWithholdsAnUnmappedAuthorsCommentLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	f.srv.SeedComment(f.issueN, youtracktest.OtherLogin, "UNMAPPED_RAW_TEXT")
	f.srv.SeedComment(f.issueN, youtracktest.BotLogin, "the account's own comment")
	f.syncOK(t)
	f.wireRuns(t)

	rr := f.call(f.h.CreateRun, http.MethodPost, map[string]any{"issue_iid": f.issueN})
	if rr.Code != http.StatusCreated {
		t.Fatalf("manual run: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var raw []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT issue_comments FROM runs WHERE repo_id = $1 AND issue_iid = $2`, f.repo, f.issueN).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "UNMAPPED_RAW_TEXT") {
		t.Fatalf("an unmapped author's comment reached the stored snapshot: %s", raw)
	}
	var th issueinput.Thread
	if err := json.Unmarshal(raw, &th); err != nil {
		t.Fatal(err)
	}
	if len(th.Comments) != 2 {
		t.Fatalf("comments = %+v, want 2", th.Comments)
	}
	if c := th.Comments[0]; c.AuthorUsername != youtracktest.OtherLogin || c.Body != issueinput.Placeholder || c.Reason != issueinput.Unknown {
		t.Fatalf("unmapped comment = %+v", c)
	}
	if c := th.Comments[1]; c.AuthorUsername != youtracktest.BotLogin || c.Body != "the account's own comment" || c.Reason != "" {
		t.Fatalf("the account's own comment = %+v", c)
	}
}

// Acceptance 3 (manual half): an unmapped issue author never stops a manual start.
func TestLinkedUnmappedIssueAuthorManualStartIsCreatedLiveDB(t *testing.T) {
	f := newLinkedHandlerFixture(t, true)
	f.srv.SetReporter(f.issueN, youtracktest.OtherLogin)
	f.syncOK(t)
	f.wireRuns(t)

	rr := f.call(f.h.CreateRun, http.MethodPost, map[string]any{"issue_iid": f.issueN})
	if rr.Code != http.StatusCreated {
		t.Fatalf("manual run on an unmapped author's issue: status = %d body = %s", rr.Code, rr.Body.String())
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM runs WHERE repo_id = $1 AND issue_iid = $2`, f.repo, f.issueN).Scan(&n); err != nil || n != 1 {
		t.Fatalf("runs = %d (%v), want 1", n, err)
	}
	// The unmapped path ran: the run records the author as permission_unknown.
	var reason *string
	if err := f.pool.QueryRow(context.Background(), `SELECT issue_input_reason FROM runs WHERE repo_id = $1 AND issue_iid = $2`, f.repo, f.issueN).Scan(&reason); err != nil || reason == nil || *reason != "permission_unknown" {
		t.Fatalf("issue_input_reason = %v (%v), want permission_unknown", reason, err)
	}
}

// Acceptance 3 (scheduled half): an unmapped author's scheduled run is created but not
// auto-approved and records permission_unknown; a mapped author's is auto-approved.
func TestLinkedUnmappedIssueAuthorScheduledRunIsNotAutoApprovedLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name        string
		reporter    string
		wantApprove bool
		wantReasons string
	}{
		{"unmapped author", youtracktest.OtherLogin, false, issueinput.Unknown},
		{"mapped author", youtracktest.BotLogin, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLinkedHandlerFixture(t, true)
			f.srv.SetReporter(f.issueN, tc.reporter)
			f.syncOK(t)
			f.wireRuns(t)

			run, err := f.h.wsvc.CreateScheduledAutopilotRun(context.Background(), f.user.ID, f.repo, f.issueN, "", nil, nil, nil, false, nil, nil)
			if err != nil {
				t.Fatalf("scheduled run: %v", err)
			}
			var approve bool
			var reasons []string
			if err := f.pool.QueryRow(context.Background(), `SELECT auto_approve, auto_approve_blocked_reasons FROM runs WHERE id = $1`, run.ID).Scan(&approve, &reasons); err != nil {
				t.Fatal(err)
			}
			if approve != tc.wantApprove || strings.Join(reasons, ",") != tc.wantReasons {
				t.Fatalf("auto_approve = %v reasons = %v, want %v / %q", approve, reasons, tc.wantApprove, tc.wantReasons)
			}
		})
	}
}
