package privcheck

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
)

// roleResult / protResult script the per-project forge answers.
type roleResult struct {
	role   int
	member bool
	err    error
}
type protResult struct {
	bp  forge.BranchProtection
	err error
}

// fakeForge is a scriptable Forge for the checker tests. Only the four methods
// the checker calls carry behavior; the rest satisfy the interface with zeros.
type fakeForge struct {
	identity  forge.BotIdentity
	verifyErr error
	tokenInfo forge.TokenInfo
	tokenErr  error
	roles     map[int64]roleResult
	prots     map[int64]protResult
}

func (f *fakeForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return f.identity, f.verifyErr
}
func (f *fakeForge) TokenInfo(context.Context) (forge.TokenInfo, error) {
	return f.tokenInfo, f.tokenErr
}
func (f *fakeForge) ProjectRole(_ context.Context, projectID, _ int64) (int, bool, error) {
	r := f.roles[projectID]
	return r.role, r.member, r.err
}
func (f *fakeForge) DefaultBranchProtection(_ context.Context, projectID int64, _ string, _ int64) (forge.BranchProtection, error) {
	p := f.prots[projectID]
	return p.bp, p.err
}

// Unused-by-checker methods.
func (f *fakeForge) ListProjects(context.Context) ([]forge.Project, error) { return nil, nil }
func (f *fakeForge) ListLabels(context.Context, int64) ([]forge.Label, error) {
	return nil, nil
}
func (f *fakeForge) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }
func (f *fakeForge) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	return nil, nil
}
func (f *fakeForge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (f *fakeForge) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, nil
}
func (f *fakeForge) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return nil
}
func (f *fakeForge) GetMergeRequest(context.Context, int64, int64) (forge.MergeRequest, error) {
	return forge.MergeRequest{}, nil
}

var now = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

func hasFinding(list []string, substr string) bool {
	for _, s := range list {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

func TestEvaluateToken(t *testing.T) {
	warnWindow := 14 * 24 * time.Hour
	soon := now.Add(7 * 24 * time.Hour)
	farOff := now.Add(365 * 24 * time.Hour)
	expired := now.Add(-time.Hour)

	cases := []struct {
		name          string
		info          forge.TokenInfo
		isAdmin       bool
		wantViolation string // substring expected in violations, "" if none
		wantWarning   string // substring expected in warnings, "" if none
	}{
		{"exactly api is clean", forge.TokenInfo{Scopes: []string{"api"}, Active: true}, false, "", ""},
		{"extra scope is a violation", forge.TokenInfo{Scopes: []string{"api", "sudo"}, Active: true}, false, "are not exactly [api]", ""},
		{"read_api instead of api is a violation", forge.TokenInfo{Scopes: []string{"read_api"}, Active: true}, false, "are not exactly [api]", ""},
		{"instance admin is a violation", forge.TokenInfo{Scopes: []string{"api"}, Active: true}, true, "instance admin", ""},
		{"inactive is a violation", forge.TokenInfo{Scopes: []string{"api"}, Active: false}, false, "not active", ""},
		{"expired is a violation", forge.TokenInfo{Scopes: []string{"api"}, Active: true, ExpiresAt: expired}, false, "has expired", ""},
		{"expiring soon is a warning", forge.TokenInfo{Scopes: []string{"api"}, Active: true, ExpiresAt: soon}, false, "", "expires within 14 days"},
		{"far-off expiry is clean", forge.TokenInfo{Scopes: []string{"api"}, Active: true, ExpiresAt: farOff}, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := evaluateToken(tc.info, tc.isAdmin, now, warnWindow, "")
			if tc.wantViolation == "" && len(tr.Violations) > 0 {
				t.Errorf("unexpected violations: %v", tr.Violations)
			}
			if tc.wantViolation != "" && !hasFinding(tr.Violations, tc.wantViolation) {
				t.Errorf("violations %v missing %q", tr.Violations, tc.wantViolation)
			}
			if tc.wantWarning == "" && len(tr.Warnings) > 0 {
				t.Errorf("unexpected warnings: %v", tr.Warnings)
			}
			if tc.wantWarning != "" && !hasFinding(tr.Warnings, tc.wantWarning) {
				t.Errorf("warnings %v missing %q", tr.Warnings, tc.wantWarning)
			}
		})
	}
}

// TestCheckTokenIntrospectionUnsupported: a forge that 404s introspection warns
// (never blocks), but the admin check still runs off VerifyToken's identity.
func TestCheckTokenIntrospectionUnsupported(t *testing.T) {
	c := NewChecker()

	f := &fakeForge{tokenErr: forge.ErrTokenIntrospectionUnsupported}
	tr := c.CheckToken(context.Background(), f, false, now)
	if len(tr.Violations) != 0 {
		t.Fatalf("unsupported introspection must not block; got violations %v", tr.Violations)
	}
	if !hasFinding(tr.Warnings, "requires 15.5+") {
		t.Fatalf("want a version warning, got %v", tr.Warnings)
	}

	// Admin flag still turns into a violation even when scopes can't be read.
	trAdmin := c.CheckToken(context.Background(), f, true, now)
	if !hasFinding(trAdmin.Violations, "instance admin") {
		t.Fatalf("admin check must apply without introspection; got %v", trAdmin.Violations)
	}
}

// TestCheckFullReport walks the per-repo matrix through the full Check path.
func TestCheckFullReport(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42, IsAdmin: false},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles: map[int64]roleResult{
			1: {role: 30, member: true}, // compliant Developer
			2: {role: 40, member: true}, // Maintainer → violation
			3: {member: false},          // not a member → violation
			4: {role: 30, member: true}, // Developer but branch problems below
		},
		prots: map[int64]protResult{
			1: {bp: forge.BranchProtection{Protected: true, DevelopersCanPush: false}},
			2: {bp: forge.BranchProtection{Protected: true, DevelopersCanPush: false}},
			3: {bp: forge.BranchProtection{Protected: true, DevelopersCanPush: false}},
			4: {bp: forge.BranchProtection{Protected: false}}, // unprotected → violation
		},
	}
	repos := []Repo{
		{ID: "r1", Path: "g/one", ForgeProjectID: 1, DefaultBranch: "main"},
		{ID: "r2", Path: "g/two", ForgeProjectID: 2, DefaultBranch: "main"},
		{ID: "r3", Path: "g/three", ForgeProjectID: 3, DefaultBranch: "main"},
		{ID: "r4", Path: "g/four", ForgeProjectID: 4, DefaultBranch: "main"},
	}
	rep := NewChecker().Check(context.Background(), f, repos, now)

	if rep.Status != StatusViolations {
		t.Fatalf("status = %q, want violations", rep.Status)
	}
	if len(rep.Token.Violations) != 0 {
		t.Fatalf("token should be clean, got %v", rep.Token.Violations)
	}
	byID := map[string]RepoReport{}
	for _, r := range rep.Repos {
		byID[r.RepoID] = r
	}
	if len(byID["r1"].Violations) != 0 {
		t.Errorf("r1 should be clean, got %v", byID["r1"].Violations)
	}
	if !hasFinding(byID["r2"].Violations, "Maintainer (40)") {
		t.Errorf("r2 want Maintainer violation, got %v", byID["r2"].Violations)
	}
	if !hasFinding(byID["r3"].Violations, "no longer a Developer member") {
		t.Errorf("r3 want not-a-member violation, got %v", byID["r3"].Violations)
	}
	if !hasFinding(byID["r4"].Violations, "not protected") {
		t.Errorf("r4 want unprotected-branch violation, got %v", byID["r4"].Violations)
	}
}

