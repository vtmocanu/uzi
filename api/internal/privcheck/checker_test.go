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
	role   forge.Role
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
func (f *fakeForge) ProjectRole(_ context.Context, projectID, _ int64) (forge.Role, bool, error) {
	r := f.roles[projectID]
	return r.role, r.member, r.err
}
func (f *fakeForge) DefaultBranchProtection(_ context.Context, projectID int64, _ string, _ int64) (forge.BranchProtection, error) {
	p := f.prots[projectID]
	return p.bp, p.err
}

// Pipeline reads (PRD #6) are unused by this fake — stubbed to satisfy forge.Forge.
func (f *fakeForge) LatestPipeline(context.Context, int64, string) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}
func (f *fakeForge) LatestMRPipeline(context.Context, int64, int64) (forge.Pipeline, error) {
	return forge.Pipeline{}, forge.ErrNoPipeline
}
func (f *fakeForge) ListPipelineJobs(context.Context, int64, int64) ([]forge.Job, error) {
	return nil, nil
}
func (f *fakeForge) JobLogTail(context.Context, int64, int64, int) (string, error) { return "", nil }

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
func (f *fakeForge) UserExists(context.Context, string) (bool, error) { return false, nil }
func (f *fakeForge) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return nil, nil
}
func (f *fakeForge) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	return forge.IssueNote{}, nil
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
			tr := evaluateToken(forge.TypeGitLab, tc.info, tc.isAdmin, now, warnWindow, "")
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
	tr := c.CheckToken(context.Background(), f, forge.TypeGitLab, false, now)
	if len(tr.Violations) != 0 {
		t.Fatalf("unsupported introspection must not block; got violations %v", tr.Violations)
	}
	if !hasFinding(tr.Warnings, "requires 15.5+") {
		t.Fatalf("want a version warning, got %v", tr.Warnings)
	}

	// Admin flag still turns into a violation even when scopes can't be read.
	trAdmin := c.CheckToken(context.Background(), f, forge.TypeGitLab, true, now)
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
			1: {role: forge.RoleWrite, member: true}, // compliant write role
			2: {role: forge.RoleAdmin, member: true}, // admin → violation
			3: {member: false},                       // not a member → violation
			4: {role: forge.RoleWrite, member: true}, // write role but branch problems below
		},
		prots: map[int64]protResult{
			1: {bp: forge.BranchProtection{Protected: true, WriteRoleCanPush: false}},
			2: {bp: forge.BranchProtection{Protected: true, WriteRoleCanPush: false}},
			3: {bp: forge.BranchProtection{Protected: true, WriteRoleCanPush: false}},
			4: {bp: forge.BranchProtection{Protected: false}}, // unprotected → violation
		},
	}
	repos := []Repo{
		{ID: "r1", Path: "g/one", ForgeProjectID: 1, DefaultBranch: "main"},
		{ID: "r2", Path: "g/two", ForgeProjectID: 2, DefaultBranch: "main"},
		{ID: "r3", Path: "g/three", ForgeProjectID: 3, DefaultBranch: "main"},
		{ID: "r4", Path: "g/four", ForgeProjectID: 4, DefaultBranch: "main"},
	}
	rep := NewChecker().Check(context.Background(), f, forge.TypeGitLab, repos, now)

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
	if !hasFinding(byID["r2"].Violations, "role is admin, above") {
		t.Errorf("r2 want above-write violation, got %v", byID["r2"].Violations)
	}
	if !hasFinding(byID["r3"].Violations, "no longer a member") {
		t.Errorf("r3 want not-a-member violation, got %v", byID["r3"].Violations)
	}
	if !hasFinding(byID["r4"].Violations, "not protected") {
		t.Errorf("r4 want unprotected-branch violation, got %v", byID["r4"].Violations)
	}
}

func TestCheckWriteRoleCanPushIsViolation(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:     map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true, WriteRoleCanPush: true}}},
	}
	rep := NewChecker().Check(context.Background(), f, forge.TypeGitLab, []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: "main"}}, now)
	if !hasFinding(rep.Repos[0].Violations, "the write role may push") {
		t.Fatalf("want write-role-can-push violation, got %v", rep.Repos[0].Violations)
	}
}

