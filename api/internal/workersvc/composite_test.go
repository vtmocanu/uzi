package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeForge is a forge.Forge stub: it embeds the interface (so the 20 unused methods
// panic if ever called) and overrides only GetIssue/CreateIssue, counting the
// CreateIssue calls so a test can prove exactly-once.
type fakeForge struct {
	forge.Forge
	createCalls    int
	createErr      error
	created        forge.Issue
	getIssueResult forge.Issue
	getIssueErr    error
}

func (f *fakeForge) CreateIssue(_ context.Context, _ int64, title, _ string, _ []string) (forge.Issue, error) {
	f.createCalls++
	if f.createErr != nil {
		return forge.Issue{}, f.createErr
	}
	iss := f.created
	iss.Title = title
	return iss, nil
}

func (f *fakeForge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return f.getIssueResult, f.getIssueErr
}

// fakeForges is a ForgeBuilder stub. buildErr fails the build; otherwise it hands
// back the same *fakeForge so a test can inspect the calls the flow made.
type fakeForges struct {
	f          *fakeForge
	buildErr   error
	buildCalls int
}

func (fb *fakeForges) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	fb.buildCalls++
	if fb.buildErr != nil {
		return nil, fb.buildErr
	}
	return fb.f, nil
}

// confirmFixture wires a Service with a claimable pending proposal and an owned repo,
// the happy-path preconditions every ConfirmProposalForUser test starts from and then
// perturbs.
func confirmFixture(t *testing.T) (*Service, *fakeStore, *fakeForges) {
	t.Helper()
	fs := &fakeStore{
		claimProposalRow: store.ClaimProposalForConfirmRow{
			ID:          uuid.New(),
			RunID:       uuid.New(),
			RepoID:      uuid.New(),
			Title:       "Add retries to the poller",
			Description: "the poller should back off and retry",
			Labels:      []byte(`["bug"]`),
		},
		repoRow: store.GetRepoForUserRow{
			ID:              uuid.New(),
			ForgeProjectID:  42,
			ForgeType:       "gitlab",
			BaseUrl:         "https://gitlab.example",
			TokenCiphertext: []byte("ciphertext"),
		},
	}
	fb := &fakeForges{f: &fakeForge{created: forge.Issue{IID: 7, WebURL: "https://gitlab.example/-/issues/7"}}}
	svc := New(fs, newBox(t), testParams())
	svc.SetForges(fb)
	return svc, fs, fb
}

// TestConfirmProposalForUserHappyPath: the claim, the forge write and the settle all
// succeed, the created issue rides back, and nothing reverts.
func TestConfirmProposalForUserHappyPath(t *testing.T) {
	svc, fs, fb := confirmFixture(t)

	created, err := svc.ConfirmProposalForUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if err != nil {
		t.Fatalf("ConfirmProposalForUser: %v", err)
	}
	if created.IID != 7 {
		t.Errorf("created.IID = %d, want 7", created.IID)
	}
	if fb.f.createCalls != 1 {
		t.Errorf("CreateIssue calls = %d, want 1", fb.f.createCalls)
	}
	if len(fs.revertedProposals) != 0 {
		t.Errorf("happy path must not revert; got %d reverts", len(fs.revertedProposals))
	}
	if fs.markedProposalConfirm == nil {
		t.Fatal("expected the proposal to settle to confirmed")
	}
	if got := fs.markedProposalConfirm.CreatedIssueIid.Int64; got != 7 {
		t.Errorf("settled iid = %d, want 7", got)
	}
}

// TestConfirmProposalForUserRevertsOnEachPostClaimFailure: every failure AFTER the
// claim (repo gone, forge build, forge CreateIssue) reverts the proposal to pending
// and surfaces the right sentinel. This is the M1 composition net — one case per
// post-claim failure point.
func TestConfirmProposalForUserRevertsOnEachPostClaimFailure(t *testing.T) {
	forgeDown := errors.New("build failed")
	writeDown := errors.New("gitlab 500 (token=[REDACTED])")

	cases := []struct {
		name    string
		perturb func(*fakeStore, *fakeForges)
		wantErr error
		// wantCreate is whether CreateIssue should have been reached before failing.
		wantCreate int
	}{
		{
			name:       "repo gone",
			perturb:    func(fs *fakeStore, _ *fakeForges) { fs.repoErr = pgx.ErrNoRows },
			wantErr:    ErrProposalRepoGone,
			wantCreate: 0,
		},
		{
			name:       "forge build fails",
			perturb:    func(_ *fakeStore, fb *fakeForges) { fb.buildErr = forgeDown },
			wantErr:    ErrForgeBuild,
			wantCreate: 0,
		},
		{
			name:       "forge CreateIssue fails",
			perturb:    func(_ *fakeStore, fb *fakeForges) { fb.f.createErr = writeDown },
			wantErr:    ErrForgeIssueWrite,
			wantCreate: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, fs, fb := confirmFixture(t)
			tc.perturb(fs, fb)

			_, err := svc.ConfirmProposalForUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if len(fs.revertedProposals) != 1 {
				t.Errorf("expected exactly one revert-to-pending, got %d", len(fs.revertedProposals))
			}
			if fb.f.createCalls != tc.wantCreate {
				t.Errorf("CreateIssue calls = %d, want %d", fb.f.createCalls, tc.wantCreate)
			}
			if fs.markedProposalConfirm != nil {
				t.Errorf("a failed confirm must not settle the proposal to confirmed")
			}
		})
	}
}

