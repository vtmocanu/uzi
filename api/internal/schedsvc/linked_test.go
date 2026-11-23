package schedsvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuesource"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M4 (DL-5): a schedule on a repo linked to an external issue tracker reads
// issues through the resolver; the forge is never used for them, and a scheduler with
// no resolver fails closed for a linked repo.

type fakeSources struct {
	tr    forge.IssueTracker
	err   error
	users []uuid.UUID
	repos []uuid.UUID
}

func (f *fakeSources) IssueTrackerForRepo(_ context.Context, userID, repoID uuid.UUID) (forge.IssueTracker, error) {
	f.users = append(f.users, userID)
	f.repos = append(f.repos, repoID)
	return f.tr, f.err
}

// linkedTagName is the eligibility tag name of linkedHarness's repo, deliberately not
// the configured uzi label.
const linkedTagName = "ready-for-uzi"

// linkedHarness is newHarness with the repo linked and a resolver whose tracker is a
// separate fake from the forge, so a read shows which side served it.
func linkedHarness(withSources bool) (*harness, *fakeForge, *fakeSources) {
	h := newHarness()
	h.st.repoRow.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	h.st.repoRow.EligibilityTagName = pgtype.Text{String: linkedTagName, Valid: true}
	tr := &fakeForge{issue: forge.Issue{IID: 7, Description: "tracker body", Labels: []string{"PRD"}}, createdIID: 7}
	src := &fakeSources{tr: tr}
	if withSources {
		h.sched.SetIssueSources(src)
	}
	return h, tr, src
}

func TestLinkedFireIssueReadsThroughResolver(t *testing.T) {
	h, tr, src := linkedHarness(true)
	out, err := h.sched.RunNow(context.Background(), h.issueSchedule())
	if err != nil || len(out.Started) != 1 {
		t.Fatalf("fire did not start a run: out=%+v err=%v", out, err)
	}
	if len(tr.getIID) != 1 || len(h.fb.f.getIID) != 0 {
		t.Fatalf("the issue was read from the forge (%v) instead of the resolver (%v)", h.fb.f.getIID, tr.getIID)
	}
	if len(src.users) != 1 || src.users[0] != h.owner || src.repos[0] != h.repoID {
		t.Fatalf("the resolver is bound by owner and repo row, got %v %v", src.users, src.repos)
	}
}

func TestLinkedFireSweepReadsThroughResolver(t *testing.T) {
	h, tr, _ := linkedHarness(true)
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}, {ForgeIssueIid: 2}}
	out, err := h.sched.RunNow(context.Background(), h.sweepSchedule(pgtype.Int4{Int32: 2, Valid: true}))
	if err != nil || len(out.Started) != 2 {
		t.Fatalf("sweep did not start two runs: out=%+v err=%v", out, err)
	}
	if len(tr.getIID) != 2 || len(h.fb.f.getIID) != 0 {
		t.Fatalf("sweep read forge=%v resolver=%v", h.fb.f.getIID, tr.getIID)
	}
}

func TestLinkedFireWithoutSourcesFailsClosed(t *testing.T) {
	for name, sched := range map[string]func(*harness) store.RunSchedule{
		"issue": func(h *harness) store.RunSchedule { return h.issueSchedule() },
		"sweep": func(h *harness) store.RunSchedule { return h.sweepSchedule(pgtype.Int4{}) },
	} {
		t.Run(name, func(t *testing.T) {
			h, _, _ := linkedHarness(false)
			h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}}
			_, err := h.sched.fireOne(context.Background(), sched(h), false)
			if err == nil {
				t.Fatal("a linked repo with no resolver fired")
			}
			if len(h.fb.f.getIID) != 0 || h.runsCreated() != 0 {
				t.Fatalf("fell back to the forge: GetIssue=%v runs=%d", h.fb.f.getIID, h.runsCreated())
			}
		})
	}
}

