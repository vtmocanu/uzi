package setupcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The token is assembled at runtime so no provider-token literal sits in source.
var testToken = "glpat-" + "fakeSetupToken" + "0123456789"

const testBase = "https://forge.example"

type fakeForge struct {
	forgetest.BaseFake
	verifyCalls int
	verifyErr   error
	projects    []forge.Project
	labelsErr   error
}

func (f *fakeForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	f.verifyCalls++
	return forge.BotIdentity{Username: "uzi-bot", ForgeUserID: 7}, f.verifyErr
}
func (f *fakeForge) ListProjects(context.Context) ([]forge.Project, error) { return f.projects, nil }
func (f *fakeForge) EnsureLabels(context.Context, int64, []forge.Label) error {
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
	blocked    map[string]bool // by path_with_namespace
}

func (g *fakeGuard) CheckToken(context.Context, forge.CodeHost, forge.Type, bool) privcheck.TokenReport {
	return privcheck.TokenReport{Violations: g.violations}
}
func (g *fakeGuard) GuardRepo(_ context.Context, in privcheck.GuardInput) privcheck.GuardResult {
	if g.blocked[in.Repo.Path] {
		return privcheck.GuardResult{Blocked: true, Findings: []privcheck.Finding{
			{Code: "bot_can_push_default", Severity: privcheck.SeverityBlock, Message: "the bot can push to " + in.Repo.Path},
			{Code: "warn_only", Severity: privcheck.SeverityWarn, Message: "a warning that is not a reason"},
		}}
	}
	return privcheck.GuardResult{}
}

// fakeStore is an in-memory Store. Every write bumps writes, so a refusal path
// can assert that nothing was written.
type fakeStore struct {
	user      *store.User
	conn      *store.ForgeConnection
	repos     map[uuid.UUID]*store.Repo
	columns   map[uuid.UUID]int
	flips     map[uuid.UUID]int
	writes    int
	creations int
	// getRepoErr makes GetRepoForUser fail; provisionsvc logs that error
	// through slog, the path a scrub test needs.
	getRepoErr error
	// onGetRepo, when set, runs at the start of GetRepoForUser, so a test can
	// hold one run inside the enable step while another run starts.
	onGetRepo func()
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		user:    &store.User{ID: uuid.New(), Email: "admin@example.com", IsAdmin: true},
		repos:   map[uuid.UUID]*store.Repo{},
		columns: map[uuid.UUID]int{},
		flips:   map[uuid.UUID]int{},
	}
}