// TestCheckBotDirectPushGrantIsViolation: a protected branch the write role
// cannot push, but which grants the bot a direct per-user push, is still a
// violation (the false negative the role check alone would miss).
func TestCheckBotDirectPushGrantIsViolation(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:     map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true, WriteRoleCanPush: false, BotCanPush: true}}},
	}
	rep := NewChecker().Check(context.Background(), f, forge.TypeGitLab, []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: "main"}}, now)
	if !hasFinding(rep.Repos[0].Violations, "direct push grant") {
		t.Fatalf("want bot direct-push-grant violation, got %v", rep.Repos[0].Violations)
	}
}

func TestCheckEmptyDefaultBranchWarns(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
	}
	rep := NewChecker().Check(context.Background(), f, forge.TypeGitLab, []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: ""}}, now)
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
		roles:     map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:     map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true}}},
	}
	repos := []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: "main"}}
	c := NewChecker()

	if rep := c.Check(context.Background(), f, forge.TypeGitLab, repos, now); rep.Status != StatusOK {
		t.Fatalf("initial status = %q, want ok", rep.Status)
	}
	// A teammate promotes the bot to Maintainer.
	f.roles[1] = roleResult{role: forge.RoleAdmin, member: true}
	if rep := c.Check(context.Background(), f, forge.TypeGitLab, repos, now); rep.Status != StatusViolations {
		t.Fatalf("post-drift status = %q, want violations", rep.Status)
	}
}

// TestCheckErrorReport: a top-level VerifyToken failure (revoked token / forge
// down) yields StatusError with the finding recorded, never a returned error.
func TestCheckErrorReport(t *testing.T) {
	f := &fakeForge{verifyErr: errors.New("401 unauthorized")}
	rep := NewChecker().Check(context.Background(), f, forge.TypeGitLab, []Repo{{ID: "r1", ForgeProjectID: 1}}, now)
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

// TestScopesPerForge pins D6b: the required scope set is per-forge and compared
// as an unordered set, keeping the "exactly" semantics. GitLab is unchanged
// (exactly {api}); Forgejo is exactly {write:repository, write:issue, read:user}.
// The two traps the PRD calls out are the load-bearing cases here: order must not
// matter, and the god-mode ["all"] literal must be rejected as a plain non-match,
// never expanded.
func TestScopesPerForge(t *testing.T) {
	warnWindow := 14 * 24 * time.Hour
	forgejoOK := []string{"write:repository", "write:issue", "read:user"}
	cases := []struct {
		name          string
		forgeType     forge.Type
		scopes        []string
		wantViolation bool
	}{
		{"gitlab exactly api is clean", forge.TypeGitLab, []string{"api"}, false},
		{"gitlab extra scope violates", forge.TypeGitLab, []string{"api", "sudo"}, true},
		{"gitlab forgejo-scopes violate", forge.TypeGitLab, forgejoOK, true},
		{"forgejo exact set is clean", forge.TypeForgejo, forgejoOK, false},
		// Forgejo re-emits scopes in its own canonical order, not mint order, so a
		// string compare would be a coin flip. Every permutation must pass.
		{"forgejo reordered is clean", forge.TypeForgejo, []string{"read:user", "write:repository", "write:issue"}, false},
		{"forgejo superset violates", forge.TypeForgejo, append(append([]string{}, forgejoOK...), "write:organization"), true},
		{"forgejo missing one violates", forge.TypeForgejo, []string{"write:repository", "write:issue"}, true},
		// The god-mode token arrives as the single literal "all" (Forgejo collapses
		// every write:* into it); it is rejected because it is not the three-scope
		// set, with no attempt to expand it.
		{"forgejo all literal violates", forge.TypeForgejo, []string{"all"}, true},
		{"forgejo gitlab-api violates", forge.TypeForgejo, []string{"api"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := evaluateToken(tc.forgeType, forge.TokenInfo{Scopes: tc.scopes, Active: true}, false, now, warnWindow, "")
			got := hasFinding(tr.Violations, "are not exactly")
			if got != tc.wantViolation {
				t.Errorf("scopes %v on %s: scope violation = %v, want %v (violations=%v)",
					tc.scopes, tc.forgeType, got, tc.wantViolation, tr.Violations)
			}
		})
	}
}

// TestMergeFindingsAreWarningsNotViolations is the dark-landing proof for D6a-1.
// A protected branch the bot may push nothing to but may still merge into is the
// case D6a-1 exists to surface. In PRD #65 it must surface as a WARNING, not a
// violation: a violation would flip a real GitLab connection from ok to
// violations, the exact behaviour change that splitting enforcement into PRD #66
// exists to keep out of #65. #66 is what turns these into blocking.
func TestMergeFindingsAreWarningsNotViolations(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots: map[int64]protResult{1: {bp: forge.BranchProtection{
			Protected:         true,
			WriteRoleCanPush:  false,
			BotCanPush:        false,
			WriteRoleCanMerge: true,
			BotCanMerge:       true,
		}}},
	}
	rep := NewChecker().Check(context.Background(), f, forge.TypeGitLab, []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: "main"}}, now)
	rr := rep.Repos[0]
	if len(rr.Violations) != 0 {
		t.Fatalf("merge findings must not be violations in #65, got %v", rr.Violations)
	}
	if !hasFinding(rr.Warnings, "the write role may merge into protected") {
		t.Fatalf("want a write-role merge warning, got %v", rr.Warnings)
	}
	if !hasFinding(rr.Warnings, "the bot has a direct merge grant on protected") {
		t.Fatalf("want a bot merge-grant warning, got %v", rr.Warnings)
	}
	// The connection-level status is warnings, not violations: nothing blocks.
	if rep.Status != StatusWarnings {
		t.Fatalf("a merge-only finding must leave status at warnings, got %q", rep.Status)
	}
}