// TestConfirmProposalForUserClaimGatesTheWrite: when the claim does not win (the
// proposal is no longer pending — a concurrent confirm already claimed it), the flow
// NEVER reaches the forge, so of two racing confirms exactly one CreateIssue happens.
// This is the observable invariant claim-first ordering enforces: remove the claim
// gate and the second confirm would create a duplicate issue and redden this.
func TestConfirmProposalForUserClaimGatesTheWrite(t *testing.T) {
	svc, fs, fb := confirmFixture(t)

	// First confirm wins the claim and creates the issue.
	if _, err := svc.ConfirmProposalForUser(context.Background(), uuid.New(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("first confirm: %v", err)
	}
	// Second confirm loses the claim (already confirming/resolved).
	fs.claimProposalErr = ErrProposalNotPending
	_, err := svc.ConfirmProposalForUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if !errors.Is(err, ErrProposalNotPending) {
		t.Fatalf("second confirm err = %v, want ErrProposalNotPending", err)
	}
	if fb.f.createCalls != 1 {
		t.Errorf("CreateIssue calls across two confirms = %d, want 1 (the claim gates the write)", fb.f.createCalls)
	}
	// A lost claim reverts nothing — it never held the proposal.
	if len(fs.revertedProposals) != 0 {
		t.Errorf("a lost claim must not revert; got %d", len(fs.revertedProposals))
	}
}

// TestConfirmProposalForUserNoForgeBuilder: an unwired service refuses rather than
// panicking, and never touches the proposal.
func TestConfirmProposalForUserNoForgeBuilder(t *testing.T) {
	fs := &fakeStore{}
	svc := New(fs, newBox(t), testParams())
	// SetForges deliberately not called.

	_, err := svc.ConfirmProposalForUser(context.Background(), uuid.New(), uuid.New(), uuid.New())
	if !errors.Is(err, ErrForgesUnavailable) {
		t.Fatalf("err = %v, want ErrForgesUnavailable", err)
	}
}

// TestStartRunForUserForgeReadError: a forge GetIssue failure surfaces as
// ErrForgeIssueRead (→ 502) and no run is created.
func TestStartRunForUserForgeReadError(t *testing.T) {
	fs := &fakeStore{repoRow: store.GetRepoForUserRow{ID: uuid.New(), ForgeProjectID: 1, ForgeType: "gitlab", BaseUrl: "https://x"}}
	fb := &fakeForges{f: &fakeForge{getIssueErr: errors.New("gitlab 404 (token=[REDACTED])")}}
	svc := New(fs, newBox(t), testParams())
	svc.SetForges(fb)

	_, err := svc.StartRunForUser(context.Background(), uuid.New(), uuid.New(), 5, nil, nil)
	if !errors.Is(err, ErrForgeIssueRead) {
		t.Fatalf("err = %v, want ErrForgeIssueRead", err)
	}
	if fs.createRunParams != nil {
		t.Error("a forge read failure must not create a run")
	}
}

// TestStartRunForUserRepoNotOwned: an unknown/foreign repo is ErrRepoNotFound before
// any forge call.
func TestStartRunForUserRepoNotOwned(t *testing.T) {
	fs := &fakeStore{repoErr: pgx.ErrNoRows}
	fb := &fakeForges{f: &fakeForge{}}
	svc := New(fs, newBox(t), testParams())
	svc.SetForges(fb)

	_, err := svc.StartRunForUser(context.Background(), uuid.New(), uuid.New(), 5, nil, nil)
	if !errors.Is(err, ErrRepoNotFound) {
		t.Fatalf("err = %v, want ErrRepoNotFound", err)
	}
	if fb.buildCalls != 0 {
		t.Error("a repo the user does not own must not reach the forge")
	}
}

// TestPrdlessAllows exercises the PRDLESS gate the lift reads off settings: on only
// when the feature is enabled AND the issue carries the configured label.
func TestPrdlessAllows(t *testing.T) {
	cases := []struct {
		name     string
		settings *fakeSettings
		labels   []string
		want     bool
	}{
		{"nil settings → gated", nil, []string{"prdless"}, false},
		{"disabled → gated", &fakeSettings{prdlessEnabled: false, prdlessLabel: "prdless"}, []string{"prdless"}, false},
		{"enabled, label absent → gated", &fakeSettings{prdlessEnabled: true, prdlessLabel: "prdless"}, []string{"bug"}, false},
		{"enabled, label present → bypass", &fakeSettings{prdlessEnabled: true, prdlessLabel: "prdless"}, []string{"bug", "prdless"}, true},
		{"enabled, empty label → gated", &fakeSettings{prdlessEnabled: true, prdlessLabel: ""}, []string{""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := New(&fakeStore{}, newBox(t), testParams())
			if tc.settings != nil {
				svc.SetSettings(*tc.settings)
			}
			if got := svc.prdlessAllows(context.Background(), tc.labels); got != tc.want {
				t.Errorf("prdlessAllows = %v, want %v", got, tc.want)
			}
		})
	}
}
