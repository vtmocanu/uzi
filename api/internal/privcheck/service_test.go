package privcheck

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// persisted captures one UpdatePrivilegeReport write.
type persisted struct {
	status string
	report Report
}

// fakeStore stands in for *store.Queries: it serves a fixed connection + repo set
// and records every privilege-report write.
type fakeStore struct {
	conns       []store.ForgeConnection
	reposByConn map[uuid.UUID][]store.Repo
	writes      map[uuid.UUID]persisted
	updateRows  int64 // rows UpdatePrivilegeReport reports affected (default 1)
	listErr     error
}

func newFakeStore() *fakeStore {
	return &fakeStore{reposByConn: map[uuid.UUID][]store.Repo{}, writes: map[uuid.UUID]persisted{}, updateRows: 1}
}

func (s *fakeStore) ListAllForgeConnections(context.Context) ([]store.ForgeConnection, error) {
	return s.conns, s.listErr
}
func (s *fakeStore) ListEnabledReposByConnection(_ context.Context, connID uuid.UUID) ([]store.Repo, error) {
	return s.reposByConn[connID], nil
}
func (s *fakeStore) UpdatePrivilegeReport(_ context.Context, arg store.UpdatePrivilegeReportParams) (int64, error) {
	var rep Report
	_ = json.Unmarshal(arg.PrivilegeReport, &rep)
	s.writes[arg.ID] = persisted{status: arg.PrivilegeStatus.String, report: rep}
	return s.updateRows, nil
}

// fakeBuilder returns a preconfigured forge (or a build error).
type fakeBuilder struct {
	forge    forge.Forge
	buildErr error
}

func (b *fakeBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return b.forge, b.buildErr
}

func aConn() store.ForgeConnection {
	return store.ForgeConnection{ID: uuid.New(), ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com"}
}

func aRepo(projectID int64, branch string) store.Repo {
	return store.Repo{
		ID:                uuid.New(),
		ForgeProjectID:    projectID,
		PathWithNamespace: "g/p",
		DefaultBranch:     pgtype.Text{String: branch, Valid: branch != ""},
		Enabled:           true,
	}
}

// TestCheckConnectionPersists: a clean connection yields StatusOK, written to the
// store.
func TestCheckConnectionPersists(t *testing.T) {
	conn := aConn()
	st := newFakeStore()
	st.reposByConn[conn.ID] = []store.Repo{aRepo(1, "main")}
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:     map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true}}},
	}
	svc := NewService(st, &fakeBuilder{forge: f})

	rep, err := svc.CheckConnection(context.Background(), conn)
	if err != nil {
		t.Fatalf("CheckConnection: %v", err)
	}
	if rep.Status != StatusOK {
		t.Fatalf("report status = %q, want ok", rep.Status)
	}
	if got := st.writes[conn.ID].status; got != string(StatusOK) {
		t.Fatalf("persisted status = %q, want ok", got)
	}
}

// TestSweepGrandfatheredAndDrift: the boot sweep checks a never-checked
// (grandfathered) connection — turning a NULL status into a real one — and a
// re-run flips ok→violations when the default branch's protection is removed (a
// BLOCK finding under #66 D6; a role promotion is only a warn now).
func TestSweepGrandfatheredAndDrift(t *testing.T) {
	conn := aConn() // no privilege_status yet: the grandfathered case
	st := newFakeStore()
	st.conns = []store.ForgeConnection{conn}
	st.reposByConn[conn.ID] = []store.Repo{aRepo(1, "main")}
	f := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42},
		tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true},
		roles:     map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:     map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true}}},
	}
	svc := NewService(st, &fakeBuilder{forge: f})

	// Boot pass back-fills the grandfathered connection.
	res, err := svc.CheckAllConnections(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if res.Checked != 1 || res.OK != 1 {
		t.Fatalf("sweep result = %+v, want 1 checked / 1 ok", res)
	}
	if st.writes[conn.ID].status != string(StatusOK) {
		t.Fatalf("grandfathered connection not back-filled to ok, got %q", st.writes[conn.ID].status)
	}

	// Drift: a teammate removes branch protection on main; the next sweep flips it.
	f.prots[1] = protResult{bp: forge.BranchProtection{Protected: false}}
	res, err = svc.CheckAllConnections(context.Background())
	if err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if res.Violations != 1 {
		t.Fatalf("post-drift sweep = %+v, want 1 violation", res)
	}
	if st.writes[conn.ID].status != string(StatusViolations) {
		t.Fatalf("post-drift persisted status = %q, want violations", st.writes[conn.ID].status)
	}
}

