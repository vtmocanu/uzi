package poller

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M5 U6: the poller's issue comments (CI autofix start and halt, MR rework
// halt) and the PRD link patch reach the YouTrack fake through the real resolver, the
// write guard and the YouTrack driver. The ledgers and the code host are the unit-test
// fakes; the repo row, the resolver and the issue tracker are live. A YouTrack 500 is not
// something the fake can answer, so that case wraps the live tracker in a failing one.

const linkedCommentsProject = 42 // the forge_project_id newLinkedEnv gives the linked repo

// failingNoteTracker is a live tracker whose comment write answers a 500.
type failingNoteTracker struct{ forge.IssueTracker }

func (failingNoteTracker) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	return forge.IssueNote{}, errors.New("create issue note: youtrack answered 500")
}

// linkedCommentFixture is a linked repo with one eligible YouTrack issue n.
func linkedCommentFixture(t *testing.T) (env *linkedEnv, srv *youtracktest.Server, tr forge.IssueTracker, n int64) {
	t.Helper()
	srv = youtracktest.NewServer(t)
	trustFake(t, srv)
	env = newLinkedEnv(t, srv, nil)
	n = srv.SeedIssue("a card", "Implements prds/72-x.md end to end.", youtracktest.EligibilityTagID)
	var err error
	tr, err = env.svc.IssueTrackerForRepo(env.ctx, env.user, env.repo)
	if err != nil {
		t.Fatal(err)
	}
	return env, srv, tr, n
}

func youtrackComments(t *testing.T, tr forge.IssueTracker, n int64) []string {
	t.Helper()
	cs, err := tr.ListIssueComments(context.Background(), linkedCommentsProject, n)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

func branchFor(n int64) string { return "agent/issue-" + strconv.FormatInt(n, 10) }

func ciCand(ref string, pipelineID int64) store.ListCIAutofixCandidateRefsRow {
	c := cfCand(pipelineID)
	c.Ref = pgtype.Text{String: ref, Valid: true}
	return c
}

func TestLinkedCIAutofixStartCommentLandsOnTheYouTrackIssueLiveDB(t *testing.T) {
	_, _, tr, n := linkedCommentFixture(t)
	ref := branchFor(n)
	st := &cfStore{candidates: []store.ListCIAutofixCandidateRefsRow{ciCand(ref, 9001)}}
	runs := &cfRuns{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "panic: boom"}

	newCF(st, runs, &cfNotifier{}).detect(context.Background(), cfRepoRow(), f, tr)

	if len(runs.calls) != 1 {
		t.Fatalf("runs started = %d, want 1", len(runs.calls))
	}
	got := youtrackComments(t, tr, n)
	if len(got) != 1 || got[0] != startCommentBody(ref, cfCand(9001).PipelineWebUrl) {
		t.Fatalf("YouTrack comments = %q, want the start comment", got)
	}
	if len(f.notes) != 0 {
		t.Fatalf("the code host received an issue note: %+v", f.notes)
	}
}

func TestLinkedCIAutofixHaltCommentLandsOnTheYouTrackIssueLiveDB(t *testing.T) {
	_, _, tr, n := linkedCommentFixture(t)
	ref := branchFor(n)
	st := &cfStore{
		candidates: []store.ListCIAutofixCandidateRefsRow{ciCand(ref, 9010)},
		attempts: map[string]store.CiAutofixAttempt{
			ref: {Ref: ref, AttemptCount: 2, LastPipelineID: pgtype.Int8{Int64: 9001, Valid: true}},
		},
	}
	runs := &cfRuns{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, runs, &cfNotifier{}).detect(context.Background(), cfRepoRow(), f, tr)

	if len(runs.calls) != 0 || len(st.haltSets) != 1 {
		t.Fatalf("runs=%d halt latches=%d, want a halt", len(runs.calls), len(st.haltSets))
	}
	got := youtrackComments(t, tr, n)
	want := haltCommentBody(haltCap, 2, cfCand(9010).MrIid)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("YouTrack comments = %q, want the halt comment", got)
	}
}