func TestLinkedProposalDigestReadsThroughResolver(t *testing.T) {
	h, tr, _ := linkedHarness(true)
	tr.listResult = []forge.Issue{{IID: 3, Title: "an earlier proposal", State: "opened"}}
	h.fb.f.listResult = []forge.Issue{{IID: 9, Title: "forge issue", State: "opened"}}
	got := h.sched.proposalDigestSection(context.Background(), h.issueSchedule())
	if tr.listCount != 1 || h.fb.f.listCount != 0 || got == "" {
		t.Fatalf("resolver lists=%d forge lists=%d digest=%q", tr.listCount, h.fb.f.listCount, got)
	}
}

func TestLinkedProposalDigestWithoutSourcesIsSkipped(t *testing.T) {
	h, _, _ := linkedHarness(false)
	h.fb.f.listResult = []forge.Issue{{IID: 9, Title: "forge issue", State: "opened"}}
	if got := h.sched.proposalDigestSection(context.Background(), h.issueSchedule()); got != "" || h.fb.f.listCount != 0 {
		t.Fatalf("digest=%q forge lists=%d: a linked repo must not list the forge", got, h.fb.f.listCount)
	}
}

// PRD #4173 M5 (converted from TestLinkedSelfImproveIsSkippedWithoutAnyIssueWrite, the M4
// refusal): the tracking issue is created through the resolver's issue side, never the
// forge, carries only the tracking label (no eligibility tag), and the cycle starts.
func TestLinkedSelfImproveFilesTheTrackingIssueThroughTheResolver(t *testing.T) {
	h, tr, src := linkedHarness(true)
	tr.createdIID = 501
	out, err := h.sched.fireOne(context.Background(), h.selfImproveSchedule(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Started) != 1 || len(h.runs.selfImprove) != 1 {
		t.Fatalf("out = %+v, runs = %d, want one started self_improve run", out, len(h.runs.selfImprove))
	}
	if tr.listCount != 1 || tr.createCount != 1 || h.fb.f.createCount != 0 || h.fb.f.listCount != 0 {
		t.Fatalf("resolver list/create = %d/%d, forge list/create = %d/%d, want 1/1 and 0/0",
			tr.listCount, tr.createCount, h.fb.f.listCount, h.fb.f.createCount)
	}
	if len(tr.createdLabels) != 1 || tr.createdLabels[0] != SelfImproveTrackingLabel {
		t.Fatalf("tracking issue labels = %v, want only %q", tr.createdLabels, SelfImproveTrackingLabel)
	}
	if len(src.users) == 0 || src.users[0] != h.owner || src.repos[0] != h.repoID {
		t.Fatalf("the resolver is bound by owner and repo row, got %v %v", src.users, src.repos)
	}
}

// PRD #4173 M5 review: the tracking label is uzi's own marker. When it is also the link's
// eligibility name the issue would be created eligible, so the cycle is refused before the
// forge, the resolver, the tracking lookup and any write, and no run starts. A cycle whose
// names differ (the control) still files the tracking issue.
func TestLinkedSelfImproveRefusesWhenTheTrackingLabelIsTheEligibilityName(t *testing.T) {
	h, tr, src := linkedHarness(true)
	h.st.repoRow.EligibilityTagName = pgtype.Text{String: SelfImproveTrackingLabel, Valid: true}
	_, err := h.sched.fireOne(context.Background(), h.selfImproveSchedule(), false)
	if !errors.Is(err, issuesource.ErrMarkerConfersEligibility) {
		t.Fatalf("err = %v, want ErrMarkerConfersEligibility", err)
	}
	if errors.Is(err, errBadConfig) {
		t.Fatalf("err = %v: the refusal must be transient so the cycle fires once a name changes", err)
	}
	if tr.listCount != 0 || tr.createCount != 0 || h.fb.f.listCount != 0 || h.fb.f.createCount != 0 || len(src.users) != 0 {
		t.Fatalf("an issue tracker or forge call was made before the refusal: resolver list/create = %d/%d, forge = %d/%d, resolver binds = %d",
			tr.listCount, tr.createCount, h.fb.f.listCount, h.fb.f.createCount, len(src.users))
	}
	if len(h.runs.selfImprove) != 0 {
		t.Fatalf("a self_improve run started: %d", len(h.runs.selfImprove))
	}
}

func TestLinkedSelfImproveWithoutSourcesFailsClosedAndNeverTouchesTheForge(t *testing.T) {
	h, _, _ := linkedHarness(false)
	if _, err := h.sched.fireOne(context.Background(), h.selfImproveSchedule(), false); err == nil {
		t.Fatal("a linked repo with no resolver fired")
	}
	if h.fb.f.createCount != 0 || h.fb.f.listCount != 0 || len(h.runs.selfImprove) != 0 {
		t.Fatal("self_improve fell back to the forge or started a run on an unresolved linked repo")
	}
}

func TestUnlinkedFireNeverConsultsResolver(t *testing.T) {
	h := newHarness()
	src := &fakeSources{tr: &fakeForge{}}
	h.sched.SetIssueSources(src)
	if _, err := h.sched.RunNow(context.Background(), h.issueSchedule()); err != nil {
		t.Fatal(err)
	}
	if len(src.users) != 0 || len(h.fb.f.getIID) != 1 {
		t.Fatalf("resolver calls=%d forge GetIssue=%v", len(src.users), h.fb.f.getIID)
	}
}

// PRD #4173 D14: a linked repo's sweep selects by the stored eligibility tag name,
// for the default selector and for the SQL eligibility label of both queries.
func TestLinkedFireSweepSelectsByTheStoredEligibilityTag(t *testing.T) {
	h, _, _ := linkedHarness(true)
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}}
	sched := h.sweepSchedule(pgtype.Int4{})
	sched.Labels = nil
	if _, err := h.sched.RunNow(context.Background(), sched); err != nil {
		t.Fatal(err)
	}
	if got := string(h.st.sweepLabelParam); got != `["`+linkedTagName+`"]` {
		t.Fatalf("empty selector resolved to %s, want the stored eligibility tag", got)
	}
	if h.st.sweepUziLabelParam != linkedTagName || h.st.countUziLabelParam != linkedTagName {
		t.Fatalf("eligibility label list=%q count=%q, want %q", h.st.sweepUziLabelParam, h.st.countUziLabelParam, linkedTagName)
	}
}

