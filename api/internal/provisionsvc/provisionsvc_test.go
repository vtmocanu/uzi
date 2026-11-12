package provisionsvc

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type fakeForge struct {
	forgetest.BaseFake
	identity  forge.BotIdentity
	projects  []forge.Project
	labelsErr error
	labelCall int
}

func (f *fakeForge) VerifyToken(context.Context) (forge.BotIdentity, error) { return f.identity, nil }
func (f *fakeForge) ListProjects(context.Context) ([]forge.Project, error) {
	return f.projects, nil
}
func (f *fakeForge) EnsureLabels(context.Context, int64, []forge.Label) error {
	f.labelCall++
	return f.labelsErr
}

type fakeForges struct{ f *fakeForge }

func (fakeForges) EncryptToken(pat string) ([]byte, error) { return []byte("sealed:" + pat), nil }
func (s fakeForges) ForgeForToken(forge.Type, string, string) (forge.Forge, error) {
	return s.f, nil
}
func (s fakeForges) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return s.f, nil
}

type fakeGuard struct {
	violations []string
	blocked    bool
	overridden []bool
}

func (g *fakeGuard) CheckToken(context.Context, forge.CodeHost, forge.Type, bool) privcheck.TokenReport {
	return privcheck.TokenReport{Violations: g.violations}
}
func (g *fakeGuard) GuardRepo(_ context.Context, in privcheck.GuardInput) privcheck.GuardResult {
	g.overridden = append(g.overridden, in.Overridden)
	return privcheck.GuardResult{Blocked: g.blocked}
}

type fakeStore struct {
	conn      *store.UpsertForgeConnectionParams
	created   *store.CreateForgeConnectionIfAbsentParams
	existing  *store.ForgeConnection // a row that already holds the target
	repoRow   store.GetRepoForUserRow
	repoErr   error
	upserted  []store.UpsertRepoParams
	enabled   bool
	settled   int
	columns   []string
	flipCalls int
}

func (s *fakeStore) UpsertForgeConnection(_ context.Context, a store.UpsertForgeConnectionParams) (store.ForgeConnection, error) {
	s.conn = &a
	return store.ForgeConnection{ID: uuid.New(), UserID: a.UserID, ForgeType: a.ForgeType, BaseUrl: a.BaseUrl}, nil
}
func (s *fakeStore) CreateForgeConnectionIfAbsent(_ context.Context, a store.CreateForgeConnectionIfAbsentParams) (store.ForgeConnection, error) {
	if s.existing != nil {
		return store.ForgeConnection{}, pgx.ErrNoRows
	}
	s.created = &a
	return store.ForgeConnection{ID: uuid.New(), UserID: a.UserID, ForgeType: a.ForgeType, BaseUrl: a.BaseUrl}, nil
}
func (s *fakeStore) GetForgeConnectionByTarget(context.Context, store.GetForgeConnectionByTargetParams) (store.ForgeConnection, error) {
	if s.existing == nil {
		return store.ForgeConnection{}, pgx.ErrNoRows
	}
	return *s.existing, nil
}
func (s *fakeStore) GetForgeConnectionForUser(_ context.Context, a store.GetForgeConnectionForUserParams) (store.ForgeConnection, error) {
	return store.ForgeConnection{ID: a.ID, UserID: a.UserID, ForgeType: "gitlab", BaseUrl: "https://forge.example"}, nil
}
func (s *fakeStore) UpsertRepo(_ context.Context, a store.UpsertRepoParams) (store.Repo, error) {
	s.upserted = append(s.upserted, a)
	return store.Repo{ID: uuid.New()}, nil
}
func (s *fakeStore) GetRepoForUser(context.Context, store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	return s.repoRow, s.repoErr
}
func (s *fakeStore) SetRepoEnabledForUser(_ context.Context, a store.SetRepoEnabledForUserParams) (store.Repo, error) {
	s.flipCalls++
	s.enabled = a.Enabled
	return store.Repo{ID: a.ID, Enabled: a.Enabled}, nil
}
func (s *fakeStore) DeletePendingGuardrailOverrideRequestsForRepo(context.Context, uuid.UUID) error {
	s.settled++
	return nil
}
func (s *fakeStore) CountBoardColumns(context.Context, uuid.UUID) (int64, error) {
	return int64(len(s.columns)), nil
}
func (s *fakeStore) InsertDefaultBoardColumns(_ context.Context, a store.InsertDefaultBoardColumnsParams) error {
	if len(s.columns) == 0 {
		s.columns = append(s.columns, a.LabelNames...)
	}
	return nil
}