// A moved-away issue: the comment is not written, the failure is logged, the run was
// already started and the halt latch is set.
func TestLinkedCIAutofixCommentsOnAMovedAwayIssueAreLoggedAndTheRunContinuesLiveDB(t *testing.T) {
	_, srv, tr, n := linkedCommentFixture(t)
	ref := branchFor(n)
	srv.MoveIssue(n, youtracktest.OtherProjectID)
	logs := captureLogs(t, slog.LevelDebug)

	st := &cfStore{candidates: []store.ListCIAutofixCandidateRefsRow{ciCand(ref, 9001)}}
	runs := &cfRuns{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "panic: boom"}
	newCF(st, runs, &cfNotifier{}).detect(context.Background(), cfRepoRow(), f, tr)
	if len(runs.calls) != 1 || len(st.upserts) != 1 {
		t.Fatalf("the run must start whatever the comment does: runs=%d upserts=%d", len(runs.calls), len(st.upserts))
	}
	if !strings.Contains(logs.String(), "ci-autofix start comment") {
		t.Fatalf("the failed start comment was not logged: %s", logs.String())
	}

	halt := &cfStore{
		candidates: []store.ListCIAutofixCandidateRefsRow{ciCand(ref, 9010)},
		attempts:   map[string]store.CiAutofixAttempt{ref: {Ref: ref, AttemptCount: 2}},
	}
	notifier := &cfNotifier{}
	newCF(halt, &cfRuns{}, notifier).detect(context.Background(), cfRepoRow(), f, tr)
	if len(halt.haltSets) != 1 || len(notifier.calls) != 1 {
		t.Fatalf("the halt must complete: latches=%d notifications=%d", len(halt.haltSets), len(notifier.calls))
	}
	if !strings.Contains(logs.String(), "ci-autofix halt comment") {
		t.Fatalf("the failed halt comment was not logged: %s", logs.String())
	}
}

// A failed comment write (a stub standing in for the tracker; youtracktest cannot
// answer a 500) is logged and the run continues.
func TestLinkedCIAutofixCommentFailureIsLoggedAndTheRunContinuesLiveDB(t *testing.T) {
	_, _, tr, n := linkedCommentFixture(t)
	logs := captureLogs(t, slog.LevelDebug)
	st := &cfStore{candidates: []store.ListCIAutofixCandidateRefsRow{ciCand(branchFor(n), 9001)}}
	runs := &cfRuns{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "panic: boom"}

	newCF(st, runs, &cfNotifier{}).detect(context.Background(), cfRepoRow(), f, failingNoteTracker{tr})

	if len(runs.calls) != 1 || len(st.upserts) != 1 {
		t.Fatalf("runs=%d upserts=%d, want the run started", len(runs.calls), len(st.upserts))
	}
	if !strings.Contains(logs.String(), "ci-autofix start comment") || !strings.Contains(logs.String(), "500") {
		t.Fatalf("the 500 was not logged: %s", logs.String())
	}
}

func mrReworkCap(ref string) *mrwStore {
	cand := mrwCand("success")
	cand.Ref = pgtype.Text{String: ref, Valid: true}
	return &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{cand},
		ledgers:    map[string]store.GetMRReworkLedgerRow{ref: mrwRow(ref, 5, 120)},
	}
}

func TestLinkedMRReworkHaltCommentLandsOnTheYouTrackIssueLiveDB(t *testing.T) {
	_, _, tr, n := linkedCommentFixture(t)
	st := mrReworkCap(branchFor(n))
	runs := &mrwRuns{}
	f := landedForge(mrwComment(200, landed(), mrwHeadSHA))

	newMRW(st, runs, &mrwNotifier{}, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f, tr)

	if len(runs.calls) != 0 || len(st.haltSets) != 1 {
		t.Fatalf("runs=%d latches=%d, want a halt", len(runs.calls), len(st.haltSets))
	}
	got := youtrackComments(t, tr, n)
	if len(got) != 1 || got[0] != mrReworkHaltCommentBody(5, mrwMrIID) {
		t.Fatalf("YouTrack comments = %q, want the rework halt comment", got)
	}
	if len(f.notes) != 0 {
		t.Fatalf("the code host received an issue note: %+v", f.notes)
	}
}