func (s *fakeStore) GetUserByEmail(context.Context, string) (store.User, error) {
	if s.user == nil {
		return store.User{}, pgx.ErrNoRows
	}
	return *s.user, nil
}
func (s *fakeStore) GetForgeConnectionByTarget(context.Context, store.GetForgeConnectionByTargetParams) (store.ForgeConnection, error) {
	if s.conn == nil {
		return store.ForgeConnection{}, pgx.ErrNoRows
	}
	return *s.conn, nil
}
func (s *fakeStore) CreateForgeConnectionIfAbsent(_ context.Context, a store.CreateForgeConnectionIfAbsentParams) (store.ForgeConnection, error) {
	s.writes++
	s.creations++
	c := store.ForgeConnection{ID: uuid.New(), UserID: a.UserID, ForgeType: a.ForgeType, BaseUrl: a.BaseUrl, BotUsername: a.BotUsername, TokenCiphertext: a.TokenCiphertext}
	s.conn = &c
	return c, nil
}
func (s *fakeStore) UpsertForgeConnection(context.Context, store.UpsertForgeConnectionParams) (store.ForgeConnection, error) {
	s.writes++
	return store.ForgeConnection{}, errors.New("setup must never call the upsert")
}
func (s *fakeStore) GetForgeConnectionForUser(_ context.Context, a store.GetForgeConnectionForUserParams) (store.ForgeConnection, error) {
	if s.conn == nil || s.conn.ID != a.ID {
		return store.ForgeConnection{}, pgx.ErrNoRows
	}
	return *s.conn, nil
}
func (s *fakeStore) UpsertRepo(_ context.Context, a store.UpsertRepoParams) (store.Repo, error) {
	s.writes++
	for _, r := range s.repos {
		if r.ForgeProjectID == a.ForgeProjectID {
			return *r, nil
		}
	}
	r := &store.Repo{ID: uuid.New(), ConnectionID: a.ConnectionID, ForgeProjectID: a.ForgeProjectID, PathWithNamespace: a.PathWithNamespace}
	s.repos[r.ID] = r
	return *r, nil
}
func (s *fakeStore) ListReposByConnectionForUser(context.Context, store.ListReposByConnectionForUserParams) ([]store.Repo, error) {
	var out []store.Repo
	for _, r := range s.repos {
		out = append(out, *r)
	}
	return out, nil
}
func (s *fakeStore) GetRepoForUser(_ context.Context, a store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	if s.onGetRepo != nil {
		s.onGetRepo()
	}
	if s.getRepoErr != nil {
		return store.GetRepoForUserRow{}, s.getRepoErr
	}
	r, ok := s.repos[a.ID]
	if !ok {
		return store.GetRepoForUserRow{}, pgx.ErrNoRows
	}
	return store.GetRepoForUserRow{ID: r.ID, ForgeProjectID: r.ForgeProjectID, PathWithNamespace: r.PathWithNamespace, ForgeType: "github", BaseUrl: testBase}, nil
}
func (s *fakeStore) SetRepoEnabledForUser(_ context.Context, a store.SetRepoEnabledForUserParams) (store.Repo, error) {
	s.writes++
	s.flips[a.ID]++
	r := s.repos[a.ID]
	r.Enabled = a.Enabled
	return *r, nil
}
func (s *fakeStore) DeletePendingGuardrailOverrideRequestsForRepo(context.Context, uuid.UUID) error {
	return nil
}
func (s *fakeStore) CountBoardColumns(_ context.Context, id uuid.UUID) (int64, error) {
	return int64(s.columns[id]), nil
}
func (s *fakeStore) InsertDefaultBoardColumns(_ context.Context, a store.InsertDefaultBoardColumnsParams) error {
	s.writes++
	if s.columns[a.RepoID] == 0 {
		s.columns[a.RepoID] = len(a.LabelNames)
	}
	return nil
}

func (s *fakeStore) flipsFor(path string) int {
	for id, r := range s.repos {
		if r.PathWithNamespace == path {
			return s.flips[id]
		}
	}
	return 0
}

// testJoinToken is a fixture join token; assembled so no token-shaped literal
// sits in source.
var testJoinToken = "uzw" + "_fixtureJoinToken" + "9876543210"

// fakeWorkers stands in for the per-owner serialised create-first step.
type fakeWorkers struct {
	exists       bool
	err          error
	calls        int
	gotName      string
	gotTemplate  string
	gotOwner     uuid.UUID
	errWithToken bool
}

func (w *fakeWorkers) CreateFirst(_ context.Context, owner uuid.UUID, name, template string) (WorkerResult, error) {
	w.calls++
	w.gotOwner, w.gotName, w.gotTemplate = owner, name, template
	if w.err != nil {
		if w.errWithToken {
			return WorkerResult{}, errors.New("insert failed for " + testJoinToken + " and " + testToken)
		}
		return WorkerResult{}, w.err
	}
	if w.exists {
		return WorkerResult{Created: false}, nil
	}
	return WorkerResult{Created: true, WorkerID: uuid.New(), Name: name, JoinToken: testJoinToken}, nil
}

type harness struct {
	workers *fakeWorkers
	st      *fakeStore
	forge   *fakeForge
	guard   *fakeGuard
	schema  error
	cfgErr  error
	stdin   io.Reader
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	opened  bool
	allowed []string
	openErr error
}