func allowAll(string) bool { return true }

func asProvisionError(t *testing.T, err error) *Error {
	t.Helper()
	var pe *Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want *provisionsvc.Error", err)
	}
	return pe
}

func defaultColumnNames() []string {
	var out []string
	for _, c := range forgesvc.DefaultColumns {
		out = append(out, c.Name)
	}
	return out
}

func TestConnectForgeRefusesOverPrivilegedToken(t *testing.T) {
	st := &fakeStore{}
	g := &fakeGuard{violations: []string{"token has the sudo scope"}}
	svc := New(st, fakeForges{&fakeForge{identity: forge.BotIdentity{Username: "bot", ForgeUserID: 7}}}, g, allowAll)

	_, err := svc.ConnectForge(context.Background(), uuid.New(), "gitlab", "https://forge.example", "tok")
	pe := asProvisionError(t, err)
	if pe.Kind != KindOverPrivileged {
		t.Fatalf("kind = %v, want KindOverPrivileged", pe.Kind)
	}
	if !slices.Equal(pe.Violations, g.violations) {
		t.Fatalf("violations = %v, want %v", pe.Violations, g.violations)
	}
	if st.conn != nil {
		t.Fatalf("an over-privileged token must not be stored: %+v", st.conn)
	}
}

func TestConnectForgeStoresSealedToken(t *testing.T) {
	st := &fakeStore{}
	svc := New(st, fakeForges{&fakeForge{identity: forge.BotIdentity{Username: "bot", ForgeUserID: 7}}}, &fakeGuard{}, allowAll)

	conn, err := svc.ConnectForge(context.Background(), uuid.New(), "forgejo", "https://forge.example/", "tok")
	if err != nil {
		t.Fatalf("ConnectForge: %v", err)
	}
	if conn.ForgeType != "forgejo" {
		t.Fatalf("forge type = %q, want forgejo", conn.ForgeType)
	}
	if st.conn == nil || string(st.conn.TokenCiphertext) != "sealed:tok" || st.conn.BotUsername != "bot" {
		t.Fatalf("stored connection = %+v", st.conn)
	}
}

// Issue #3785: a connect never guesses the forge. Both connect paths share
// validateConnection, so a missing or unknown type is refused before any forge
// call or store write, and the message names the supported values.
func TestConnectForgeRequiresForgeType(t *testing.T) {
	cases := []struct{ name, forgeType, want string }{
		{"empty", "", "forge_type is required; supported values: gitlab, forgejo, github"},
		{"blank", "  ", "forge_type is required; supported values: gitlab, forgejo, github"},
		{"unknown", "bitbucket", "unsupported forge type; supported values: gitlab, forgejo, github"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := &fakeStore{}
			svc := New(st, fakeForges{&fakeForge{identity: forge.BotIdentity{Username: "bot"}}}, &fakeGuard{}, allowAll)
			for _, connect := range []func() error{
				func() error {
					_, err := svc.ConnectForge(context.Background(), uuid.New(), tc.forgeType, "https://forge.example/", "tok")
					return err
				},
				func() error {
					_, _, err := svc.ConnectForgeCreateOnly(context.Background(), uuid.New(), tc.forgeType, "https://forge.example/", "tok")
					return err
				},
			} {
				var pe *Error
				if err := connect(); !errors.As(err, &pe) || pe.Kind != KindBadRequest || pe.Message != tc.want {
					t.Fatalf("err = %v, want KindBadRequest %q", err, tc.want)
				}
			}
			if st.conn != nil || st.created != nil {
				t.Fatalf("nothing may be stored: conn=%+v created=%+v", st.conn, st.created)
			}
		})
	}
}

func TestConnectForgeCreateOnlyStoresNewConnection(t *testing.T) {
	st := &fakeStore{}
	svc := New(st, fakeForges{&fakeForge{identity: forge.BotIdentity{Username: "bot", ForgeUserID: 7}}}, &fakeGuard{}, allowAll)

	conn, created, err := svc.ConnectForgeCreateOnly(context.Background(), uuid.New(), "github", "https://forge.example/", "tok")
	if err != nil || !created {
		t.Fatalf("created=%v err=%v, want a new connection", created, err)
	}
	if st.created == nil || string(st.created.TokenCiphertext) != "sealed:tok" || conn.BaseUrl != "https://forge.example" {
		t.Fatalf("stored = %+v, conn = %+v", st.created, conn)
	}
	if st.conn != nil {
		t.Fatal("create-only must never call the upsert")
	}
}

