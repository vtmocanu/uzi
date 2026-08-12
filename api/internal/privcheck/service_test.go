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
// re-run flips ok→violations when the bot's role drifts to Maintainer.
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

	// Drift: a teammate promotes the bot to Maintainer; the next sweep flips it.
	f.roles[1] = roleResult{role: forge.RoleAdmin, member: true}
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
