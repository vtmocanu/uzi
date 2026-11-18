package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD 4173 D13: every service entry point that would start a run, store an issue
// number or file an issue refuses a repo whose awaiting_first_enable marker is set,
// before any forge driver is built or called. The fakes fail the test on a forge call
// by count: buildCalls counts driver builds, createCalls counts CreateIssue.

func TestNeverEnabledRepoRefusedBeforeForge(t *testing.T) {
	ctx := context.Background()
	user, repo := uuid.New(), uuid.New()

	t.Run("CreateRun", func(t *testing.T) {
		fs := &fakeStore{repoRow: store.GetRepoForUserRow{ID: repo, AwaitingFirstEnable: true}}
		fb := &fakeForges{f: &fakeForge{}}
		svc := New(fs, newBox(t), testParams())
		svc.SetForges(fb)
		_, err := svc.CreateRun(ctx, user, repo, 7, "desc", nil, nil, false, nil, nil, nil, nil, nil, nil)
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
		if fb.buildCalls != 0 || fs.createRunParams != nil {
			t.Errorf("forge builds = %d, run created = %v; want none", fb.buildCalls, fs.createRunParams != nil)
		}
	})

	t.Run("StartRunForUser", func(t *testing.T) {
		fs := &fakeStore{repoRow: store.GetRepoForUserRow{ID: repo, AwaitingFirstEnable: true}}
		fb := &fakeForges{f: &fakeForge{}}
		svc := New(fs, newBox(t), testParams())
		svc.SetForges(fb)
		_, err := svc.StartRunForUser(ctx, user, repo, 7, nil, nil, false, nil, nil, nil, nil, nil, nil)
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
		if fb.buildCalls != 0 || fs.createRunParams != nil {
			t.Errorf("forge builds = %d, run created = %v; want none", fb.buildCalls, fs.createRunParams != nil)
		}
	})

	t.Run("CreateTaskRun continuing an issue", func(t *testing.T) {
		row := aValidRepoRow()
		row.AwaitingFirstEnable = true
		fs := &fakeStore{repoRow: row}
		fb := &fakeForges{f: &fakeForge{}}
		svc := New(fs, newBox(t), testParams())
		svc.SetForges(fb)
		iid := int64(17)
		_, err := svc.CreateTaskRun(ctx, user, repo, "task", "", false, false, false, false, &iid)
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
		if fb.buildCalls != 0 || fs.taskRunParams != nil {
			t.Errorf("forge builds = %d, run created = %v; want none", fb.buildCalls, fs.taskRunParams != nil)
		}
	})

	t.Run("CreateSelfImproveRun", func(t *testing.T) {
		fs := &fakeStore{repoRow: store.GetRepoForUserRow{ID: repo, AwaitingFirstEnable: true}}
		fb := &fakeForges{f: &fakeForge{}}
		svc := New(fs, newBox(t), testParams())
		svc.SetForges(fb)
		_, err := svc.CreateSelfImproveRun(ctx, user, repo, 7, "t", "d", nil, nil, false, nil)
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
		if fb.buildCalls != 0 || fs.selfImproveParams != nil {
			t.Errorf("forge builds = %d, run created = %v; want none", fb.buildCalls, fs.selfImproveParams != nil)
		}
	})

	t.Run("CreateTaskRun without an issue", func(t *testing.T) {
		row := aValidRepoRow()
		row.AwaitingFirstEnable = true
		fs := &fakeStore{repoRow: row}
		fb := &fakeForges{f: &fakeForge{}}
		svc := New(fs, newBox(t), testParams())
		svc.SetForges(fb)
		_, err := svc.CreateTaskRun(ctx, user, repo, "task", "", false, false, false, false, nil)
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
		if fb.buildCalls != 0 || fs.taskRunParams != nil {
			t.Errorf("forge builds = %d, run created = %v; want none", fb.buildCalls, fs.taskRunParams != nil)
		}
	})

	t.Run("CreatePromptRun", func(t *testing.T) {
		fs := &fakeStore{repoRow: store.GetRepoForUserRow{ID: repo, AwaitingFirstEnable: true}}
		fb := &fakeForges{f: &fakeForge{}}
		svc := New(fs, newBox(t), testParams())
		svc.SetForges(fb)
		_, err := svc.CreatePromptRun(ctx, user, repo, uuid.New(), "t", "p", false, false, nil, nil, false, nil, nil)
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
		if fb.buildCalls != 0 {
			t.Errorf("forge builds = %d, want 0", fb.buildCalls)
		}
	})

	t.Run("CreateCIFixRun", func(t *testing.T) {
		fs := &fakeStore{repoRow: store.GetRepoForUserRow{ID: repo, AwaitingFirstEnable: true}}
		fb := &fakeForges{f: &fakeForge{}}
		svc := New(fs, newBox(t), testParams())
		svc.SetForges(fb)
		_, err := svc.CreateCIFixRun(ctx, user, repo, "main", "t", "d", FailureSnapshot{}, nil)
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
		if fb.buildCalls != 0 {
			t.Errorf("forge builds = %d, want 0", fb.buildCalls)
		}
	})

	t.Run("createMRReworkRun", func(t *testing.T) {
		fs := &fakeStore{repoRow: store.GetRepoForUserRow{ID: repo, AwaitingFirstEnable: true}}
		svc := New(fs, newBox(t), testParams())
		_, err := svc.createMRReworkRun(ctx, user, repo, "ref", 1, uuid.New(), "t", "d", nil, "", nil, nil, nil)
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
	})

	t.Run("CreateProposal", func(t *testing.T) {
		fs := &fakeStore{repoRow: store.GetRepoForUserRow{ID: repo, AwaitingFirstEnable: true}, runByID: store.Run{Kind: runkind.Chat, Status: "running"}}
		svc := New(fs, newBox(t), testParams())
		_, err := svc.CreateProposal(ctx, store.Worker{UserID: user}, uuid.New(), repo, "t", "d", []byte(`[]`))
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
		if fs.createdProposal != nil {
			t.Error("a proposal was stored for a never-enabled repo")
		}
	})

	t.Run("ConfirmProposalForUser reverts the claim", func(t *testing.T) {
		svc, fs, fb := confirmFixture(t)
		fs.repoRow.AwaitingFirstEnable = true
		_, err := svc.ConfirmProposalForUser(ctx, user, uuid.New(), uuid.New())
		if !errors.Is(err, ErrRepoNeverEnabled) {
			t.Fatalf("err = %v, want ErrRepoNeverEnabled", err)
		}
		if fb.buildCalls != 0 || fb.f.createCalls != 0 {
			t.Errorf("forge builds = %d, CreateIssue calls = %d; want none", fb.buildCalls, fb.f.createCalls)
		}
		if len(fs.revertedProposals) != 1 {
			t.Errorf("reverted proposals = %d, want the claim returned to pending", len(fs.revertedProposals))
		}
	})

	t.Run("maybeFileProposal", func(t *testing.T) {
		svc, fs, ff, run := proposalFixture(t, "feature-bingo")
		fs.repoRow.AwaitingFirstEnable = true
		svc.maybeFileProposal(ctx, run, aProposal())
		if ff.createCalls != 0 || fs.createdProposal != nil {
			t.Errorf("CreateIssue calls = %d, audit row = %v; want nothing filed", ff.createCalls, fs.createdProposal != nil)
		}
	})
}

// A UZ413 from the database guard maps to the same typed error.
func TestMapNeverEnabledGuardError(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "UZ413", Message: "repo has never been enabled"}
	if err := store.MapNeverEnabled(pgErr); !errors.Is(err, ErrRepoNeverEnabled) {
		t.Fatalf("MapNeverEnabled(UZ413) = %v, want ErrRepoNeverEnabled", err)
	}
	other := &pgconn.PgError{Code: "23505"}
	if err := store.MapNeverEnabled(other); errors.Is(err, ErrRepoNeverEnabled) {
		t.Fatal("a unique violation was mapped to ErrRepoNeverEnabled")
	}
}