func TestConnectForgeCreateOnlyReusesWinnerWithoutRotating(t *testing.T) {
	winner := store.ForgeConnection{ID: uuid.New(), ForgeType: "github", BaseUrl: "https://forge.example", TokenCiphertext: []byte("sealed:old")}
	st := &fakeStore{existing: &winner}
	svc := New(st, fakeForges{&fakeForge{identity: forge.BotIdentity{Username: "bot", ForgeUserID: 7}}}, &fakeGuard{}, allowAll)

	conn, created, err := svc.ConnectForgeCreateOnly(context.Background(), uuid.New(), "github", "https://forge.example", "new-token")
	if err != nil || created {
		t.Fatalf("created=%v err=%v, want the existing connection reused", created, err)
	}
	if conn.ID != winner.ID || string(conn.TokenCiphertext) != "sealed:old" {
		t.Fatalf("conn = %+v, want the winner unchanged", conn)
	}
	if st.conn != nil || st.created != nil {
		t.Fatal("a reused connection must not be written")
	}
}

func TestConnectForgeCreateOnlyRefusesOverPrivilegedToken(t *testing.T) {
	st := &fakeStore{}
	g := &fakeGuard{violations: []string{"token has the sudo scope"}}
	svc := New(st, fakeForges{&fakeForge{}}, g, allowAll)

	_, _, err := svc.ConnectForgeCreateOnly(context.Background(), uuid.New(), "gitlab", "https://forge.example", "tok")
	if pe := asProvisionError(t, err); pe.Kind != KindOverPrivileged {
		t.Fatalf("kind = %v, want KindOverPrivileged", pe.Kind)
	}
	if st.created != nil {
		t.Fatal("an over-privileged token must not be stored")
	}
}

func TestConnectForgeRejectsBadInputBeforeAnyForgeCall(t *testing.T) {
	st := &fakeStore{}
	svc := New(st, fakeForges{&fakeForge{}}, &fakeGuard{}, func(string) bool { return false })
	for name, c := range map[string]struct{ typ, tok string }{
		"unsupported type": {"svn", "tok"},
		"blank token":      {"gitlab", "  "},
		"not allowlisted":  {"gitlab", "tok"},
	} {
		_, err := svc.ConnectForge(context.Background(), uuid.New(), c.typ, "https://forge.example", c.tok)
		if pe := asProvisionError(t, err); pe.Kind != KindBadRequest {
			t.Errorf("%s: kind = %v, want KindBadRequest", name, pe.Kind)
		}
	}
	if st.conn != nil {
		t.Fatal("nothing may be stored on a rejected input")
	}
}

func TestDiscoverReposUpsertsEveryProject(t *testing.T) {
	st := &fakeStore{}
	ff := &fakeForge{projects: []forge.Project{
		{ForgeProjectID: 1, PathWithNamespace: "g/a"},
		{ForgeProjectID: 2, PathWithNamespace: "g/b"},
	}}
	svc := New(st, fakeForges{ff}, &fakeGuard{}, allowAll)
	if _, err := svc.DiscoverRepos(context.Background(), uuid.New(), uuid.New()); err != nil {
		t.Fatalf("DiscoverRepos: %v", err)
	}
	if len(st.upserted) != 2 || st.upserted[1].PathWithNamespace != "g/b" {
		t.Fatalf("upserted = %+v", st.upserted)
	}
}

func repoRow() store.GetRepoForUserRow {
	return store.GetRepoForUserRow{ID: uuid.New(), ForgeType: "gitlab", BaseUrl: "https://forge.example", ForgeProjectID: 11, PathWithNamespace: "g/a"}
}