func newHarness() *harness {
	return &harness{
		workers: &fakeWorkers{},
		st:      newFakeStore(),
		forge:   &fakeForge{projects: []forge.Project{{ForgeProjectID: 1, PathWithNamespace: "acme/app"}, {ForgeProjectID: 2, PathWithNamespace: "acme/lib"}}},
		guard:   &fakeGuard{blocked: map[string]bool{}},
		stdin:   strings.NewReader(testToken + "\n"),
	}
}

func (h *harness) env() Env {
	return Env{
		LoadConfig: func() (config.SetupConfig, error) {
			if h.cfgErr != nil {
				return config.SetupConfig{}, h.cfgErr
			}
			allowed := h.allowed
			if allowed == nil {
				allowed = []string{testBase}
			}
			return config.SetupConfig{SeedEmail: "admin@example.com", ForgeAllowedBaseURLs: allowed}, nil
		},
		Open: func(context.Context, config.SetupConfig) (*Backend, func(), error) {
			h.opened = true
			if h.openErr != nil {
				return nil, nil, h.openErr
			}
			return &Backend{
				Q: h.st, Forges: fakeForges{h.forge}, Guard: h.guard, Workers: h.workers,
				CheckSchema: func(context.Context) error { return h.schema },
			}, func() {}, nil
		},
	}
}

func (h *harness) run(ctx context.Context, args ...string) int {
	return Run(ctx, args, h.stdin, &h.stdout, &h.stderr, h.env())
}

func baseArgs(extra ...string) []string {
	return append([]string{"--forge-type", "github", "--forge-base-url", testBase, "--forge-token-file", "-"}, extra...)
}

func (h *harness) report(t *testing.T) Report {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(h.stdout.Bytes()))
	var rep Report
	if err := dec.Decode(&rep); err != nil {
		t.Fatalf("stdout is not a JSON report: %v\n%s", err, h.stdout.String())
	}
	if dec.More() {
		t.Fatalf("stdout holds more than one JSON document:\n%s", h.stdout.String())
	}
	return rep
}

func repoStatus(rep Report, path string) string {
	for _, r := range rep.Repos {
		if r.Path == path {
			return r.Status
		}
	}
	return "<absent>"
}

func TestRunConnectsForgeAndEnablesReposWithBoardColumns(t *testing.T) {
	h := newHarness()
	code := h.run(context.Background(), baseArgs("--repos", "acme/app, acme/lib")...)
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	rep := h.report(t)
	if rep.SchemaVersion != ReportSchemaVersion || rep.Status != ReportOK {
		t.Fatalf("report header = %+v", rep)
	}
	if rep.Forge.Status != StatusConnected || rep.Discovery.Status != StatusDone {
		t.Fatalf("forge=%+v discovery=%+v", rep.Forge, rep.Discovery)
	}
	for _, p := range []string{"acme/app", "acme/lib"} {
		if got := repoStatus(rep, p); got != StatusEnabled {
			t.Errorf("%s status = %q, want enabled", p, got)
		}
	}
	if rep.Worker.Status != StatusNotRequested {
		t.Errorf("worker = %+v, want not_requested", rep.Worker)
	}
	if rep.IssueSync != "pending" {
		t.Errorf("issue_sync = %q, want pending", rep.IssueSync)
	}
	for id, r := range h.st.repos {
		if !r.Enabled || h.st.columns[id] != len(forgesvc.DefaultColumns) {
			t.Errorf("repo %s enabled=%v columns=%d, want enabled with the default columns", r.PathWithNamespace, r.Enabled, h.st.columns[id])
		}
	}
}

