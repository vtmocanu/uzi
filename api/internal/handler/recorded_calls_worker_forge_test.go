package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Recorded-call tests (PRD 4173 M4): for a repo with no issue-tracker link,
// pin every forge call the worker issue read routes make, for an issue run.
// The goldens were captured before the tracker-link rerouting; regenerate only
// deliberately, with UZI_UPDATE_GOLDEN=1.

// rcIssueForge answers the issue reads the three routes make.
type rcIssueForge struct{ forgetest.BaseFake }

func (*rcIssueForge) GetIssue(_ context.Context, _, iid int64) (forge.Issue, error) {
	return forge.Issue{IID: iid, Title: "t", State: "opened", Description: "body", Labels: []string{"uzi"}, AuthorForgeUserID: 2, UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}, nil
}
func (*rcIssueForge) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	return []forge.Issue{{IID: 8, Title: "t8", State: "opened"}}, nil
}
func (*rcIssueForge) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return []forge.IssueComment{{AuthorForgeUserID: 5, Body: "c"}}, nil
}
func (*rcIssueForge) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return []forge.LabelEvent{{ID: 1, Action: "add", LabelName: "uzi"}}, nil
}
func (*rcIssueForge) RepositoryAuthorEligibility(context.Context, int64, int64) (forge.AuthorEligibility, error) {
	return forge.AuthorEligible, nil
}

// rcSealedHandler wires a Handler whose worker-forge routes read an owned issue
// run (issue 7) and reach rec through the forgesvc builder seam.
func rcSealedHandler(t *testing.T, rec *forgetest.Recorder) *Handler {
	t.Helper()
	box := newForgeBox(t)
	svc := forgesvc.NewWithForgeBuilder(nil, box, time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) {
		return rec, nil
	})
	sealed, err := svc.EncryptToken("fixture")
	if err != nil {
		t.Fatal(err)
	}
	st := &forgeHandlerStore{
		ownedRun: store.Run{
			ID: uuid.New(), UserID: uuid.New(), Kind: runkind.Issue,
			RepoID:   pgtype.UUID{Bytes: uuid.New(), Valid: true},
			IssueIid: pgtype.Int8{Int64: 7, Valid: true},
		},
		connRow: store.GetRunForgeConnForWorkerRow{ForgeProjectID: forgeTestProjectID, ForgeType: "gitlab", BaseUrl: "https://forge.test", TokenCiphertext: sealed},
	}
	return &Handler{
		wsvc: workersvc.New(st, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()}),
		svc:  svc,
	}
}

func TestRecordedCallsWorkerForgeIssueRoutes(t *testing.T) {
	rec := forgetest.NewRecorder(&rcIssueForge{})
	h := rcSealedHandler(t, rec)
	run := map[string]string{"id": uuid.New().String()}
	withIID := func(iid string) map[string]string {
		return map[string]string{"id": run["id"], "iid": iid}
	}
	var lines []string
	step := func(name string, call func(w http.ResponseWriter, r *http.Request), req *http.Request) {
		t.Helper()
		before := len(rec.Calls())
		w := httptest.NewRecorder()
		call(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body %q", name, w.Code, w.Body.String())
		}
		lines = append(lines, "# "+name)
		lines = append(lines, rec.Calls()[before:]...)
	}
	step("WorkerForgeGetIssue own issue", h.WorkerForgeGetIssue, forgeReq(http.MethodGet, "/x", true, withIID("7")))
	step("WorkerForgeGetIssue other issue", h.WorkerForgeGetIssue, forgeReq(http.MethodGet, "/x", true, withIID("8")))
	step("WorkerForgeListIssues", h.WorkerForgeListIssues, forgeReq(http.MethodGet, "/x?state=opened&labels=uzi,PRD&updated_after=2026-03-04T05:06:07Z", true, run))
	step("WorkerForgeListIssueLabelEvents", h.WorkerForgeListIssueLabelEvents, forgeReq(http.MethodGet, "/x", true, withIID("8")))
	forgetest.AssertGolden(t, "recorded_calls_handler_worker_issue_routes.golden", lines)
}