func TestEnableRepoGuardBlockLeavesRepoOffWithoutColumns(t *testing.T) {
	st := &fakeStore{repoRow: repoRow()}
	ff := &fakeForge{}
	svc := New(st, fakeForges{ff}, &fakeGuard{blocked: true}, allowAll)

	_, err := svc.EnableRepo(context.Background(), uuid.New(), st.repoRow.ID)
	pe := asProvisionError(t, err)
	if pe.Kind != KindGuardBlocked || !pe.Guard.Blocked {
		t.Fatalf("error = %+v, want a guard block", pe)
	}
	if st.flipCalls != 0 || len(st.columns) != 0 || ff.labelCall != 0 {
		t.Fatalf("a blocked repo must see no flip, no labels, no columns: flips=%d columns=%v labels=%d", st.flipCalls, st.columns, ff.labelCall)
	}
}

func TestEnableRepoHonoursLiveOverride(t *testing.T) {
	st := &fakeStore{repoRow: repoRow()}
	st.repoRow.GuardrailOverrideReason.Valid = true
	g := &fakeGuard{}
	svc := New(st, fakeForges{&fakeForge{}}, g, allowAll)
	if _, err := svc.EnableRepo(context.Background(), uuid.New(), st.repoRow.ID); err != nil {
		t.Fatalf("EnableRepo: %v", err)
	}
	if len(g.overridden) != 1 || !g.overridden[0] {
		t.Fatalf("guard Overridden = %v, want [true]", g.overridden)
	}
}

func TestEnableRepoLabelsRefusedKeepsRepoDisabled(t *testing.T) {
	st := &fakeStore{repoRow: repoRow()}
	ff := &fakeForge{labelsErr: errors.New("403 forbidden")}
	svc := New(st, fakeForges{ff}, &fakeGuard{}, allowAll)

	_, err := svc.EnableRepo(context.Background(), uuid.New(), st.repoRow.ID)
	pe := asProvisionError(t, err)
	if pe.Kind != KindBadGateway || pe.Message != "could not seed board labels on the forge: 403 forbidden" {
		t.Fatalf("error = %+v", pe)
	}
	if st.flipCalls != 0 || st.enabled || len(st.columns) != 0 {
		t.Fatalf("repo must stay disabled with 0 columns: flips=%d enabled=%v columns=%v", st.flipCalls, st.enabled, st.columns)
	}
}

// Regression (PRD #3760): enabling a repo with no board columns must create the
// default columns, in order. Before the fix only the first GET of the board did,
// so an API-enabled repo had none and cards never resolved a column.
func TestEnableRepoCreatesDefaultBoardColumns(t *testing.T) {
	st := &fakeStore{repoRow: repoRow()}
	ff := &fakeForge{}
	svc := New(st, fakeForges{ff}, &fakeGuard{}, allowAll)

	repo, err := svc.EnableRepo(context.Background(), uuid.New(), st.repoRow.ID)
	if err != nil {
		t.Fatalf("EnableRepo: %v", err)
	}
	if !repo.Enabled || !st.enabled {
		t.Fatal("repo must be enabled")
	}
	if want := defaultColumnNames(); !slices.Equal(st.columns, want) {
		t.Fatalf("board columns = %v, want %v", st.columns, want)
	}
	if ff.labelCall != 1 {
		t.Fatalf("EnsureLabels calls = %d, want 1", ff.labelCall)
	}
	if st.settled != 1 {
		t.Fatalf("pending override requests settled %d times, want 1", st.settled)
	}
}

func TestEnableRepoKeepsCustomColumns(t *testing.T) {
	st := &fakeStore{repoRow: repoRow(), columns: []string{"Triage", "Doing"}}
	ff := &fakeForge{}
	svc := New(st, fakeForges{ff}, &fakeGuard{}, allowAll)

	if _, err := svc.EnableRepo(context.Background(), uuid.New(), st.repoRow.ID); err != nil {
		t.Fatalf("EnableRepo: %v", err)
	}
	if want := []string{"Triage", "Doing"}; !slices.Equal(st.columns, want) {
		t.Fatalf("custom columns changed: %v", st.columns)
	}
	if ff.labelCall != 0 {
		t.Fatalf("a repo with columns must not touch the forge labels, got %d calls", ff.labelCall)
	}
}

func TestEnableRepoUnknownRepoIsNotFound(t *testing.T) {
	st := &fakeStore{repoErr: pgx.ErrNoRows}
	svc := New(st, fakeForges{&fakeForge{}}, &fakeGuard{}, allowAll)
	_, err := svc.EnableRepo(context.Background(), uuid.New(), uuid.New())
	if pe := asProvisionError(t, err); pe.Kind != KindNotFound || pe.Message != "repo not found" {
		t.Fatalf("error = %+v", pe)
	}
}