func TestCheckDevelopersCanPushIsViolation(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: 30, member: true}},
		prots:     map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true, DevelopersCanPush: true}}},
	}
	rep := NewChecker().Check(context.Background(), f, []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: "main"}}, now)
	if !hasFinding(rep.Repos[0].Violations, "Developers may push") {
		t.Fatalf("want developers-can-push violation, got %v", rep.Repos[0].Violations)
	}
}

// TestCheckBotDirectPushGrantIsViolation: a protected branch that Developers
// cannot push, but which grants the bot a direct per-user push, is still a
// violation (the false negative the role check alone would miss).
func TestCheckBotDirectPushGrantIsViolation(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: 30, member: true}},
		prots:     map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true, DevelopersCanPush: false, BotCanPush: true}}},
	}
	rep := NewChecker().Check(context.Background(), f, []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: "main"}}, now)
	if !hasFinding(rep.Repos[0].Violations, "direct push grant") {
		t.Fatalf("want bot direct-push-grant violation, got %v", rep.Repos[0].Violations)
	}
}

func TestCheckEmptyDefaultBranchWarns(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: 30, member: true}},
	}
	rep := NewChecker().Check(context.Background(), f, []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: ""}}, now)
	if len(rep.Repos[0].Violations) != 0 {
		t.Fatalf("empty-default-branch repo should not violate, got %v", rep.Repos[0].Violations)
	}
	if !hasFinding(rep.Repos[0].Warnings, "no default branch") {
		t.Fatalf("want no-default-branch warning, got %v", rep.Repos[0].Warnings)
	}
	if rep.Status != StatusWarnings {
		t.Fatalf("status = %q, want warnings", rep.Status)
	}
}

// TestCheckDrift: the same connection flips ok→violations when the fake forge
// changes the bot's role between checks — the drift scenario the sweep exists for.
func TestCheckDrift(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: 30, member: true}},
		prots:     map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true}}},
	}
	repos := []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: "main"}}
	c := NewChecker()

	if rep := c.Check(context.Background(), f, repos, now); rep.Status != StatusOK {
		t.Fatalf("initial status = %q, want ok", rep.Status)
	}
	// A teammate promotes the bot to Maintainer.
	f.roles[1] = roleResult{role: 40, member: true}
	if rep := c.Check(context.Background(), f, repos, now); rep.Status != StatusViolations {
		t.Fatalf("post-drift status = %q, want violations", rep.Status)
	}
}

// TestCheckErrorReport: a top-level VerifyToken failure (revoked token / forge
// down) yields StatusError with the finding recorded, never a returned error.
func TestCheckErrorReport(t *testing.T) {
	f := &fakeForge{verifyErr: errors.New("401 unauthorized")}
	rep := NewChecker().Check(context.Background(), f, []Repo{{ID: "r1", ForgeProjectID: 1}}, now)
	if rep.Status != StatusError {
		t.Fatalf("status = %q, want error", rep.Status)
	}
	if !hasFinding(rep.Token.Warnings, "could not verify the bot token") {
		t.Fatalf("want a verify-failure warning, got %v", rep.Token.Warnings)
	}
	// The redacted error must not embed the raw forge message.
	if hasFinding(rep.Token.Warnings, "401 unauthorized") {
		t.Fatalf("report leaked the raw forge error: %v", rep.Token.Warnings)
	}
}
