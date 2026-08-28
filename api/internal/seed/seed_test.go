package seed

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeForge is a mocked Forge whose VerifyToken and ListProjects are scripted;
// the label/issue methods are unused by the seed path.
type fakeForge struct {
	identity   forge.BotIdentity
	verifyErr  error
	projects   []forge.Project
	projectErr error
}

func (f *fakeForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return f.identity, f.verifyErr
}
func (f *fakeForge) ListProjects(context.Context) ([]forge.Project, error) {
	return f.projects, f.projectErr
}
func (f *fakeForge) ListLabels(context.Context, int64) ([]forge.Label, error) { return nil, nil }
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
func (f *fakeForge) UserExists(context.Context, string) (bool, error) { return false, nil }
func (f *fakeForge) ProjectCIConfigPath(context.Context, int64) (string, error) {
	return "", nil
}
func (f *fakeForge) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return nil, nil
}
func (f *fakeForge) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return nil, nil
}
func (f *fakeForge) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	return forge.IssueNote{}, nil
}
func (f *fakeForge) GetMergeRequest(context.Context, int64, int64) (forge.MergeRequest, error) {
	return forge.MergeRequest{}, nil
}
func (f *fakeForge) ListMergeRequestComments(context.Context, int64, int64) ([]forge.MRComment, error) {
	return nil, nil
}
func (f *fakeForge) ReplyMergeRequestComment(context.Context, int64, int64, string, string) error {
	return nil
}
func (f *fakeForge) ResolveMergeRequestThread(context.Context, int64, int64, string) error {
	return nil
}
func (f *fakeForge) TokenInfo(context.Context) (forge.TokenInfo, error) {
	return forge.TokenInfo{}, nil
}
func (f *fakeForge) ProjectRole(context.Context, int64, int64) (forge.Role, bool, error) {
	return forge.RoleNone, false, nil
}
func (f *fakeForge) DefaultBranchProtection(context.Context, int64, string, int64) (forge.BranchProtection, error) {
	return forge.BranchProtection{}, nil
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

// fakeSvc stands in for *forgesvc.Service. It records whether a client was built
// (to prove the existing-connection path never re-verifies) and seals PATs with
// a recognizable prefix so the stored ciphertext can be asserted.
type fakeSvc struct {
	forge    forge.Forge
	buildErr error
	built    int
}

func (s *fakeSvc) EncryptToken(pat string) ([]byte, error) { return []byte("sealed:" + pat), nil }
func (s *fakeSvc) ForgeForToken(forge.Type, string, string) (forge.Forge, error) {
	s.built++
	return s.forge, s.buildErr
}

// fakeStore records the seed's writes, standing in for *store.Queries.
type fakeStore struct {
	user    store.User
	userErr error
	conns   []store.ForgeConnection
	connErr error

	upsertedConn  *store.UpsertForgeConnectionParams
	upsertedRepos []store.UpsertRepoParams
	enabled       []store.SetRepoEnabledForUserParams

	repoIDByPath map[string]uuid.UUID
}

func (s *fakeStore) GetUserByEmail(context.Context, string) (store.User, error) {
	return s.user, s.userErr
}
func (s *fakeStore) ListForgeConnectionsByUser(context.Context, uuid.UUID) ([]store.ForgeConnection, error) {
	return s.conns, s.connErr
}
func (s *fakeStore) UpsertForgeConnection(_ context.Context, arg store.UpsertForgeConnectionParams) (store.ForgeConnection, error) {
	s.upsertedConn = &arg
	return store.ForgeConnection{ID: uuid.New(), UserID: arg.UserID, ForgeType: arg.ForgeType, BaseUrl: arg.BaseUrl}, nil
}
func (s *fakeStore) UpsertRepo(_ context.Context, arg store.UpsertRepoParams) (store.Repo, error) {
	s.upsertedRepos = append(s.upsertedRepos, arg)
	if s.repoIDByPath == nil {
		s.repoIDByPath = map[string]uuid.UUID{}
	}
	id := uuid.New()
	s.repoIDByPath[arg.PathWithNamespace] = id
	return store.Repo{ID: id, ConnectionID: arg.ConnectionID, ForgeProjectID: arg.ForgeProjectID, PathWithNamespace: arg.PathWithNamespace}, nil
}
func (s *fakeStore) SetRepoEnabledForUser(_ context.Context, arg store.SetRepoEnabledForUserParams) (store.Repo, error) {
	s.enabled = append(s.enabled, arg)
	return store.Repo{ID: arg.ID, Enabled: arg.Enabled}, nil
}

func baseCfg() config.Config {
	return config.Config{
		SeedEmail:        "admin@uzi.test",
		SeedForgePAT:     "glpat-xxx",
		SeedForgeBaseURL: "https://gitlab.example.com",
		SeedForgeRepos:   []string{"vtmocanu/uzi", "vtmocanu/missing"},
	}
}

func TestForgeConnectionHappyPath(t *testing.T) {
	userID := uuid.New()
	st := &fakeStore{user: store.User{ID: userID, Email: "admin@uzi.test"}}
	ff := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42, Username: "uzi-bot"},
		projects: []forge.Project{
			{ForgeProjectID: 1, PathWithNamespace: "vtmocanu/uzi", WebURL: "https://github.com/vtmocanu/uzi", DefaultBranch: "main"},
			{ForgeProjectID: 2, PathWithNamespace: "myorg/other", WebURL: "https://gitlab.example.com/myorg/other"},
		},
	}
	svc := &fakeSvc{forge: ff}

	if err := ForgeConnection(context.Background(), st, svc, baseCfg()); err != nil {
		t.Fatalf("ForgeConnection: %v", err)
	}
	if st.upsertedConn == nil {
		t.Fatal("expected a connection to be created")
	}
	if st.upsertedConn.BotUsername != "uzi-bot" || st.upsertedConn.BotForgeUserID != 42 {
		t.Fatalf("connection identity not taken from VerifyToken: %+v", st.upsertedConn)
	}
	if got := string(st.upsertedConn.TokenCiphertext); got != "sealed:glpat-xxx" {
		t.Fatalf("PAT not sealed via EncryptToken: %q", got)
	}
	if len(st.upsertedRepos) != 2 {
		t.Fatalf("expected both visible projects upserted as repos, got %d", len(st.upsertedRepos))
	}
	// Only the visible requested repo (vtmocanu/uzi) is enabled; vtmocanu/missing
	// is not among the projects, so it is warned about and skipped.
	if len(st.enabled) != 1 {
		t.Fatalf("expected exactly one repo enabled, got %d", len(st.enabled))
	}
	got := st.enabled[0]
	if got.ID != st.repoIDByPath["vtmocanu/uzi"] || !got.Enabled || got.UserID != userID {
		t.Fatalf("wrong repo enabled: %+v", got)
	}
}