func TestLinkedFireSweepKeepsAnExplicitSelector(t *testing.T) {
	h, _, _ := linkedHarness(true)
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}}
	sched := h.sweepSchedule(pgtype.Int4{})
	sched.Labels = []byte(`["triage"]`)
	if _, err := h.sched.RunNow(context.Background(), sched); err != nil {
		t.Fatal(err)
	}
	if got := string(h.st.sweepLabelParam); got != `["triage"]` || h.st.sweepUziLabelParam != linkedTagName {
		t.Fatalf("selector = %s, eligibility = %q", got, h.st.sweepUziLabelParam)
	}
}

func TestUnlinkedFireSweepStillUsesTheConfiguredUziLabel(t *testing.T) {
	h := newHarness()
	h.st.sweepRows = []store.ListSweepCandidateIssuesRow{{ForgeIssueIid: 1}}
	sched := h.sweepSchedule(pgtype.Int4{})
	sched.Labels = nil
	if _, err := h.sched.RunNow(context.Background(), sched); err != nil {
		t.Fatal(err)
	}
	if got := string(h.st.sweepLabelParam); strings.Contains(got, linkedTagName) || h.st.sweepUziLabelParam == linkedTagName || h.st.sweepUziLabelParam == "" {
		t.Fatalf("selector = %s eligibility = %q", got, h.st.sweepUziLabelParam)
	}
}

func TestLinkedFireSweepWithNoStoredTagIsABadConfigNotAForgeLabel(t *testing.T) {
	h, _, _ := linkedHarness(true)
	h.st.repoRow.EligibilityTagName = pgtype.Text{}
	sched := h.sweepSchedule(pgtype.Int4{})
	sched.Labels = nil
	if _, err := h.sched.fireSweep(context.Background(), sched, false); !errors.Is(err, errBadConfig) {
		t.Fatalf("err = %v, want errBadConfig", err)
	}
}