// TestEvaluateRepoProtectedFirst pins R12 directly on the pure evaluator. An
// unprotected branch — the case where the fields say the bot CAN push and merge
// — must yield exactly the "not protected" finding and short-circuit: it must NOT
// additionally emit a per-field push/merge finding, and it must NOT be readable
// as clean. Checking Protected first is what makes that true regardless of what
// the push/merge fields hold.
func TestEvaluateRepoProtectedFirst(t *testing.T) {
	repo := Repo{ID: "r1", Path: "g/one", ForgeProjectID: 1, DefaultBranch: "main"}

	// Unprotected, with the fields truthfully reporting the bot can push and merge
	// (as both drivers now do). The strongest finding wins and nothing else fires.
	var unprot RepoReport
	unprot.Violations, unprot.Warnings = []string{}, []string{}
	evaluateRepo(&unprot, repo, forge.RoleWrite, true, nil, true,
		forge.BranchProtection{Protected: false, WriteRoleCanPush: true, WriteRoleCanMerge: true}, nil)
	if !hasFinding(unprot.Violations, "is not protected") {
		t.Fatalf("unprotected must yield the not-protected violation, got %v", unprot.Violations)
	}
	if len(unprot.Violations) != 1 {
		t.Fatalf("unprotected must short-circuit to the single strongest finding, got %v", unprot.Violations)
	}
	if len(unprot.Warnings) != 0 {
		t.Fatalf("unprotected must not also emit per-field merge findings, got %v", unprot.Warnings)
	}

	// A fully clean protected branch produces nothing — the negative control that
	// proves the short-circuit is not just swallowing everything.
	var clean RepoReport
	clean.Violations, clean.Warnings = []string{}, []string{}
	evaluateRepo(&clean, repo, forge.RoleWrite, true, nil, true,
		forge.BranchProtection{Protected: true}, nil)
	if len(clean.Violations) != 0 || len(clean.Warnings) != 0 {
		t.Fatalf("a clean protected branch must be finding-free, got v=%v w=%v", clean.Violations, clean.Warnings)
	}
}