// TestCheckConnectionForgeBuildFailurePersistsError: an undecryptable token (key
// rotated) is surfaced as a persisted StatusError report, not a crash.
func TestCheckConnectionForgeBuildFailurePersistsError(t *testing.T) {
	conn := aConn()
	st := newFakeStore()
	svc := NewService(st, &fakeBuilder{buildErr: errors.New("open: cipher: message authentication failed")})

	rep, err := svc.CheckConnection(context.Background(), conn)
	if err != nil {
		t.Fatalf("build failure must not be a returned error: %v", err)
	}
	if rep.Status != StatusError {
		t.Fatalf("status = %q, want error", rep.Status)
	}
	if st.writes[conn.ID].status != string(StatusError) {
		t.Fatalf("persisted status = %q, want error", st.writes[conn.ID].status)
	}
}

// TestGuardrailImpact: a live scan over one connection with three repos — a
// blocked one (bot may push to protected main), a clean one, and an unevaluable
// one (protection read errors) — counts each correctly and PERSISTS NOTHING.
func TestGuardrailImpact(t *testing.T) {
	conn := aConn()
	st := newFakeStore()
	st.conns = []store.ForgeConnection{conn}
	blocked := aRepo(1, "main") // WriteRoleCanPush → blocked
	clean := aRepo(2, "main")   // protected, nothing granted → not blocked
	broken := aRepo(3, "main")  // protection read errors → unevaluable
	st.reposByConn[conn.ID] = []store.Repo{blocked, clean, broken}
	f := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles: map[int64]roleResult{
			1: {role: forge.RoleWrite, member: true},
			2: {role: forge.RoleWrite, member: true},
			3: {role: forge.RoleWrite, member: true},
		},
		prots: map[int64]protResult{
			1: {bp: forge.BranchProtection{Protected: true, WriteRoleCanPush: true}},
			2: {bp: forge.BranchProtection{Protected: true}},
			3: {err: errors.New("403 from protected-branches")},
		},
	}
	svc := NewService(st, &fakeBuilder{forge: f})

	rep, err := svc.GuardrailImpact(context.Background())
	if err != nil {
		t.Fatalf("GuardrailImpact: %v", err)
	}
	if rep.EnabledRepoCount != 3 || rep.BlockedCount != 1 || rep.UnevaluableCount != 1 {
		t.Fatalf("counts = enabled %d / blocked %d / unevaluable %d, want 3/1/1",
			rep.EnabledRepoCount, rep.BlockedCount, rep.UnevaluableCount)
	}
	byID := map[string]ImpactRepo{}
	for _, r := range rep.Repos {
		byID[r.RepoID] = r
		if r.UserID != conn.UserID.String() || r.ConnectionID != conn.ID.String() {
			t.Fatalf("repo %s carries user %q / conn %q, want %q / %q", r.RepoID, r.UserID, r.ConnectionID, conn.UserID, conn.ID)
		}
	}
	if r := byID[blocked.ID.String()]; !r.Blocked || r.Unevaluable {
		t.Fatalf("blocked repo = %+v, want Blocked && !Unevaluable", r)
	}
	if r := byID[clean.ID.String()]; r.Blocked || r.Unevaluable {
		t.Fatalf("clean repo = %+v, want !Blocked && !Unevaluable", r)
	}
	if r := byID[broken.ID.String()]; r.Blocked || !r.Unevaluable {
		t.Fatalf("broken repo = %+v, want Unevaluable && !Blocked", r)
	}
	// The whole point of M3: it measures without touching the stored report.
	if len(st.writes) != 0 {
		t.Fatalf("GuardrailImpact persisted %d reports, want 0", len(st.writes))
	}
}