// Regression for the bypass (PRD #3760): the forge seed enables repos without
// the live guard. setup must refuse a blocked repo and never flip it.
func TestRunDoesNotEnableARepoWhoseGuardBlocks(t *testing.T) {
	h := newHarness()
	h.guard.blocked["acme/lib"] = true
	code := h.run(context.Background(), baseArgs("--repos", "acme/app,acme/lib")...)
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 1", code)
	}
	rep := h.report(t)
	if got := repoStatus(rep, "acme/app"); got != StatusEnabled {
		t.Errorf("acme/app = %q, want enabled", got)
	}
	var lib RepoStep
	for _, r := range rep.Repos {
		if r.Path == "acme/lib" {
			lib = r
		}
	}
	if lib.Status != StatusRefused {
		t.Fatalf("acme/lib = %+v, want refused", lib)
	}
	if len(lib.Findings) != 1 || !strings.Contains(lib.Findings[0], "acme/lib") {
		t.Errorf("findings = %v, want only the blocking reason", lib.Findings)
	}
	if n := h.st.flipsFor("acme/lib"); n != 0 {
		t.Fatalf("a repo whose guard blocks was enabled (%d flips)", n)
	}
	for id, r := range h.st.repos {
		if r.PathWithNamespace == "acme/lib" && (r.Enabled || h.st.columns[id] != 0) {
			t.Errorf("blocked repo changed: enabled=%v columns=%d", r.Enabled, h.st.columns[id])
		}
	}
	if h.st.conn == nil {
		t.Error("completed writes (the connection) must remain")
	}
}

func TestRunNeverPrintsTheForgeToken(t *testing.T) {
	cases := map[string]func(h *harness){
		"success": func(*harness) {},
		// A driver that leaked the token into its error text is the case the
		// backstop exists for.
		"leaky verify error": func(h *harness) { h.forge.verifyErr = errors.New("401 for token " + testToken) },
		"leaky label error": func(h *harness) {
			h.forge.labelsErr = errors.New("forge said no to " + testToken)
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			setup(h)
			h.run(context.Background(), baseArgs("--repos", "acme/app")...)
			if strings.Contains(h.stdout.String(), testToken) || strings.Contains(h.stderr.String(), testToken) {
				t.Fatalf("the token reached an output stream\nstdout:\n%s\nstderr:\n%s", h.stdout.String(), h.stderr.String())
			}
			if h.stdout.Len() == 0 {
				t.Fatal("no report written")
			}
		})
	}
}

func TestRunReusesAnExistingConnectionUntouched(t *testing.T) {
	h := newHarness()
	existing := store.ForgeConnection{ID: uuid.New(), ForgeType: "github", BaseUrl: testBase, BotUsername: "old-bot", TokenCiphertext: []byte("sealed:old")}
	h.st.conn = &existing
	if code := h.run(context.Background(), baseArgs("--repos", "acme/app")...); code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	rep := h.report(t)
	if rep.Forge.Status != StatusPresent || rep.Forge.ConnectionID != existing.ID.String() {
		t.Fatalf("forge = %+v, want present", rep.Forge)
	}
	if h.forge.verifyCalls != 0 || h.st.creations != 0 {
		t.Errorf("an existing connection must not be re-verified (%d) or rewritten (%d)", h.forge.verifyCalls, h.st.creations)
	}
	if string(h.st.conn.TokenCiphertext) != "sealed:old" {
		t.Error("the stored token changed")
	}
}

func TestRunForgeFailureSkipsDependentSteps(t *testing.T) {
	h := newHarness()
	h.forge.verifyErr = errors.New("connection refused")
	code := h.run(context.Background(), baseArgs("--repos", "acme/app,acme/lib")...)
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 1", code)
	}
	rep := h.report(t)
	if rep.Forge.Status != StatusFailed || rep.Discovery == nil || rep.Discovery.Status != StatusSkippedDependency {
		t.Fatalf("forge=%+v discovery=%+v", rep.Forge, rep.Discovery)
	}
	for _, p := range []string{"acme/app", "acme/lib"} {
		if got := repoStatus(rep, p); got != StatusSkippedDependency {
			t.Errorf("%s = %q, want skipped_dependency", p, got)
		}
	}
	if h.st.writes != 0 {
		t.Errorf("writes = %d, want 0", h.st.writes)
	}
}

