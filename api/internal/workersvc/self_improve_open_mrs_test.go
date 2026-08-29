package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// siOpenMRHarness wires a Service with a fake store + fake forge and a run/claim-context
// pair, the happy-path preconditions the selfImproveOpenMRs (PRD #686 D11/D12) cases start
// from. forges is wired unless the caller wants the nil-forges branch.
func siOpenMRHarness(t *testing.T) (*Service, *fakeStore, *fakeForge, store.Run, store.GetRunClaimContextRow) {
	t.Helper()
	repoID := uuid.New()
	fs := &fakeStore{}
	svc := New(fs, newBox(t), testParams())
	f := &fakeForge{mrStateByIID: map[int64]string{}}
	svc.SetForges(&fakeForges{f: f})
	run := store.Run{ID: uuid.New(), RepoID: pgtype.UUID{Bytes: repoID, Valid: true}}
	rc := store.GetRunClaimContextRow{
		ForgeProjectID:  42,
		ForgeType:       "gitlab",
		BaseUrl:         "https://gitlab.example",
		TokenCiphertext: []byte("ciphertext"),
	}
	return svc, fs, f, run, rc
}

// siRow builds a candidate self_improve run row with an mr_iid and an issue_description
// (plan_md left NULL, the autopilot self_improve shape — issue_description is the
// effective proposed-text source).
func siRow(id uuid.UUID, mrIID int64, issueDescription string) store.RecentSelfImproveMRRunsForRepoRow {
	return store.RecentSelfImproveMRRunsForRepoRow{
		ID:               id,
		MrIid:            pgtype.Int8{Int64: mrIID, Valid: true},
		IssueDescription: issueDescription,
	}
}