// TestGuardrailImpactVerifyTokenFailureIsUnevaluable: a connection whose bot token
// cannot be verified marks all its repos unevaluable (not blocked, not safe) and
// does not crash the scan.
func TestGuardrailImpactVerifyTokenFailureIsUnevaluable(t *testing.T) {
	conn := aConn()
	st := newFakeStore()
	st.conns = []store.ForgeConnection{conn}
	st.reposByConn[conn.ID] = []store.Repo{aRepo(1, "main"), aRepo(2, "main")}
	f := &fakeForge{verifyErr: errors.New("401 token revoked")}
	svc := NewService(st, &fakeBuilder{forge: f})

	rep, err := svc.GuardrailImpact(context.Background())
	if err != nil {
		t.Fatalf("a VerifyToken failure must not be a returned error: %v", err)
	}
	if rep.EnabledRepoCount != 2 || rep.UnevaluableCount != 2 || rep.BlockedCount != 0 {
		t.Fatalf("counts = enabled %d / blocked %d / unevaluable %d, want 2/0/2",
			rep.EnabledRepoCount, rep.BlockedCount, rep.UnevaluableCount)
	}
	if len(st.writes) != 0 {
		t.Fatalf("persisted %d reports, want 0", len(st.writes))
	}
}

// TestGuardrailImpactNoDefaultBranchIsUnevaluable: a repo with no default branch
// is unevaluable (there is nothing to read protection on), not silently safe.
func TestGuardrailImpactProtectionUnverifiedIsUnevaluable(t *testing.T) {
	// A protected branch the driver could not authoritatively read (GitHub
	// legacy-branch case) comes back with a nil error and all Can* fields false —
	// which is "unknown", not "safe". It must count unevaluable, never as
	// not-affected (R1; matches what M2's live gate refuses as protection_unreadable).
	conn := aConn()
	st := newFakeStore()
	st.conns = []store.ForgeConnection{conn}
	st.reposByConn[conn.ID] = []store.Repo{aRepo(1, "main")}
	f := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots: map[int64]protResult{
			1: {bp: forge.BranchProtection{Protected: true, ProtectionUnverified: true}},
		},
	}
	svc := NewService(st, &fakeBuilder{forge: f})

	rep, err := svc.GuardrailImpact(context.Background())
	if err != nil {
		t.Fatalf("GuardrailImpact: %v", err)
	}
	if rep.EnabledRepoCount != 1 || rep.UnevaluableCount != 1 || rep.BlockedCount != 0 {
		t.Fatalf("counts = enabled %d / blocked %d / unevaluable %d, want 1/0/1",
			rep.EnabledRepoCount, rep.BlockedCount, rep.UnevaluableCount)
	}
	if len(st.writes) != 0 {
		t.Fatalf("persisted %d reports, want 0", len(st.writes))
	}
}
func TestGuardrailImpactNoDefaultBranchIsUnevaluable(t *testing.T) {
	conn := aConn()
	st := newFakeStore()
	st.conns = []store.ForgeConnection{conn}
	st.reposByConn[conn.ID] = []store.Repo{aRepo(1, "")}
	f := &fakeForge{identity: forge.BotIdentity{ForgeUserID: 42}}
	svc := NewService(st, &fakeBuilder{forge: f})

	rep, err := svc.GuardrailImpact(context.Background())
	if err != nil {
		t.Fatalf("GuardrailImpact: %v", err)
	}
	if rep.EnabledRepoCount != 1 || rep.UnevaluableCount != 1 || rep.BlockedCount != 0 {
		t.Fatalf("counts = enabled %d / blocked %d / unevaluable %d, want 1/0/1",
			rep.EnabledRepoCount, rep.BlockedCount, rep.UnevaluableCount)
	}
}

// guardRepo helper: a Repo view for the guard, defaulting to project 1 / "main".
func aGuardRepo(branch string) Repo {
	return Repo{ID: uuid.New().String(), Path: "g/p", ForgeProjectID: 1, DefaultBranch: branch}
}