func TestRunOverPrivilegedTokenIsRefusedWithItsReasons(t *testing.T) {
	h := newHarness()
	h.guard.violations = []string{"token has the admin scope"}
	code := h.run(context.Background(), baseArgs("--repos", "acme/app")...)
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 1", code)
	}
	rep := h.report(t)
	if rep.Forge.Status != StatusRefused || len(rep.Forge.Findings) != 1 {
		t.Fatalf("forge = %+v, want refused with the violation", rep.Forge)
	}
	if h.st.conn != nil {
		t.Error("an over-privileged token must not be stored")
	}
	if got := repoStatus(h.report(t), "acme/app"); got != StatusSkippedDependency {
		t.Errorf("repo = %q, want skipped_dependency", got)
	}
}

func TestRunMissingRepoDoesNotStopTheOthers(t *testing.T) {
	h := newHarness()
	code := h.run(context.Background(), baseArgs("--repos", "acme/nope,acme/app")...)
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 1", code)
	}
	rep := h.report(t)
	if repoStatus(rep, "acme/nope") != StatusNotFound || repoStatus(rep, "acme/app") != StatusEnabled {
		t.Fatalf("repos = %+v", rep.Repos)
	}
}

func TestRunRefusedColumnLabelsKeepTheRepoDisabled(t *testing.T) {
	h := newHarness()
	h.forge.labelsErr = errors.New("labels forbidden")
	code := h.run(context.Background(), baseArgs("--repos", "acme/app")...)
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 1", code)
	}
	if got := repoStatus(h.report(t), "acme/app"); got != StatusFailed {
		t.Errorf("repo = %q, want failed", got)
	}
	if n := h.st.flipsFor("acme/app"); n != 0 {
		t.Errorf("a repo without columns was enabled (%d flips)", n)
	}
}

func TestRunPreconditionsExitTwoAndWriteNothing(t *testing.T) {
	cases := map[string]func(h *harness){
		"owner missing":    func(h *harness) { h.st.user = nil },
		"owner not admin":  func(h *harness) { h.st.user.IsAdmin = false },
		"schema behind":    func(h *harness) { h.schema = errors.New("the database is at migration 1") },
		"config invalid":   func(h *harness) { h.cfgErr = errors.New("DATABASE_URL is required") },
		"base not allowed": func(h *harness) { h.allowed = []string{"https://other.example"} },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			setup(h)
			code := h.run(context.Background(), baseArgs("--repos", "acme/app")...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want 2", code)
			}
			if h.report(t).Status != ReportRejected {
				t.Error("report must say rejected")
			}
			if h.st.writes != 0 {
				t.Errorf("writes = %d, want none", h.st.writes)
			}
		})
	}
}

func TestRunInvalidInputExitsTwoBeforeAnyIO(t *testing.T) {
	long := strings.Repeat("a", MaxRepoInputBytes+1)
	manyRepos := make([]string, MaxRepos+1)
	for i := range manyRepos {
		manyRepos[i] = "acme/r" + strconv.Itoa(i)
	}
	cases := map[string][]string{
		"no forge type":        {"--forge-base-url", testBase, "--forge-token-file", "-"},
		"unknown forge type":   {"--forge-type", "svn", "--forge-base-url", testBase, "--forge-token-file", "-"},
		"no base url":          {"--forge-type", "github", "--forge-token-file", "-"},
		"no token file":        {"--forge-type", "github", "--forge-base-url", testBase},
		"token as a flag":      baseArgs("--forge-token", testToken),
		"positional":           baseArgs("extra"),
		"bad repo path":        baseArgs("--repos", "justaname"),
		"too many repos":       baseArgs("--repos", strings.Join(manyRepos, ",")),
		"repo input too large": baseArgs("--repos", long),
		"template no worker":   baseArgs("--worker-template", "base"),
		"bad worker name":      baseArgs("--worker-name", "a\nb"),
		"long worker name":     baseArgs("--worker-name", strings.Repeat("w", 201)),
		"unknown template":     baseArgs("--worker-name", "w", "--worker-template", "nope"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness()
			code := h.run(context.Background(), args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want 2\nstdout:\n%s", code, h.stdout.String())
			}
			if h.opened || h.st.writes != 0 {
				t.Errorf("opened=%v writes=%d: invalid input must fail before any I/O", h.opened, h.st.writes)
			}
			if strings.Contains(h.stdout.String()+h.stderr.String(), testToken) {
				t.Error("the token leaked")
			}
		})
	}
}

