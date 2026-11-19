package workersvc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M4 (DL-5): on a repo linked to an external issue tracker every issue-side
// call of this package goes through the resolver; the code host (author eligibility)
// stays on the forge; a service with no resolver fails closed instead of using the forge.

// ytTracker stands in for a linked repo's issue tracker: its issues and comments carry
// no forge user ids, as the YouTrack driver's do.
type ytTracker struct {
	forgetest.BaseFake
}

func (*ytTracker) GetIssue(_ context.Context, _, iid int64) (forge.Issue, error) {
	// The fresh read carries the eligibility tag under its stored name, as the driver's does.
	return forge.Issue{IID: iid, Title: "yt title", Description: "yt body", State: "opened", Labels: []string{linkedEligibilityName}}, nil
}
func (*ytTracker) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return []forge.IssueComment{{Body: "yt comment"}}, nil
}

// createRecorder is a linked repo's issue tracker that records the CreateIssue it receives.
type createRecorder struct {
	forgetest.BaseFake
	createErr    error
	created      forge.Issue
	creates      int
	createdTitle string
	createdLabel []string
}

func (c *createRecorder) CreateIssue(_ context.Context, _ int64, title, _ string, labels []string) (forge.Issue, error) {
	c.creates++
	c.createdTitle = title
	c.createdLabel = labels
	return c.created, c.createErr
}

type fakeSources struct {
	tr    forge.IssueTracker
	err   error
	calls []struct{ user, repo uuid.UUID }
}

func (f *fakeSources) IssueTrackerForRepo(_ context.Context, userID, repoID uuid.UUID) (forge.IssueTracker, error) {
	f.calls = append(f.calls, struct{ user, repo uuid.UUID }{userID, repoID})
	return f.tr, f.err
}

// linkedEligibilityName is the eligibility tag name of linkedRepoRow. It equals the
// default uzi label only so the shared uziLabels() cache fixture is eligible; the
// run gate reads the stored name on a linked repo, never the configured label.
const linkedEligibilityName = settings.DefaultUziLabel

func linkedRepoRow() store.GetRepoForUserRow {
	return store.GetRepoForUserRow{
		ID: uuid.New(), ForgeProjectID: 7, BotForgeUserID: 1,
		ForgeType: "gitlab", BaseUrl: "https://gitlab.example", TokenCiphertext: []byte("sealed"),
		IssueTrackerConnectionID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		EligibilityTagName:       pgtype.Text{String: linkedEligibilityName, Valid: true},
	}
}

