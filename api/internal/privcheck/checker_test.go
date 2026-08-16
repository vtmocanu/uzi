package privcheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
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
func (f *fakeForge) ProjectCIConfigPath(context.Context, int64) (string, error)    { return "", nil }

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

// PRD #72 M5: no-op stub — this fake's tests never patch a description.
func (f *fakeForge) UpdateIssueDescription(context.Context, int64, int64, string) error {
	return nil
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

// hasCode reports whether the coded findings contain a finding with the given
// Code (PRD #66 D5).
func hasCode(list []Finding, code Code) bool {
	for _, f := range list {
		if f.Code == code {
			return true
		}
	}
	return false
}

// findingByCode returns the first finding with the given Code, or (zero, false).
func findingByCode(list []Finding, code Code) (Finding, bool) {
	for _, f := range list {
		if f.Code == code {
			return f, true
		}
	}
	return Finding{}, false
}

// allDeclaredCodes lists every Code constant declared in report.go. The coverage
// test (TestFindingSeverityCoversEveryCode) asserts this list and findingSeverity
// are in exact agreement, so a Code added without a severity — or a map entry
// without a declared Code — fails the build's tests (D5).
var allDeclaredCodes = []Code{
	CodeDefaultBranchUnprotected,
	CodeWriteRoleCanPush,
	CodeBotCanPush,
	CodeWriteRoleCanMerge,
	CodeBotCanMerge,
	CodeUnprotectedFilePatterns,
	CodeProtectionUnreadable,
	CodeBotNotMember,
	CodeBotRoleBelowWrite,
	CodeBotRoleAboveWrite,
	CodeGroupPushGrantUndetected,
	CodeRoleUnreadable,
	CodeNoDefaultBranch,
}

// TestFindingSeverityCoversEveryCode pins D5: findingSeverity is the single source
// of severity, so every declared Code must map to a valid (block|warn) severity,
// and the map must carry no code beyond the declared set.
func TestFindingSeverityCoversEveryCode(t *testing.T) {
	for _, c := range allDeclaredCodes {
		sev, ok := findingSeverity[c]
		if !ok {
			t.Errorf("code %q missing from findingSeverity (D5: severity is never hand-set)", c)
			continue
		}
		if sev != SeverityBlock && sev != SeverityWarn {
			t.Errorf("code %q has invalid severity %q", c, sev)
		}
	}
	if len(findingSeverity) != len(allDeclaredCodes) {
		t.Errorf("findingSeverity has %d entries, allDeclaredCodes has %d — a map entry has no declared Code (or vice versa)",
			len(findingSeverity), len(allDeclaredCodes))
	}
	// newFinding must stamp the severity from the map, never leave it empty.
	f := newFinding(CodeDefaultBranchUnprotected, "x")
	if f.Severity != SeverityBlock {
		t.Errorf("newFinding severity = %q, want block", f.Severity)
	}
}

// TestBlocksRun pins the canonical blocking predicate (PRD #66 D3/D6). The
// load-bearing rows are the first (unprotected → blocked, the R3 inversion guard)
// and the last (a protected branch with all four push/merge fields false is NOT
// read as blocked — the false,false,false,false case must clear).
func TestBlocksRun(t *testing.T) {
	cases := []struct {
		name string
		bp   forge.BranchProtection
		want bool
	}{
		{"unprotected is the worst case and blocks", forge.BranchProtection{Protected: false}, true},
		{"protected with nothing granted does not block", forge.BranchProtection{Protected: true}, false},
		{"write role may push blocks", forge.BranchProtection{Protected: true, WriteRoleCanPush: true}, true},
		{"bot direct push grant blocks", forge.BranchProtection{Protected: true, BotCanPush: true}, true},
		{"write role may merge blocks", forge.BranchProtection{Protected: true, WriteRoleCanMerge: true}, true},
		{"bot direct merge grant blocks", forge.BranchProtection{Protected: true, BotCanMerge: true}, true},
		// The inversion trap: a protected branch reporting false on every push/merge
		// field is genuinely safe and must clear — proving BlocksRun is not just
		// returning true on a zero value.
		{"protected false,false,false,false is not blocked", forge.BranchProtection{
			Protected: true, WriteRoleCanPush: false, BotCanPush: false, WriteRoleCanMerge: false, BotCanMerge: false,
		}, false},
		// ProtectionUnverified is NOT a bp field BlocksRun OR's in (that is the
		// caller's fail-closed error path): on a protected branch with all grants
		// false, it stays unblocked here.
		{"protection unverified alone does not block", forge.BranchProtection{Protected: true, ProtectionUnverified: true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BlocksRun(tc.bp); got != tc.want {
				t.Fatalf("BlocksRun(%+v) = %v, want %v", tc.bp, got, tc.want)
			}
		})
	}
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
	if len(byID["r1"].Findings) != 0 {
		t.Errorf("r1 should be clean, got %v", byID["r1"].Findings)
	}
	// r2: bot above write — a WARN under D6 (was a violation), so it no longer
	// drives the connection to violations on its own.
	if !hasCode(byID["r2"].Findings, CodeBotRoleAboveWrite) {
		t.Errorf("r2 want bot_role_above_write finding, got %v", byID["r2"].Findings)
	}
	// r3: not a member — a WARN under D6 (was a violation).
	if !hasCode(byID["r3"].Findings, CodeBotNotMember) {
		t.Errorf("r3 want bot_not_member finding, got %v", byID["r3"].Findings)
	}
	// r4: unprotected default branch — the BLOCK finding that drives status to
	// violations (the connection-level assertion above).
	if f, ok := findingByCode(byID["r4"].Findings, CodeDefaultBranchUnprotected); !ok || f.Severity != SeverityBlock {
		t.Errorf("r4 want default_branch_unprotected BLOCK finding, got %v", byID["r4"].Findings)
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
	if f, ok := findingByCode(rep.Repos[0].Findings, CodeWriteRoleCanPush); !ok || f.Severity != SeverityBlock {
		t.Fatalf("want write_role_can_push BLOCK finding, got %v", rep.Repos[0].Findings)
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
	if f, ok := findingByCode(rep.Repos[0].Findings, CodeBotCanPush); !ok || f.Severity != SeverityBlock {
		t.Fatalf("want bot_can_push BLOCK finding, got %v", rep.Repos[0].Findings)
	}
}

func TestCheckEmptyDefaultBranchWarns(t *testing.T) {
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
	}
	rep := NewChecker().Check(context.Background(), f, forge.TypeGitLab, []Repo{{ID: "r1", ForgeProjectID: 1, DefaultBranch: ""}}, now)
	if rep.Repos[0].Blocks() {
		t.Fatalf("empty-default-branch repo should not block, got %v", rep.Repos[0].Findings)
	}
	if f, ok := findingByCode(rep.Repos[0].Findings, CodeNoDefaultBranch); !ok || f.Severity != SeverityWarn {
		t.Fatalf("want no_default_branch WARN finding, got %v", rep.Repos[0].Findings)
	}
	if rep.Status != StatusWarnings {
		t.Fatalf("status = %q, want warnings", rep.Status)
	}
}

// TestCheckDrift: the same connection flips ok→violations when the fake forge
// changes the branch protection between checks — the drift scenario the sweep
// exists for. Drift is on branch protection (a BLOCK finding under D6) rather than
// role: #66 D6 downgrades bot_role_above_write to a warn, so a role promotion no
// longer drives the connection to violations on its own.
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
	// A teammate removes branch protection on the default branch (a BLOCK finding).
	f.prots[1] = protResult{bp: forge.BranchProtection{Protected: false}}
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

// TestCheckVersionDowngradeDistinctFinding: when VerifyToken refuses because the
// forge downgraded below the driver minimum (Forgejo <16, D4/D4a), the sweep
// surfaces a distinct upgrade-the-server finding via errors.Is on
// forge.ErrForgeVersionUnsupported — NOT the generic revoked/unreachable message
// that would send the user to re-mint a working token. Still StatusError.
func TestCheckVersionDowngradeDistinctFinding(t *testing.T) {
	// Wrap the sentinel the way the driver does (%w through a redacted message),
	// so errors.Is reaches it through the wrapper.
	f := &fakeForge{verifyErr: fmt.Errorf("forgejo: server version %q is below the required Forgejo %s: %w", "15.0.4", "16.0.0", forge.ErrForgeVersionUnsupported)}
	rep := NewChecker().Check(context.Background(), f, forge.TypeForgejo, []Repo{{ID: "r1", ForgeProjectID: 1}}, now)
	if rep.Status != StatusError {
		t.Fatalf("status = %q, want error", rep.Status)
	}
	if !hasFinding(rep.Token.Warnings, "older than the minimum version uzi requires") {
		t.Fatalf("want the distinct version-downgrade finding, got %v", rep.Token.Warnings)
	}
	// It must NOT collapse to the generic token-failure message.
	if hasFinding(rep.Token.Warnings, "could not verify the bot token") {
		t.Fatalf("a version downgrade must not read as a token failure: %v", rep.Token.Warnings)
	}
	// And it must not leak the raw driver error (version string / wrapper text).
	if hasFinding(rep.Token.Warnings, "15.0.4") || hasFinding(rep.Token.Warnings, "forgejo:") {
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
		// GitHub classic PAT: exactly {repo} (PRD #238 D7). The "exactly" set
		// semantics make {repo, workflow} and {repo, delete_repo} over-privilege
		// violations for free (D7a); {public_repo} alone is a subset and fails too.
		{"github exactly repo is clean", forge.TypeGitHub, []string{"repo"}, false},
		{"github repo+workflow violates", forge.TypeGitHub, []string{"repo", "workflow"}, true},
		{"github repo+delete_repo violates", forge.TypeGitHub, []string{"repo", "delete_repo"}, true},
		{"github public_repo alone violates", forge.TypeGitHub, []string{"public_repo"}, true},
		{"github gitlab-api violates", forge.TypeGitHub, []string{"api"}, true},
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

// TestRequiredScopesGitHub pins PRD #238 D7: the GitHub required set is exactly
// {repo}, and the unordered set-equality accepts it while rejecting supersets and
// subsets. GitLab {api} and Forgejo's three-scope set are unchanged (covered by
// TestScopesPerForge); this asserts the rule primitives directly.
func TestRequiredScopesGitHub(t *testing.T) {
	req := requiredScopesFor(forge.TypeGitHub)
	if len(req) != 1 || req[0] != "repo" {
		t.Fatalf("requiredScopesFor(github) = %v, want [repo]", req)
	}
	if !scopesEqualRequired(forge.TypeGitHub, []string{"repo"}) {
		t.Errorf("{repo} should equal the github required set")
	}
	if scopesEqualRequired(forge.TypeGitHub, []string{"repo", "workflow"}) {
		t.Errorf("{repo, workflow} is over-privilege, must not equal required")
	}
	if scopesEqualRequired(forge.TypeGitHub, []string{"repo", "delete_repo"}) {
		t.Errorf("{repo, delete_repo} is over-privilege, must not equal required")
	}
	if scopesEqualRequired(forge.TypeGitHub, []string{"public_repo"}) {
		t.Errorf("{public_repo} is a subset, must not equal required")
	}
}

// TestIntrospectionUnsupportedMessagePerForge pins PRD #238 S4: the
// introspection-unsupported warning is per-forge. GitLab keeps its actionable
// "requires 15.5+" hint; GitHub points the user at a classic PAT rather than
// flattening to a lossy generic string.
func TestIntrospectionUnsupportedMessagePerForge(t *testing.T) {
	c := NewChecker()
	f := &fakeForge{tokenErr: forge.ErrTokenIntrospectionUnsupported}

	trGitHub := c.CheckToken(context.Background(), f, forge.TypeGitHub, false, now)
	if len(trGitHub.Violations) != 0 {
		t.Fatalf("unsupported introspection must not block on github; got %v", trGitHub.Violations)
	}
	if !hasFinding(trGitHub.Warnings, "classic PAT") {
		t.Fatalf("want the github-specific classic-PAT warning, got %v", trGitHub.Warnings)
	}
	if hasFinding(trGitHub.Warnings, "15.5+") {
		t.Fatalf("github must not surface GitLab's version hint, got %v", trGitHub.Warnings)
	}

	// GitLab's path is unchanged.
	trGitLab := c.CheckToken(context.Background(), f, forge.TypeGitLab, false, now)
	if !hasFinding(trGitLab.Warnings, "requires 15.5+") {
		t.Fatalf("gitlab must keep its version warning, got %v", trGitLab.Warnings)
	}
}

// TestEvaluateRepoProtectionUnverified pins PRD #238 D6/H1 as reshaped by #66
// D3/D6: a github-shaped protected-but-unverified branch (WriteRoleCanPush/Merge
// =false, the driver never fabricating a true) folds into the protection_unreadable
// BLOCK code — "could not tell" is fail-closed the same as a read error — not a
// push/merge finding. When ProtectionUnverified is false, behaviour is the
// existing one: the push BLOCK finding fires and no protection_unreadable is added.
func TestEvaluateRepoProtectionUnverified(t *testing.T) {
	repo := Repo{ID: "r1", Path: "g/one", ForgeProjectID: 1, DefaultBranch: "main"}

	// Undetermined: protected, but the driver could not read push/merge rights.
	var unver RepoReport
	unver.Findings = []Finding{}
	evaluateRepo(&unver, repo, forge.RoleWrite, true, nil, true,
		forge.BranchProtection{Protected: true, ProtectionUnverified: true, WriteRoleCanPush: false, WriteRoleCanMerge: false}, nil)
	if f, ok := findingByCode(unver.Findings, CodeProtectionUnreadable); !ok || f.Severity != SeverityBlock {
		t.Fatalf("want protection_unreadable BLOCK finding, got %v", unver.Findings)
	}
	if hasCode(unver.Findings, CodeWriteRoleCanPush) || hasCode(unver.Findings, CodeWriteRoleCanMerge) {
		t.Fatalf("unverified protection must not emit push/merge findings, got %v", unver.Findings)
	}

	// Verified (ProtectionUnverified=false) with a real push right: unchanged, the
	// push BLOCK finding fires and no protection_unreadable is added.
	var ver RepoReport
	ver.Findings = []Finding{}
	evaluateRepo(&ver, repo, forge.RoleWrite, true, nil, true,
		forge.BranchProtection{Protected: true, ProtectionUnverified: false, WriteRoleCanPush: true}, nil)
	if !hasCode(ver.Findings, CodeWriteRoleCanPush) {
		t.Fatalf("verified protection with push right must still emit write_role_can_push, got %v", ver.Findings)
	}
	if hasCode(ver.Findings, CodeProtectionUnreadable) {
		t.Fatalf("verified protection must not add protection_unreadable, got %v", ver.Findings)
	}
}

// TestMergeFindingsAreViolations pins D6a-1's tier: a bot that can merge its own
// PR into protected main breaks the primary directive exactly as one that can
// push does, so the merge findings are Violations, the same tier as the push
// sibling. This is non-blocking in #65 in the sense that matters — a per-repo
// Violation never blocks a save (only token violations do) and nothing gates a
// run on privilege_status — so the badge going ok->violations for the
// merge-lowered GitLab population is the intended, user-confirmed behaviour, not
// a refusal. #66 promotes these fields to run-refusals, not the badge tier.
func TestMergeFindingsAreViolations(t *testing.T) {
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
	if wf, ok := findingByCode(rr.Findings, CodeWriteRoleCanMerge); !ok || wf.Severity != SeverityBlock {
		t.Fatalf("want a write_role_can_merge BLOCK finding, got %v", rr.Findings)
	}
	if bf, ok := findingByCode(rr.Findings, CodeBotCanMerge); !ok || bf.Severity != SeverityBlock {
		t.Fatalf("want a bot_can_merge BLOCK finding, got %v", rr.Findings)
	}
	// A merge finding drives the connection status to violations (the badge), but
	// this blocks nothing in M1: no save is rejected and no run is refused on it.
	if rep.Status != StatusViolations {
		t.Fatalf("a merge finding must drive status to violations, got %q", rep.Status)
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
	// Push and merge findings are both Violations now, so a leaked per-field
	// finding would show up as an EXTRA violation — the exact-count assertion
	// below is what catches it, on whichever array it would land.
	var unprot RepoReport
	unprot.Findings = []Finding{}
	evaluateRepo(&unprot, repo, forge.RoleWrite, true, nil, true,
		forge.BranchProtection{Protected: false, WriteRoleCanPush: true, BotCanPush: true, WriteRoleCanMerge: true, BotCanMerge: true}, nil)
	if f, ok := findingByCode(unprot.Findings, CodeDefaultBranchUnprotected); !ok || f.Severity != SeverityBlock {
		t.Fatalf("unprotected must yield the default_branch_unprotected BLOCK finding, got %v", unprot.Findings)
	}
	if len(unprot.Findings) != 1 {
		t.Fatalf("unprotected must short-circuit to the single strongest finding, got %v", unprot.Findings)
	}
	// The push/merge codes must NOT leak on the unprotected path (the inversion trap).
	for _, c := range []Code{CodeWriteRoleCanPush, CodeBotCanPush, CodeWriteRoleCanMerge, CodeBotCanMerge} {
		if hasCode(unprot.Findings, c) {
			t.Fatalf("unprotected must not emit %q, got %v", c, unprot.Findings)
		}
	}

	// A fully clean protected branch produces nothing — the negative control that
	// proves the short-circuit is not just swallowing everything.
	var clean RepoReport
	clean.Findings = []Finding{}
	evaluateRepo(&clean, repo, forge.RoleWrite, true, nil, true,
		forge.BranchProtection{Protected: true}, nil)
	if len(clean.Findings) != 0 {
		t.Fatalf("a clean protected branch must be finding-free, got %v", clean.Findings)
	}
}

// TestEvaluateRepoProtectionReadErrorBlocks pins D3/D6: a branch-protection read
// error escalates to the protection_unreadable BLOCK code (was a warning before
// #66). Fail-closed: a hostile forge must not pass the guardrail by erroring.
func TestEvaluateRepoProtectionReadErrorBlocks(t *testing.T) {
	repo := Repo{ID: "r1", Path: "g/one", ForgeProjectID: 1, DefaultBranch: "main"}
	var rr RepoReport
	rr.Findings = []Finding{}
	evaluateRepo(&rr, repo, forge.RoleWrite, true, nil, true,
		forge.BranchProtection{}, errors.New("503 service unavailable"))
	if f, ok := findingByCode(rr.Findings, CodeProtectionUnreadable); !ok || f.Severity != SeverityBlock {
		t.Fatalf("a protection read error must yield protection_unreadable BLOCK, got %v", rr.Findings)
	}
	if !rr.Blocks() {
		t.Fatalf("a protection read error must make the repo block, got %v", rr.Findings)
	}
	// The raw driver error must not leak into the finding message.
	if f, _ := findingByCode(rr.Findings, CodeProtectionUnreadable); strings.Contains(f.Message, "503") {
		t.Fatalf("protection_unreadable message leaked the raw error: %q", f.Message)
	}
}

// TestEvaluateRepoPushMergeFlagsBlock pins D6: on a protected branch, each of the
// four push/merge flags yields its own BLOCK code with the correct severity.
func TestEvaluateRepoPushMergeFlagsBlock(t *testing.T) {
	repo := Repo{ID: "r1", Path: "g/one", ForgeProjectID: 1, DefaultBranch: "main"}
	cases := []struct {
		name string
		bp   forge.BranchProtection
		code Code
	}{
		{"write role can push", forge.BranchProtection{Protected: true, WriteRoleCanPush: true}, CodeWriteRoleCanPush},
		{"bot can push", forge.BranchProtection{Protected: true, BotCanPush: true}, CodeBotCanPush},
		{"write role can merge", forge.BranchProtection{Protected: true, WriteRoleCanMerge: true}, CodeWriteRoleCanMerge},
		{"bot can merge", forge.BranchProtection{Protected: true, BotCanMerge: true}, CodeBotCanMerge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var rr RepoReport
			rr.Findings = []Finding{}
			evaluateRepo(&rr, repo, forge.RoleWrite, true, nil, true, tc.bp, nil)
			f, ok := findingByCode(rr.Findings, tc.code)
			if !ok {
				t.Fatalf("want %q finding, got %v", tc.code, rr.Findings)
			}
			if f.Severity != SeverityBlock {
				t.Fatalf("%q severity = %q, want block", tc.code, f.Severity)
			}
			if !rr.Blocks() {
				t.Fatalf("%q must make the repo block", tc.code)
			}
		})
	}
}