// Acceptance 1 (worker half): the worker is created for the owner, and the
// join token appears exactly once, in the stdout document, never on stderr.
func TestRunCreatesTheWorkerAndPrintsTheJoinTokenOnceOnStdout(t *testing.T) {
	h := newHarness()
	code := h.run(context.Background(), baseArgs("--repos", "acme/app", "--worker-name", "vps-1", "--worker-template", "base")...)
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	rep := h.report(t)
	if rep.Worker == nil || rep.Worker.Status != StatusCreated || rep.Worker.JoinToken != testJoinToken {
		t.Fatalf("worker = %+v, want created with the join token", rep.Worker)
	}
	if rep.Worker.WorkerID == "" || rep.Worker.Name != "vps-1" {
		t.Errorf("worker entry lacks id or name: %+v", rep.Worker)
	}
	if n := strings.Count(h.stdout.String(), testJoinToken); n != 1 {
		t.Errorf("join token appears %d times on stdout, want exactly 1", n)
	}
	if strings.Contains(h.stderr.String(), testJoinToken) {
		t.Error("the join token reached stderr")
	}
	if strings.Contains(h.stdout.String()+h.stderr.String(), testToken) {
		t.Error("the forge token leaked")
	}
	if h.workers.gotOwner != h.st.user.ID || h.workers.gotName != "vps-1" || h.workers.gotTemplate != "base" {
		t.Errorf("worker call = owner %v name %q template %q", h.workers.gotOwner, h.workers.gotName, h.workers.gotTemplate)
	}
}

// Acceptance 3: an existing worker gives exit 3 and no token.
func TestRunExistingWorkerExitsThreeWithoutAToken(t *testing.T) {
	h := newHarness()
	h.workers.exists = true
	code := h.run(context.Background(), baseArgs("--repos", "acme/app", "--worker-name", "vps-1")...)
	if code != ExitWorkerExists {
		t.Fatalf("exit = %d, want 3\nstderr:\n%s", code, h.stderr.String())
	}
	rep := h.report(t)
	if rep.Worker.Status != StatusExists || rep.Worker.JoinToken != "" || rep.ExitCode != ExitWorkerExists {
		t.Fatalf("report = %+v worker = %+v", rep, rep.Worker)
	}
	if strings.Contains(h.stdout.String()+h.stderr.String(), testJoinToken) {
		t.Error("a token was issued for an existing worker")
	}
}

// Exit 1 wins over exit 3, and a repo that is not enabled prevents the worker
// step entirely: no worker is created and no token is minted.
func TestRunRefusedRepoSkipsTheWorkerAndExitOneBeatsThree(t *testing.T) {
	for _, exists := range []bool{false, true} {
		h := newHarness()
		h.workers.exists = exists
		h.guard.blocked["acme/lib"] = true
		code := h.run(context.Background(), baseArgs("--repos", "acme/app,acme/lib", "--worker-name", "vps-1")...)
		if code != ExitFailed {
			t.Fatalf("exists=%v: exit = %d, want 1", exists, code)
		}
		rep := h.report(t)
		if rep.Worker.Status != StatusSkippedDependency {
			t.Errorf("exists=%v: worker = %+v, want skipped_dependency", exists, rep.Worker)
		}
		if h.workers.calls != 0 {
			t.Errorf("exists=%v: the worker step ran after a refused repo", exists)
		}
		if strings.Contains(h.stdout.String(), testJoinToken) {
			t.Error("a join token was minted")
		}
	}
}