func TestForgeConnectionExistingIsNoOp(t *testing.T) {
	st := &fakeStore{
		user:  store.User{ID: uuid.New()},
		conns: []store.ForgeConnection{{ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com"}},
	}
	svc := &fakeSvc{forge: &fakeForge{}}

	if err := ForgeConnection(context.Background(), st, svc, baseCfg()); err != nil {
		t.Fatalf("ForgeConnection: %v", err)
	}
	if svc.built != 0 {
		t.Fatal("existing connection must not be re-verified (no forge client should be built)")
	}
	if st.upsertedConn != nil || len(st.upsertedRepos) != 0 || len(st.enabled) != 0 {
		t.Fatal("existing connection must be left entirely untouched")
	}
}

func TestForgeConnectionDisabledIsNoOp(t *testing.T) {
	st := &fakeStore{}
	svc := &fakeSvc{forge: &fakeForge{}}
	cfg := baseCfg()
	cfg.SeedForgePAT = "" // disabled

	if err := ForgeConnection(context.Background(), st, svc, cfg); err != nil {
		t.Fatalf("disabled seed must be a no-op, got: %v", err)
	}
	if svc.built != 0 || st.upsertedConn != nil {
		t.Fatal("disabled seed must not touch the forge or the store")
	}
}

func TestForgeConnectionClientBuildErrorNonFatal(t *testing.T) {
	st := &fakeStore{user: store.User{ID: uuid.New()}}
	svc := &fakeSvc{buildErr: errors.New("bad base url")}

	if err := ForgeConnection(context.Background(), st, svc, baseCfg()); err != nil {
		t.Fatalf("client-build failure must be non-fatal, got: %v", err)
	}
	if st.upsertedConn != nil {
		t.Fatal("nothing should be persisted when the forge client cannot be built")
	}
}

func TestForgeConnectionVerifyErrorNonFatal(t *testing.T) {
	st := &fakeStore{user: store.User{ID: uuid.New()}}
	svc := &fakeSvc{forge: &fakeForge{verifyErr: errors.New("dial tcp: connection refused")}}

	if err := ForgeConnection(context.Background(), st, svc, baseCfg()); err != nil {
		t.Fatalf("forge-down (verify) must be non-fatal, got: %v", err)
	}
	if st.upsertedConn != nil {
		t.Fatal("nothing should be persisted when token verification fails")
	}
}

func TestForgeConnectionProjectListErrorNonFatal(t *testing.T) {
	st := &fakeStore{user: store.User{ID: uuid.New()}}
	svc := &fakeSvc{forge: &fakeForge{
		identity:   forge.BotIdentity{Username: "uzi-bot"},
		projectErr: errors.New("500 from forge"),
	}}

	if err := ForgeConnection(context.Background(), st, svc, baseCfg()); err != nil {
		t.Fatalf("project-list failure must be non-fatal, got: %v", err)
	}
	// The connection must NOT be created on a project-list failure: a half-seeded
	// connection would be skipped forever by the existing-connection guard,
	// stranding the repos.
	if st.upsertedConn != nil {
		t.Fatal("connection must not be created when project listing fails")
	}
}

func TestForgeConnectionUserLookupErrorIsFatal(t *testing.T) {
	st := &fakeStore{userErr: errors.New("db down")}
	svc := &fakeSvc{forge: &fakeForge{}}

	if err := ForgeConnection(context.Background(), st, svc, baseCfg()); err == nil {
		t.Fatal("a DB error looking up the seed admin must be fatal (returned)")
	}
}