// hasFinding reports whether findings contains a finding with the given code, and
// returns it.
func findFinding(findings []Finding, code Code) (Finding, bool) {
	for _, f := range findings {
		if f.Code == code {
			return f, true
		}
	}
	return Finding{}, false
}

// TestGuardRepoClientBuildErrorBlocks: a client that won't build fails closed with
// protection_unreadable, never waived.
func TestGuardRepoClientBuildErrorBlocks(t *testing.T) {
	svc := NewService(newFakeStore(), &fakeBuilder{buildErr: errors.New("cipher: message authentication failed")})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main")})
	if !res.Blocked {
		t.Fatalf("client-build error must block, got %+v", res)
	}
	if _, ok := findFinding(res.Findings, CodeProtectionUnreadable); !ok {
		t.Fatalf("want protection_unreadable, got %+v", res.Findings)
	}
}

// TestGuardRepoVerifyTokenErrorBlocks: a token that cannot be verified fails closed.
func TestGuardRepoVerifyTokenErrorBlocks(t *testing.T) {
	f := &fakeForge{verifyErr: errors.New("401 token revoked")}
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main")})
	if !res.Blocked {
		t.Fatalf("VerifyToken error must block, got %+v", res)
	}
	if _, ok := findFinding(res.Findings, CodeProtectionUnreadable); !ok {
		t.Fatalf("want protection_unreadable, got %+v", res.Findings)
	}
}

// TestGuardRepoUnprotectedBranchBlocksProtectedFirst: an unprotected branch blocks
// with exactly default_branch_unprotected — the push/merge codes are NOT present,
// asserting Protected-first (R3): false,false is never read as safe.
func TestGuardRepoUnprotectedBranchBlocksProtectedFirst(t *testing.T) {
	f := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:    map[int64]protResult{1: {bp: forge.BranchProtection{Protected: false}}},
	}
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main")})
	if !res.Blocked {
		t.Fatalf("unprotected branch must block, got %+v", res)
	}
	if _, ok := findFinding(res.Findings, CodeDefaultBranchUnprotected); !ok {
		t.Fatalf("want default_branch_unprotected, got %+v", res.Findings)
	}
	for _, c := range []Code{CodeWriteRoleCanPush, CodeBotCanPush, CodeWriteRoleCanMerge, CodeBotCanMerge} {
		if _, ok := findFinding(res.Findings, c); ok {
			t.Fatalf("push/merge code %q must NOT be present on an unprotected branch (R3), got %+v", c, res.Findings)
		}
	}
}

// TestGuardRepoProtectedGrantsBlock: each of the four protected-branch push/merge
// grants blocks with its own code.
func TestGuardRepoProtectedGrantsBlock(t *testing.T) {
	cases := []struct {
		name string
		bp   forge.BranchProtection
		code Code
	}{
		{"write_role_can_push", forge.BranchProtection{Protected: true, WriteRoleCanPush: true}, CodeWriteRoleCanPush},
		{"bot_can_push", forge.BranchProtection{Protected: true, BotCanPush: true}, CodeBotCanPush},
		{"write_role_can_merge", forge.BranchProtection{Protected: true, WriteRoleCanMerge: true}, CodeWriteRoleCanMerge},
		{"bot_can_merge", forge.BranchProtection{Protected: true, BotCanMerge: true}, CodeBotCanMerge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeForge{
				identity: forge.BotIdentity{ForgeUserID: 42},
				roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
				prots:    map[int64]protResult{1: {bp: tc.bp}},
			}
			svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
			res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main")})
			if !res.Blocked {
				t.Fatalf("%s must block, got %+v", tc.name, res)
			}
			if _, ok := findFinding(res.Findings, tc.code); !ok {
				t.Fatalf("want %q, got %+v", tc.code, res.Findings)
			}
		})
	}
}

// TestGuardRepoProtectedCleanNotBlocked: a protected branch with nothing granted
// does not block.
func TestGuardRepoProtectedCleanNotBlocked(t *testing.T) {
	f := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:    map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true}}},
	}
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main")})
	if res.Blocked {
		t.Fatalf("clean protected repo must not block, got %+v", res)
	}
	for _, fnd := range res.Findings {
		if fnd.Severity == SeverityBlock {
			t.Fatalf("clean repo carries a block finding: %+v", fnd)
		}
	}
}

