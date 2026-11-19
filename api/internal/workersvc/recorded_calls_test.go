package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Recorded-call tests (PRD 4173 M4): for a repo with no issue-tracker link, a
// manual issue run captures its input (GetIssue, ListIssueComments, and one
// RepositoryAuthorEligibility check per distinct author, the issue author
// included) and makes no other forge call. The goldens were captured before the tracker-link rerouting;
// regenerate only deliberately, with UZI_UPDATE_GOLDEN=1.

func recordedRunFixture(t *testing.T) (*Service, *forgetest.Recorder, *fakeStore) {
	t.Helper()
	fs := &fakeStore{
		repoRow:         store.GetRepoForUserRow{ForgeProjectID: 7, BotForgeUserID: 1},
		issueByID:       store.Issue{Labels: uziLabels()},
		createRunResult: store.Run{ID: uuid.New()},
	}
	f := &m2Forge{
		disposition: forge.AuthorEligible,
		comments: []forge.IssueComment{
			{AuthorForgeUserID: 2, Body: "first"},
			{AuthorForgeUserID: 5, Body: "second"},
			{AuthorForgeUserID: 5, Body: "third"},
		},
	}
	rec := forgetest.NewRecorder(f)
	svc := New(fs, newBox(t), testParams())
	svc.SetForges(m2Builder{rec})
	return svc, rec, fs
}

// TestRecordedCallsCreateRunManual drives CreateRun directly, so the input
// capture runs inside it (no capture handed in through the context).
func TestRecordedCallsCreateRunManual(t *testing.T) {
	svc, rec, fs := recordedRunFixture(t)
	if _, err := svc.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "caller raw body", nil, nil, false, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if fs.createRunParams == nil {
		t.Fatal("CreateRun did not persist a run")
	}
	forgetest.AssertGolden(t, "recorded_calls_workersvc_createrun.golden", rec.Calls())
}

// TestRecordedCallsStartRunForUser drives the manual start composite, which
// captures the issue input once and hands it to CreateRun.
func TestRecordedCallsStartRunForUser(t *testing.T) {
	svc, rec, fs := recordedRunFixture(t)
	if _, err := svc.StartRunForUser(context.Background(), uuid.New(), uuid.New(), 4, nil, nil, false, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if fs.createRunParams == nil {
		t.Fatal("StartRunForUser did not persist a run")
	}
	forgetest.AssertGolden(t, "recorded_calls_workersvc_startrunforuser.golden", rec.Calls())
}