func TestRunForgeFailureSkipsTheWorker(t *testing.T) {
	h := newHarness()
	h.forge.verifyErr = errors.New("connection refused")
	code := h.run(context.Background(), baseArgs("--repos", "acme/app", "--worker-name", "vps-1")...)
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 1", code)
	}
	if rep := h.report(t); rep.Worker.Status != StatusSkippedDependency || h.workers.calls != 0 {
		t.Errorf("worker = %+v calls = %d", rep.Worker, h.workers.calls)
	}
}

// A worker failure is exit 1 with a failed worker entry that carries neither
// token, even when the underlying error text does.
func TestRunWorkerFailureExitsOneAndLeaksNoToken(t *testing.T) {
	h := newHarness()
	h.workers.err = errors.New("database unavailable")
	h.workers.errWithToken = true
	code := h.run(context.Background(), baseArgs("--repos", "acme/app", "--worker-name", "vps-1")...)
	if code != ExitFailed {
		t.Fatalf("exit = %d, want 1", code)
	}
	rep := h.report(t)
	if rep.Worker.Status != StatusFailed || rep.Status != ReportFailed {
		t.Fatalf("report = %+v worker = %+v", rep, rep.Worker)
	}
	out := h.stdout.String() + h.stderr.String()
	if strings.Contains(out, testJoinToken) || strings.Contains(out, testToken) {
		t.Errorf("a token leaked:\n%s", out)
	}
}

func TestRunWithoutAWorkerNameNeverTouchesWorkers(t *testing.T) {
	h := newHarness()
	if code := h.run(context.Background(), baseArgs("--repos", "acme/app")...); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if h.workers.calls != 0 || h.report(t).Worker.Status != StatusNotRequested {
		t.Error("worker step must report not_requested and not run")
	}
}

// A worker name with no repos is valid: forge connect, then the worker.
func TestRunWorkerWithoutReposStillCreates(t *testing.T) {
	h := newHarness()
	if code := h.run(context.Background(), baseArgs("--worker-name", "vps-1")...); code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	if h.report(t).Worker.Status != StatusCreated {
		t.Error("worker not created")
	}
}

func TestRunRejectsAnOversizedTokenSourceBeforeAnyWrite(t *testing.T) {
	h := newHarness()
	h.stdin = strings.NewReader(strings.Repeat("x", MaxTokenBytes+1))
	if code := h.run(context.Background(), baseArgs("--repos", "acme/app")...); code != ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if h.opened || h.st.writes != 0 {
		t.Errorf("opened=%v writes=%d", h.opened, h.st.writes)
	}
}

func TestRunAcceptsATokenExactlyAtTheLimit(t *testing.T) {
	h := newHarness()
	h.stdin = strings.NewReader(strings.Repeat("x", MaxTokenBytes))
	if code := h.run(context.Background(), baseArgs()...); code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
}

func TestRunReadsTheTokenFromAFile(t *testing.T) {
	h := newHarness()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(testToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--forge-type", "github", "--forge-base-url", testBase, "--forge-token-file", path, "--repos", "acme/app"}
	if code := h.run(context.Background(), args...); code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	if string(h.st.conn.TokenCiphertext) != "sealed:"+testToken {
		t.Error("the file's token (without its newline) was not the one stored")
	}
}

// A blocked stdin read ends when the context ends. The pipe never delivers, so
// the only way Run returns is the cancellation; the test needs no sleeps.
func TestRunCancellationInterruptsABlockedStdinRead(t *testing.T) {
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()
	h := newHarness()
	h.stdin = pr
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- h.run(ctx, baseArgs("--repos", "acme/app")...) }()
	cancel()
	if code := <-done; code != ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	if h.opened || h.st.writes != 0 {
		t.Errorf("opened=%v writes=%d", h.opened, h.st.writes)
	}
}

func TestRunHelpExitsZeroWithoutAReport(t *testing.T) {
	h := newHarness()
	if code := h.run(context.Background(), "-h"); code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if h.stdout.Len() != 0 {
		t.Errorf("help must not write a report: %s", h.stdout.String())
	}
}