func TestLinkedMRReworkHaltCommentOnAMovedAwayIssueIsLoggedAndTheHaltContinuesLiveDB(t *testing.T) {
	_, srv, tr, n := linkedCommentFixture(t)
	srv.MoveIssue(n, youtracktest.OtherProjectID)
	logs := captureLogs(t, slog.LevelDebug)
	st := mrReworkCap(branchFor(n))
	notifier := &mrwNotifier{}
	f := landedForge(mrwComment(200, landed(), mrwHeadSHA))

	newMRW(st, &mrwRuns{}, notifier, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f, tr)

	if len(st.haltSets) != 1 || len(notifier.calls) != 1 {
		t.Fatalf("the halt must complete: latches=%d notifications=%d", len(st.haltSets), len(notifier.calls))
	}
	if !strings.Contains(logs.String(), "mr-rework halt comment") {
		t.Fatalf("the failed comment was not logged: %s", logs.String())
	}
}

// A merged MR's moved PRD path patches the YouTrack description, and the marker settles.
func TestLinkedPRDLinkPatchWritesTheYouTrackDescriptionLiveDB(t *testing.T) {
	env, _, tr, n := linkedCommentFixture(t)
	if _, err := env.pool.Exec(env.ctx, `UPDATE runs SET issue_iid = $1 WHERE issue_iid = 8 AND repo_id = $2`, n, env.repo); err != nil {
		t.Fatal(err)
	}

	if err := env.svc.SyncPRDLinkPatches(env.ctx, env.repo, linkedCommentsProject, env.rec, tr); err != nil {
		t.Fatal(err)
	}

	iss, err := tr.GetIssue(env.ctx, linkedCommentsProject, n)
	if err != nil {
		t.Fatal(err)
	}
	if iss.Description != "Implements prds/done/72-x.md end to end." {
		t.Fatalf("YouTrack description = %q, want the patched path", iss.Description)
	}
	var settled pgtype.Timestamptz
	if err := env.pool.QueryRow(env.ctx, `SELECT prd_patch_settled_at FROM runs WHERE issue_iid = $1 AND repo_id = $2`, n, env.repo).Scan(&settled); err != nil || !settled.Valid {
		t.Fatalf("the marker was not settled: %+v, %v", settled, err)
	}
}

// The issue moved away: the patch reads first, settles on the read, and never writes.
func TestLinkedPRDLinkPatchSettlesOnAMovedAwayIssueLiveDB(t *testing.T) {
	env, srv, tr, n := linkedCommentFixture(t)
	if _, err := env.pool.Exec(env.ctx, `UPDATE runs SET issue_iid = $1 WHERE issue_iid = 8 AND repo_id = $2`, n, env.repo); err != nil {
		t.Fatal(err)
	}
	srv.MoveIssue(n, youtracktest.OtherProjectID)
	srv.ResetRequests()

	if err := env.svc.SyncPRDLinkPatches(env.ctx, env.repo, linkedCommentsProject, env.rec, tr); err != nil {
		t.Fatal(err)
	}

	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			t.Fatalf("a write reached YouTrack for a moved issue: %+v", r)
		}
	}
	var settled pgtype.Timestamptz
	if err := env.pool.QueryRow(env.ctx, `SELECT prd_patch_settled_at FROM runs WHERE issue_iid = $1 AND repo_id = $2`, n, env.repo).Scan(&settled); err != nil || !settled.Valid {
		t.Fatalf("the marker was not settled on the moved-away issue: %+v, %v", settled, err)
	}
}