func hasCall(calls []string, prefix string) bool {
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// linkedFixture builds a Service whose repo row is linked, with a recorded forge (the
// code host) and a recorded issue tracker behind the resolver.
func linkedRunFixture(t *testing.T, withSources bool) (*Service, *fakeStore, *forgetest.Recorder, *forgetest.Recorder, *fakeSources) {
	t.Helper()
	fs := &fakeStore{repoRow: linkedRepoRow(), issueByID: store.Issue{Labels: uziLabels()}, createRunResult: store.Run{ID: uuid.New()}}
	forgeRec := forgetest.NewRecorder(&m2Forge{disposition: forge.AuthorEligible})
	trRec := forgetest.NewRecorder(&ytTracker{})
	svc := New(fs, newBox(t), testParams())
	svc.SetForges(m2Builder{forgeRec})
	src := &fakeSources{tr: trRec}
	if withSources {
		svc.SetIssueSources(src)
	}
	return svc, fs, forgeRec, trRec, src
}

func TestLinkedRunCaptureUsesResolverAndNeverAsksForgeAboutTrackerAuthors(t *testing.T) {
	sites := map[string]func(*Service, uuid.UUID, uuid.UUID) error{
		"CreateRun": func(s *Service, user, repo uuid.UUID) error {
			_, err := s.CreateRun(context.Background(), user, repo, 4, "caller body", nil, nil, false, nil, nil, nil, nil, nil, nil)
			return err
		},
		"StartRunForUser": func(s *Service, user, repo uuid.UUID) error {
			_, err := s.StartRunForUser(context.Background(), user, repo, 4, nil, nil, false, nil, nil, nil, nil, nil, nil)
			return err
		},
	}
	for name, run := range sites {
		t.Run(name, func(t *testing.T) {
			svc, _, forgeRec, trRec, src := linkedRunFixture(t, true)
			user, repo := uuid.New(), uuid.New()
			if err := run(svc, user, repo); err != nil {
				t.Fatal(err)
			}
			if !hasCall(trRec.Calls(), "GetIssue(") || !hasCall(trRec.Calls(), "ListIssueComments(") {
				t.Fatalf("the resolver's tracker was not read: %v", trRec.Calls())
			}
			if hasCall(forgeRec.Calls(), "GetIssue(") || hasCall(forgeRec.Calls(), "ListIssueComments(") {
				t.Fatalf("the forge was read for a linked repo's issue: %v", forgeRec.Calls())
			}
			if hasCall(forgeRec.Calls(), "RepositoryAuthorEligibility(") {
				t.Fatalf("the forge was asked about an issue tracker author: %v", forgeRec.Calls())
			}
			if len(src.calls) != 1 || src.calls[0].user != user {
				t.Fatalf("the resolver must be asked once for the owner, got %+v", src.calls)
			}
		})
	}
}

func TestLinkedRunCaptureWithoutSourcesFailsClosed(t *testing.T) {
	for name, run := range map[string]func(*Service) error{
		"CreateRun": func(s *Service) error {
			_, err := s.CreateRun(context.Background(), uuid.New(), uuid.New(), 4, "b", nil, nil, false, nil, nil, nil, nil, nil, nil)
			return err
		},
		"StartRunForUser": func(s *Service) error {
			_, err := s.StartRunForUser(context.Background(), uuid.New(), uuid.New(), 4, nil, nil, false, nil, nil, nil, nil, nil, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			svc, fs, forgeRec, _, _ := linkedRunFixture(t, false)
			err := run(svc)
			if !errors.Is(err, ErrForgeBuild) {
				t.Fatalf("err = %v, want ErrForgeBuild", err)
			}
			if fs.createRunParams != nil {
				t.Fatal("a run was created for an unresolvable linked repo")
			}
			if hasCall(forgeRec.Calls(), "GetIssue(") {
				t.Fatalf("fell back to the forge: %v", forgeRec.Calls())
			}
		})
	}
}

func TestUnlinkedRunCaptureNeverConsultsResolver(t *testing.T) {
	svc, _, _, _, src := linkedRunFixture(t, true)
	svc.q.(*fakeStore).repoRow.IssueTrackerConnectionID = pgtype.UUID{}
	if _, err := svc.StartRunForUser(context.Background(), uuid.New(), uuid.New(), 4, nil, nil, false, nil, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(src.calls) != 0 {
		t.Fatalf("an unlinked repo consulted the resolver: %+v", src.calls)
	}
}

// PRD #4173 M5 (converted from TestLinkedConfirmProposalIsRefusedAndReverts, which modelled the
// M4 guard's refusal): a confirmed chat proposal is created through the resolver's issue side
// with the uzi label mapped to the stored eligibility name and the autopilot label dropped.
func TestLinkedConfirmProposalCreatesThroughTheResolverWithTheEligibilityTag(t *testing.T) {
	svc, fs, fb := confirmFixture(t)
	fs.repoRow.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	fs.repoRow.EligibilityTagName = pgtype.Text{String: "ready-for-uzi", Valid: true}
	fs.claimProposalRow.Labels = []byte(`["bug","uzi","autopilot"]`)
	tr := &createRecorder{created: forge.Issue{IID: 9, WebURL: "https://yt.test/UZI-9", Title: "t"}}
	src := &fakeSources{tr: tr}
	svc.SetIssueSources(src)

	created, err := svc.ConfirmProposalForUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if err != nil || created.IID != 9 {
		t.Fatalf("created = %+v, err = %v, want issue 9", created, err)
	}
	if want := []string{"bug", "ready-for-uzi"}; !slices.Equal(tr.createdLabel, want) {
		t.Fatalf("labels = %v, want %v", tr.createdLabel, want)
	}
	if fb.f.createCalls != 0 {
		t.Fatal("the forge's CreateIssue was called for a linked repo")
	}
	if len(fs.revertedProposals) != 0 || fs.markedProposalConfirm == nil || fs.markedProposalConfirm.CreatedIssueIid.Int64 != 9 {
		t.Fatalf("the proposal must settle confirmed with iid 9: reverts=%d", len(fs.revertedProposals))
	}
	if len(src.calls) != 1 {
		t.Fatalf("resolver calls = %d, want 1", len(src.calls))
	}
}

// A confirmed proposal without the uzi label carries no eligibility tag.
func TestLinkedConfirmProposalWithoutTheUziLabelCarriesNoEligibilityTag(t *testing.T) {
	svc, fs, _ := confirmFixture(t)
	fs.repoRow.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	fs.repoRow.EligibilityTagName = pgtype.Text{String: "ready-for-uzi", Valid: true}
	tr := &createRecorder{created: forge.Issue{IID: 9}}
	svc.SetIssueSources(&fakeSources{tr: tr})

	if _, err := svc.ConfirmProposalForUser(context.Background(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if want := []string{"bug"}; !slices.Equal(tr.createdLabel, want) {
		t.Fatalf("labels = %v, want %v", tr.createdLabel, want)
	}
}

// An issue tracker failure on a linked repo still reverts the claim to pending.
func TestLinkedConfirmProposalFailureReverts(t *testing.T) {
	svc, fs, fb := confirmFixture(t)
	fs.repoRow.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	fs.repoRow.EligibilityTagName = pgtype.Text{String: "ready-for-uzi", Valid: true}
	svc.SetIssueSources(&fakeSources{tr: &createRecorder{createErr: errors.New("youtrack unreachable")}})

	_, err := svc.ConfirmProposalForUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if !errors.Is(err, ErrForgeIssueWrite) {
		t.Fatalf("err = %v, want ErrForgeIssueWrite", err)
	}
	if fb.f.createCalls != 0 || len(fs.revertedProposals) != 1 || fs.markedProposalConfirm != nil {
		t.Fatalf("forge creates=%d reverts=%d: the claim must revert", fb.f.createCalls, len(fs.revertedProposals))
	}
}

func TestLinkedConfirmProposalWithoutSourcesFailsClosed(t *testing.T) {
	svc, fs, fb := confirmFixture(t)
	fs.repoRow.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}

	_, err := svc.ConfirmProposalForUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if !errors.Is(err, ErrForgeBuild) {
		t.Fatalf("err = %v, want ErrForgeBuild", err)
	}
	if fb.f.createCalls != 0 || len(fs.revertedProposals) != 1 {
		t.Fatalf("forge creates=%d reverts=%d, want 0 and 1", fb.f.createCalls, len(fs.revertedProposals))
	}
}

// PRD #4173 M5 (converted from TestLinkedRepoFilesNoScheduledProposal, the M4 skip): a scheduled
// proposal is filed through the resolver's issue side with the proposal marker only (no
// eligibility tag), and the audit row is stamped.
func TestLinkedRepoFilesTheScheduledProposalThroughTheResolver(t *testing.T) {
	svc, fs, ff, run := proposalFixture(t, "feature-bingo")
	fs.repoRow.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	fs.repoRow.EligibilityTagName = pgtype.Text{String: "ready-for-uzi", Valid: true}
	tr := &createRecorder{created: forge.Issue{IID: 11}}
	src := &fakeSources{tr: tr}
	svc.SetIssueSources(src)

	svc.maybeFileProposal(context.Background(), run, aProposal())

	if ff.createCalls != 0 {
		t.Fatal("a proposal was filed on the forge for a linked repo")
	}
	if want := []string{"proposal::feature-bingo"}; tr.creates != 1 || !slices.Equal(tr.createdLabel, want) {
		t.Fatalf("creates = %d labels = %v, want one create with %v", tr.creates, tr.createdLabel, want)
	}
	if fs.createdProposal == nil || fs.stampedProposal == nil || fs.stampedProposal.Iid.Int64 != 11 {
		t.Fatalf("the audit row must be inserted and stamped with iid 11: %+v %+v", fs.createdProposal, fs.stampedProposal)
	}
	if len(src.calls) != 1 || src.calls[0].user != run.UserID {
		t.Fatalf("the resolver is bound by the run's owner, got %+v", src.calls)
	}
}

// With no resolver a linked repo files nothing, falls back to nothing, and leaves no
// pending audit row.
func TestLinkedRepoScheduledProposalWithoutSourcesFilesNothing(t *testing.T) {
	svc, fs, ff, run := proposalFixture(t, "feature-bingo")
	fs.repoRow.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}

	svc.maybeFileProposal(context.Background(), run, aProposal())

	if ff.createCalls != 0 || fs.createdProposal != nil {
		t.Fatalf("forge creates = %d, audit row = %v: nothing may be filed", ff.createCalls, fs.createdProposal)
	}
}

func TestLinkedContinuedIssueUsesResolverOrFailsClosed(t *testing.T) {
	iid := int64(17)
	newSvc := func(withSources bool) (*Service, *linkedTaskForge, *linkedTaskForge, *fakeStore) {
		row := aValidRepoRow()
		row.ForgeProjectID = 621
		row.IssueTrackerConnectionID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		fs := &fakeStore{repoRow: row}
		svc := New(fs, newBox(t), testParams())
		forgeSide := &linkedTaskForge{issue: forge.Issue{IID: iid, State: "opened"}}
		trSide := &linkedTaskForge{issue: forge.Issue{IID: iid, State: "opened"}}
		svc.SetForges(m2Builder{forgeSide})
		if withSources {
			svc.SetIssueSources(&fakeSources{tr: trSide})
		}
		return svc, forgeSide, trSide, fs
	}
	t.Run("resolver", func(t *testing.T) {
		svc, forgeSide, trSide, fs := newSvc(true)
		if _, err := svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "task", "", false, false, false, false, &iid); err != nil {
			t.Fatal(err)
		}
		if trSide.calls != 1 || forgeSide.calls != 0 || fs.taskRunParams == nil {
			t.Fatalf("resolver calls=%d forge calls=%d persisted=%v", trSide.calls, forgeSide.calls, fs.taskRunParams != nil)
		}
	})
	t.Run("no resolver", func(t *testing.T) {
		svc, forgeSide, _, fs := newSvc(false)
		_, err := svc.CreateTaskRun(context.Background(), uuid.New(), uuid.New(), "task", "", false, false, false, false, &iid)
		if !errors.Is(err, ErrForgeBuild) || forgeSide.calls != 0 || fs.taskRunParams != nil {
			t.Fatalf("err=%v forge calls=%d persisted=%v", err, forgeSide.calls, fs.taskRunParams != nil)
		}
	})
}

func TestLinkedSemanticIssueReadUsesResolverOrFailsClosed(t *testing.T) {
	iid := int64(4)
	build := func(withSources bool) (*Service, *driftFakeForge, *driftFakeForge) {
		run := store.Run{ID: uuid.New(), UserID: uuid.New(), RepoID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, Kind: runkind.Issue, IssueIid: pgtype.Int8{Int64: iid, Valid: true}}
		fs := &forgeConnStore{ownedRun: run, connRow: store.GetRunForgeConnForWorkerRow{
			RepoID: uuid.UUID(run.RepoID.Bytes), UserID: run.UserID, ForgeProjectID: 9, ForgeType: "gitlab", BaseUrl: "https://gl.test",
			IssueTrackerConnectionID: pgtype.UUID{Bytes: uuid.New(), Valid: true},
		}}
		svc := New(fs, newBox(t), Params{})
		forgeSide := &driftFakeForge{issue: forge.Issue{Description: "forge text"}}
		trSide := &driftFakeForge{issue: forge.Issue{Description: "tracker text"}}
		svc.SetForges(m2Builder{forgeSide})
		if withSources {
			svc.SetIssueSources(&fakeSources{tr: trSide})
		}
		return svc, forgeSide, trSide
	}
	svc, forgeSide, trSide := build(true)
	got, err := svc.currentSemanticIssue(context.Background(), worker(), uuid.New())
	if err != nil || got.Text != "tracker text" || trSide.calls != 1 || forgeSide.calls != 0 {
		t.Fatalf("got=%+v err=%v tracker calls=%d forge calls=%d", got, err, trSide.calls, forgeSide.calls)
	}
	svc, forgeSide, _ = build(false)
	got, err = svc.currentSemanticIssue(context.Background(), worker(), uuid.New())
	if err != nil || got.Reason != "unreadable" || forgeSide.calls != 0 {
		t.Fatalf("no resolver: got=%+v err=%v forge calls=%d", got, err, forgeSide.calls)
	}
}

func TestForgeConnForRunCarriesTheRunsIssueBinding(t *testing.T) {
	run := runWithRepoID()
	link := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	repoID, owner := uuid.New(), uuid.New()
	fs := &forgeConnStore{ownedRun: run, connRow: store.GetRunForgeConnForWorkerRow{RepoID: repoID, UserID: owner, IssueTrackerConnectionID: link}}
	conn, err := New(fs, newBox(t), Params{}).ForgeConnForRun(context.Background(), worker(), uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if conn.RepoID != repoID || conn.UserID != owner || conn.IssueTrackerConnectionID != link {
		t.Fatalf("conn = %+v", conn)
	}
}
