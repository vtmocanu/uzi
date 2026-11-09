package seed

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeForge is a mocked Forge whose VerifyToken and ListProjects are scripted;
// the label/issue methods are unused by the seed path.
type fakeForge struct {
	forgetest.BaseFake
	identity   forge.BotIdentity
	verifyErr  error
	projects   []forge.Project
	projectErr error
	labelsErr  error
}

func (f *fakeForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return f.identity, f.verifyErr
}
func (f *fakeForge) EnsureLabels(context.Context, int64, []forge.Label) error { return f.labelsErr }
func (f *fakeForge) ListProjects(context.Context) ([]forge.Project, error) {
	return f.projects, f.projectErr
}

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

	columns map[uuid.UUID][]string // board_columns label names per repo, in insert order
	// countErr and insertErr make the board column reads and writes fail, the
	// database errors boot must not swallow.
	countErr  error
	insertErr error
}

func (s *fakeStore) CountBoardColumns(_ context.Context, repoID uuid.UUID) (int64, error) {
	if s.countErr != nil {
		return 0, s.countErr
	}
	return int64(len(s.columns[repoID])), nil
}
func (s *fakeStore) InsertDefaultBoardColumns(_ context.Context, arg store.InsertDefaultBoardColumnsParams) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	if s.columns == nil {
		s.columns = map[uuid.UUID][]string{}
	}
	if len(s.columns[arg.RepoID]) == 0 {
		s.columns[arg.RepoID] = append(s.columns[arg.RepoID], arg.LabelNames...)
	}
	return nil
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

// Regression (PRD #3760): a repo the startup seed turns on must get the default
// board columns, in DefaultColumns order, or its cards never resolve a column.
func TestForgeConnectionCreatesBoardColumnsForEnabledRepo(t *testing.T) {
	st := &fakeStore{user: store.User{ID: uuid.New(), Email: "admin@uzi.test"}}
	ff := &fakeForge{
		identity: forge.BotIdentity{ForgeUserID: 42, Username: "uzi-bot"},
		projects: []forge.Project{{ForgeProjectID: 1, PathWithNamespace: "vtmocanu/uzi"}},
	}
	if err := ForgeConnection(context.Background(), st, &fakeSvc{forge: ff}, baseCfg()); err != nil {
		t.Fatalf("ForgeConnection: %v", err)
	}
	var want []string
	for _, c := range forgesvc.DefaultColumns {
		want = append(want, c.Name)
	}
	got := st.columns[st.repoIDByPath["vtmocanu/uzi"]]
	if !slices.Equal(got, want) {
		t.Fatalf("board columns = %v, want %v", got, want)
	}
	if len(st.enabled) != 1 {
		t.Fatalf("expected the repo enabled, got %d enables", len(st.enabled))
	}
}

// Regression (PR #3956 review, Decision 13): a forge that refuses the labels
// leaves the repo disabled, like the web enable and setup, and boot still
// succeeds. A repo enabled with 0 columns would make the run lifecycle read an
// automated card move as a person's.
func TestForgeConnectionLabelRefusalLeavesRepoDisabled(t *testing.T) {
	st := &fakeStore{user: store.User{ID: uuid.New(), Email: "admin@uzi.test"}}
	ff := &fakeForge{
		identity:  forge.BotIdentity{ForgeUserID: 42, Username: "uzi-bot"},
		projects:  []forge.Project{{ForgeProjectID: 1, PathWithNamespace: "vtmocanu/uzi"}},
		labelsErr: errors.New("403 from forge"),
	}
	if err := ForgeConnection(context.Background(), st, &fakeSvc{forge: ff}, baseCfg()); err != nil {
		t.Fatalf("a label refusal must not fail boot: %v", err)
	}
	if len(st.enabled) != 0 {
		t.Fatalf("a repo whose labels the forge refused must stay disabled, got %d enables", len(st.enabled))
	}
	if n := len(st.columns[st.repoIDByPath["vtmocanu/uzi"]]); n != 0 {
		t.Fatalf("no columns may be recorded when the labels failed, got %d", n)
	}
	// The connection stays created (create-only): the operator fixes the forge
	// access and reruns /server setup to enable the repo.
	if st.upsertedConn == nil {
		t.Fatal("the forge connection must still be created")
	}
}

// Only the forge refusing the labels is non-fatal. A database error while the
// seed creates the board columns must stop boot, as every other seed database
// error does (PR #3956 review, item 5).
func TestForgeConnectionBoardColumnDatabaseErrorIsFatal(t *testing.T) {
	for name, st := range map[string]*fakeStore{
		"count":  {countErr: errors.New("db down on count")},
		"insert": {insertErr: errors.New("db down on insert")},
	} {
		t.Run(name, func(t *testing.T) {
			st.user = store.User{ID: uuid.New(), Email: "admin@uzi.test"}
			ff := &fakeForge{
				identity: forge.BotIdentity{ForgeUserID: 42, Username: "uzi-bot"},
				projects: []forge.Project{{ForgeProjectID: 1, PathWithNamespace: "vtmocanu/uzi"}},
			}
			if err := ForgeConnection(context.Background(), st, &fakeSvc{forge: ff}, baseCfg()); err == nil {
				t.Fatal("a database error creating the board columns must be fatal (returned), got nil")
			}
		})
	}
}