// TestSelfImproveOpenMRsNilForges pins the best-effort nil-forges branch: an unwired
// Service returns nil (never panics, never queries), so a claim on a server without a
// forge builder still assembles.
func TestSelfImproveOpenMRsNilForges(t *testing.T) {
	fs := &fakeStore{}
	svc := New(fs, newBox(t), testParams())
	// SetForges deliberately NOT called.
	run := store.Run{ID: uuid.New(), RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
	got := svc.selfImproveOpenMRs(context.Background(), run, store.GetRunClaimContextRow{})
	if got != nil {
		t.Fatalf("nil forges must yield nil, got %v", got)
	}
	// It must not even reach the DB query.
	if len(fs.recentSIMRRepos) != 0 {
		t.Fatalf("nil forges must not query the DB, got %d calls", len(fs.recentSIMRRepos))
	}
}

// TestSelfImproveOpenMRsOpenCandidateRendered pins the core case: a candidate whose MR the
// forge reports OPEN contributes the FIRST non-empty line of its issue_description (plan_md
// NULL) to the returned slice, and the query is scoped to the run's repo.
func TestSelfImproveOpenMRsOpenCandidateRendered(t *testing.T) {
	svc, fs, f, run, rc := siOpenMRHarness(t)
	fs.recentSIMRRuns = []store.RecentSelfImproveMRRunsForRepoRow{
		siRow(uuid.New(), 101, "Add retries to the poller\nand back off exponentially"),
	}
	f.mrStateByIID[101] = forge.MRStateOpened

	got := svc.selfImproveOpenMRs(context.Background(), run, rc)
	if len(got) != 1 || got[0] != "Add retries to the poller" {
		t.Fatalf("open candidate lines = %v, want the first line only", got)
	}
	// Scoped to the run's repo.
	if len(fs.recentSIMRRepos) != 1 || fs.recentSIMRRepos[0] != uuid.UUID(run.RepoID.Bytes) {
		t.Fatalf("candidate query repos = %v, want [%v]", fs.recentSIMRRepos, uuid.UUID(run.RepoID.Bytes))
	}
}

// TestSelfImproveOpenMRsPlanMdWins pins the proposed-text source precedence: when plan_md
// is present and non-blank, its first line is used INSTEAD of issue_description.
func TestSelfImproveOpenMRsPlanMdWins(t *testing.T) {
	svc, fs, f, run, rc := siOpenMRHarness(t)
	row := siRow(uuid.New(), 101, "issue-desc line")
	row.PlanMd = pgtype.Text{String: "Refactor the claim assembler\nsecond line", Valid: true}
	fs.recentSIMRRuns = []store.RecentSelfImproveMRRunsForRepoRow{row}
	f.mrStateByIID[101] = forge.MRStateOpened

	got := svc.selfImproveOpenMRs(context.Background(), run, rc)
	if len(got) != 1 || got[0] != "Refactor the claim assembler" {
		t.Fatalf("lines = %v, want the plan_md first line (not issue_description)", got)
	}
	if strings.Contains(strings.Join(got, "|"), "issue-desc") {
		t.Fatalf("plan_md must win over issue_description: %v", got)
	}
}

// TestSelfImproveOpenMRsExcludesNonOpenAndSelf pins the three exclusions that keep the
// picker context to genuinely-open, other-cycle MRs: a MERGED MR and a CLOSED MR are
// dropped, the current run's OWN row is dropped, and a row without an mr_iid is dropped.
// Only the single OPEN, other-run, mr-bearing candidate survives.
func TestSelfImproveOpenMRsExcludesNonOpenAndSelf(t *testing.T) {
	svc, fs, f, run, rc := siOpenMRHarness(t)
	// The current run appears in the window (recent MR-bearing self_improve run) and must
	// be excluded even though its MR would read as opened.
	selfRow := siRow(run.ID, 100, "MY OWN proposal")
	openRow := siRow(uuid.New(), 101, "keep: the open one")
	mergedRow := siRow(uuid.New(), 102, "drop: merged")
	closedRow := siRow(uuid.New(), 103, "drop: closed")
	noMRRow := store.RecentSelfImproveMRRunsForRepoRow{ID: uuid.New(), IssueDescription: "drop: no mr_iid"} // MrIid invalid
	fs.recentSIMRRuns = []store.RecentSelfImproveMRRunsForRepoRow{selfRow, openRow, mergedRow, closedRow, noMRRow}
	f.mrStateByIID = map[int64]string{
		100: forge.MRStateOpened,
		101: forge.MRStateOpened,
		102: forge.MRStateMerged,
		103: forge.MRStateClosed,
	}

	got := svc.selfImproveOpenMRs(context.Background(), run, rc)
	if len(got) != 1 || got[0] != "keep: the open one" {
		t.Fatalf("lines = %v, want only the open, other-run candidate", got)
	}
	// The self row and the no-mr row are excluded BEFORE the forge call (never fetched);
	// merged/closed ARE fetched but then dropped.
	if contains(f.getMRIID, 100) {
		t.Fatalf("the current run's own MR must be excluded before the forge call: %v", f.getMRIID)
	}
	for _, want := range []int64{101, 102, 103} {
		if !contains(f.getMRIID, want) {
			t.Fatalf("MR %d should have been checked live: %v", want, f.getMRIID)
		}
	}
}

// TestSelfImproveOpenMRsPerCandidateErrorSkipsOnly pins the best-effort per-candidate
// posture: a GetMergeRequest error on ONE candidate skips only that candidate and the loop
// continues, so the other open candidates still render and the claim never fails.
func TestSelfImproveOpenMRsPerCandidateErrorSkipsOnly(t *testing.T) {
	svc, fs, f, run, rc := siOpenMRHarness(t)
	fs.recentSIMRRuns = []store.RecentSelfImproveMRRunsForRepoRow{
		siRow(uuid.New(), 201, "before the error"),
		siRow(uuid.New(), 202, "the errored one"),
		siRow(uuid.New(), 203, "after the error"),
	}
	f.mrStateByIID = map[int64]string{201: forge.MRStateOpened, 203: forge.MRStateOpened}
	f.mrErrByIID = map[int64]error{202: errors.New("forge 502")}

	got := svc.selfImproveOpenMRs(context.Background(), run, rc)
	if len(got) != 2 {
		t.Fatalf("lines = %v, want the two non-errored open candidates", got)
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "before the error") || !strings.Contains(joined, "after the error") {
		t.Fatalf("both non-errored candidates must render: %v", got)
	}
	if strings.Contains(joined, "the errored one") {
		t.Fatalf("the errored candidate must be skipped, not rendered: %v", got)
	}
}

// TestSelfImproveOpenMRsCapsOutput pins the cap: even with more open candidates than
// maxOpenSelfImproveMRs, at most that many lines are returned.
func TestSelfImproveOpenMRsCapsOutput(t *testing.T) {
	svc, fs, f, run, rc := siOpenMRHarness(t)
	var rows []store.RecentSelfImproveMRRunsForRepoRow
	for i := int64(0); i < maxOpenSelfImproveMRs+3; i++ {
		iid := 300 + i
		rows = append(rows, siRow(uuid.New(), iid, "proposal "+string(rune('A'+i))))
		f.mrStateByIID[iid] = forge.MRStateOpened
	}
	fs.recentSIMRRuns = rows

	got := svc.selfImproveOpenMRs(context.Background(), run, rc)
	if len(got) != maxOpenSelfImproveMRs {
		t.Fatalf("lines = %d, want capped at %d", len(got), maxOpenSelfImproveMRs)
	}
}

// TestSelfImproveOpenMRsQueryErrorYieldsNil pins the best-effort DB-error branch: a failing
// candidate query yields nil rather than failing the claim.
func TestSelfImproveOpenMRsQueryErrorYieldsNil(t *testing.T) {
	svc, fs, _, run, rc := siOpenMRHarness(t)
	fs.recentSIMRRunsErr = errors.New("db down")
	if got := svc.selfImproveOpenMRs(context.Background(), run, rc); got != nil {
		t.Fatalf("a query error must yield nil, got %v", got)
	}
}

func contains(xs []int64, v int64) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