// TestGuardRepoBranchProtectionErrorBlocks: a DefaultBranchProtection read error
// becomes protection_unreadable BLOCK inside evaluateRepo.
func TestGuardRepoBranchProtectionErrorBlocks(t *testing.T) {
	f := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:    map[int64]protResult{1: {err: errors.New("403 from protected-branches")}},
	}
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main")})
	if !res.Blocked {
		t.Fatalf("branch-protection read error must block, got %+v", res)
	}
	if _, ok := findFinding(res.Findings, CodeProtectionUnreadable); !ok {
		t.Fatalf("want protection_unreadable, got %+v", res.Findings)
	}
}

// TestGuardRepoProtectionUnverifiedBlocks: protected + nil error but unverified
// (GitHub legacy case) is fail-closed protection_unreadable.
func TestGuardRepoProtectionUnverifiedBlocks(t *testing.T) {
	f := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:    map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true, ProtectionUnverified: true}}},
	}
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main")})
	if !res.Blocked {
		t.Fatalf("ProtectionUnverified must block, got %+v", res)
	}
	if _, ok := findFinding(res.Findings, CodeProtectionUnreadable); !ok {
		t.Fatalf("want protection_unreadable, got %+v", res.Findings)
	}
}

// TestGuardRepoNoDefaultBranchNotBlocked: an empty repo (no default branch) is not
// blocked — evaluateRepo emits no_default_branch (warn) and returns.
func TestGuardRepoNoDefaultBranchNotBlocked(t *testing.T) {
	f := &fakeForge{identity: forge.BotIdentity{ForgeUserID: 42}}
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("")})
	if res.Blocked {
		t.Fatalf("no-default-branch repo must not block, got %+v", res)
	}
}

// TestGuardRepoOverrideWaivesPushGrant: Overridden + WriteRoleCanPush → not
// blocked, and the finding survives with Severity overridden (waived, not skipped).
func TestGuardRepoOverrideWaivesPushGrant(t *testing.T) {
	f := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:    map[int64]protResult{1: {bp: forge.BranchProtection{Protected: true, WriteRoleCanPush: true}}},
	}
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main"), Overridden: true})
	if res.Blocked {
		t.Fatalf("overridden push grant must not block, got %+v", res)
	}
	fnd, ok := findFinding(res.Findings, CodeWriteRoleCanPush)
	if !ok {
		t.Fatalf("finding must survive the override (waived, not skipped), got %+v", res.Findings)
	}
	if fnd.Severity != SeverityOverridden {
		t.Fatalf("finding severity = %q, want overridden", fnd.Severity)
	}
}

// TestGuardRepoOverrideWaivesUnprotectedProtectedFirst: Overridden + unprotected
// branch → not blocked, but evaluateRepo still SAW it (Protected-first not
// reintroduced): the default_branch_unprotected finding exists with severity
// overridden — evidence it was evaluated and waived, not skipped.
func TestGuardRepoOverrideWaivesUnprotectedProtectedFirst(t *testing.T) {
	f := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:    map[int64]protResult{1: {bp: forge.BranchProtection{Protected: false}}},
	}
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main"), Overridden: true})
	if res.Blocked {
		t.Fatalf("overridden unprotected branch must not block, got %+v", res)
	}
	fnd, ok := findFinding(res.Findings, CodeDefaultBranchUnprotected)
	if !ok {
		t.Fatalf("unprotected finding must be present (seen then waived, not skipped), got %+v", res.Findings)
	}
	if fnd.Severity != SeverityOverridden {
		t.Fatalf("finding severity = %q, want overridden", fnd.Severity)
	}
}

// TestGuardRepoOverrideNeverWaivesUnreadable: Overridden + DefaultBranchProtection
// error → STILL blocked (protection_unreadable is never waived, R8).
func TestGuardRepoOverrideNeverWaivesUnreadable(t *testing.T) {
	f := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42},
		roles:    map[int64]roleResult{1: {role: forge.RoleWrite, member: true}},
		prots:    map[int64]protResult{1: {err: errors.New("403 from protected-branches")}},
	}
	svc := NewService(newFakeStore(), &fakeBuilder{forge: f})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main"), Overridden: true})
	if !res.Blocked {
		t.Fatalf("override must NOT waive protection_unreadable (R8), got %+v", res)
	}
	fnd, ok := findFinding(res.Findings, CodeProtectionUnreadable)
	if !ok || fnd.Severity != SeverityBlock {
		t.Fatalf("protection_unreadable must stay BLOCK under override, got %+v", res.Findings)
	}
}

// TestGuardRepoOverrideNeverWaivesClientBuildError: Overridden + client-build error
// → STILL blocked.
func TestGuardRepoOverrideNeverWaivesClientBuildError(t *testing.T) {
	svc := NewService(newFakeStore(), &fakeBuilder{buildErr: errors.New("cipher: message authentication failed")})
	res := svc.GuardRepo(context.Background(), GuardInput{Repo: aGuardRepo("main"), Overridden: true})
	if !res.Blocked {
		t.Fatalf("override must NOT waive a client-build failure, got %+v", res)
	}
	if _, ok := findFinding(res.Findings, CodeProtectionUnreadable); !ok {
		t.Fatalf("want protection_unreadable, got %+v", res.Findings)
	}
}

// TestDowngradeOverridden covers the primitive directly: waivable BLOCK→overridden;
// protection_unreadable stays BLOCK; warns unchanged; overridden=false is a no-op;
// the input slice and its elements are not mutated.
func TestDowngradeOverridden(t *testing.T) {
	in := []Finding{
		newFinding(CodeWriteRoleCanPush, "push"),        // waivable BLOCK
		newFinding(CodeDefaultBranchUnprotected, "unp"), // waivable BLOCK
		newFinding(CodeProtectionUnreadable, "unread"),  // BLOCK, never waivable
		newFinding(CodeBotNotMember, "weak"),            // warn
	}

	// overridden=false is a no-op returning the same slice.
	if got := DowngradeOverridden(in, false); &got[0] != &in[0] {
		t.Fatalf("overridden=false must return the input slice unchanged")
	}

	out := DowngradeOverridden(in, true)
	if f, _ := findFinding(out, CodeWriteRoleCanPush); f.Severity != SeverityOverridden {
		t.Fatalf("waivable BLOCK write_role_can_push not downgraded: %+v", f)
	}
	if f, _ := findFinding(out, CodeDefaultBranchUnprotected); f.Severity != SeverityOverridden {
		t.Fatalf("waivable BLOCK default_branch_unprotected not downgraded: %+v", f)
	}
	if f, _ := findFinding(out, CodeProtectionUnreadable); f.Severity != SeverityBlock {
		t.Fatalf("protection_unreadable must stay BLOCK: %+v", f)
	}
	if f, _ := findFinding(out, CodeBotNotMember); f.Severity != SeverityWarn {
		t.Fatalf("warn finding must be unchanged: %+v", f)
	}

	// Input not mutated.
	if f, _ := findFinding(in, CodeWriteRoleCanPush); f.Severity != SeverityBlock {
		t.Fatalf("DowngradeOverridden mutated its input: %+v", f)
	}
}

// TestSweepToleratesDeletedMidSweep: a 0-row write-back (connection deleted mid
// -sweep) is not an error.
func TestSweepToleratesDeletedMidSweep(t *testing.T) {
	conn := aConn()
	st := newFakeStore()
	st.updateRows = 0 // the UPDATE matched no rows
	st.conns = []store.ForgeConnection{conn}
	f := &fakeForge{identity: forge.BotIdentity{ForgeUserID: 42}, tokenInfo: forge.TokenInfo{Scopes: []string{"api"}, Active: true}}
	svc := NewService(st, &fakeBuilder{forge: f})

	res, err := svc.CheckAllConnections(context.Background())
	if err != nil {
		t.Fatalf("a 0-row write-back must be tolerated, got %v", err)
	}
	if res.Errors != 0 {
		t.Fatalf("0-row write must not tally an error, got %+v", res)
	}
}
