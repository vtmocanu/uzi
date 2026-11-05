package store_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/setupcmd"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The live-DB half of PRD #3760 M2: `setup` driven in-process (setupcmd.Run)
// against a real Postgres, the real provisionsvc, store and privcheck, and a
// scripted forge. It lives in this package because the live-DB sweep covers
// only the packages it enumerates; it needs no production code from here.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package).

// setupForge is the scripted forge. protection is keyed by forge project id.
type setupForge struct {
	forgetest.BaseFake
	projects   []forge.Project
	protection map[int64]forge.BranchProtection
}

func (f *setupForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return forge.BotIdentity{Username: "uzi-bot", ForgeUserID: 4242}, nil
}
func (f *setupForge) TokenInfo(context.Context) (forge.TokenInfo, error) {
	return forge.TokenInfo{}, forge.ErrTokenIntrospectionUnsupported
}
func (f *setupForge) ListProjects(context.Context) ([]forge.Project, error) { return f.projects, nil }
func (f *setupForge) ProjectRole(context.Context, int64, int64) (forge.Role, bool, error) {
	return forge.RoleWrite, true, nil
}
func (f *setupForge) DefaultBranchProtection(_ context.Context, id int64, _ string, _ int64) (forge.BranchProtection, error) {
	return f.protection[id], nil
}
func (f *setupForge) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }

// cleanProtection: protected, and the write role can neither push nor merge.
var cleanProtection = forge.BranchProtection{Protected: true}

// pushableProtection: the bot (write role) may push to main.
var pushableProtection = forge.BranchProtection{Protected: true, WriteRoleCanPush: true}

type setupFixture struct {
	pool    *pgxpool.Pool
	q       *store.Queries
	box     *secretbox.Box
	forge   *setupForge
	email   string
	userID  uuid.UUID
	baseURL string
	appID   int64
	libID   int64
}

func newSetupFixture(ctx context.Context, t *testing.T, admin bool) *setupFixture {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 3)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	f := &setupFixture{
		pool: pool, q: store.New(pool), box: box,
		email:   fmt.Sprintf("setup-%s@example.com", tag),
		userID:  uuid.New(),
		baseURL: fmt.Sprintf("https://f%s.example", tag),
		appID:   int64(uuid.New().ID()),
		libID:   int64(uuid.New().ID()),
	}
	f.forge = &setupForge{
		projects: []forge.Project{
			{ForgeProjectID: f.appID, PathWithNamespace: "acme/app-" + tag, WebURL: f.baseURL + "/acme/app", DefaultBranch: "main"},
			{ForgeProjectID: f.libID, PathWithNamespace: "acme/lib-" + tag, WebURL: f.baseURL + "/acme/lib", DefaultBranch: "main"},
		},
		protection: map[int64]forge.BranchProtection{f.appID: cleanProtection, f.libID: cleanProtection},
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', $3)`, f.userID, f.email, admin); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	return f
}

func (f *setupFixture) appPath() string { return f.forge.projects[0].PathWithNamespace }
func (f *setupFixture) libPath() string { return f.forge.projects[1].PathWithNamespace }

// env wires the real services over the fixture's pool. schema, when non-nil,
// replaces the production schema check.
func (f *setupFixture) env(pool *pgxpool.Pool) setupcmd.Env {
	return setupcmd.Env{
		LoadConfig: func() (config.SetupConfig, error) {
			return config.SetupConfig{SeedEmail: f.email, ForgeAllowedBaseURLs: []string{f.baseURL}}, nil
		},
		Open: func(context.Context, config.SetupConfig) (*setupcmd.Backend, func(), error) {
			q := store.New(pool)
			svc := forgesvc.NewWithForgeBuilder(q, f.box, 5*time.Second, nil,
				func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return f.forge, nil })
			return &setupcmd.Backend{
				Q: q, Forges: svc, Guard: privcheck.NewService(q, svc),
				CheckSchema: func(ctx context.Context) error { return setupcmd.CheckSchemaAtHead(ctx, pool) },
				Workers:     setupcmd.NewPoolWorkers(pool, f.box),
			}, func() {}, nil
		},
	}
}

func (f *setupFixture) run(ctx context.Context, env setupcmd.Env, token string, extra ...string) (int, setupcmd.Report, string, string) {
	args := append([]string{"--forge-type", "github", "--forge-base-url", f.baseURL, "--forge-token-file", "-"}, extra...)
	var stdout, stderr bytes.Buffer
	code := setupcmd.Run(ctx, args, strings.NewReader(token+"\n"), &stdout, &stderr, env)
	var rep setupcmd.Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		panic(fmt.Sprintf("stdout is not a report: %v\n%s", err, stdout.String()))
	}
	return code, rep, stdout.String(), stderr.String()
}

func (f *setupFixture) count(ctx context.Context, t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func repoState(rep setupcmd.Report, path string) setupcmd.RepoStep {
	for _, r := range rep.Repos {
		if r.Path == path {
			return r
		}
	}
	return setupcmd.RepoStep{Status: "<absent>"}
}

// Acceptance examples 1 (without the worker) and 2, then an idempotent re-run.
func TestSetupCmdAcceptanceLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSetupFixture(ctx, t, true)
	repos := f.appPath() + "," + f.libPath()
	token := "tok-" + uuid.NewString()

	// Example 2: the bot may push to lib's main.
	f.forge.protection[f.libID] = pushableProtection
	code, rep, stdout, stderr := f.run(ctx, f.env(f.pool), token, "--repos", repos)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, stdout)
	}
	if rep.Forge.Status != setupcmd.StatusConnected || repoState(rep, f.appPath()).Status != setupcmd.StatusEnabled {
		t.Fatalf("report = %s", stdout)
	}
	lib := repoState(rep, f.libPath())
	if lib.Status != setupcmd.StatusRefused || len(lib.Findings) == 0 {
		t.Fatalf("lib = %+v, want refused with reasons", lib)
	}
	if strings.Contains(stdout+stderr, token) {
		t.Fatal("the forge token reached an output stream")
	}
	enabledCount := func() int {
		return f.count(ctx, t, `SELECT count(*) FROM repos r JOIN forge_connections c ON c.id = r.connection_id
			WHERE c.user_id = $1 AND r.enabled`, f.userID)
	}
	if got := enabledCount(); got != 1 {
		t.Fatalf("enabled repos = %d, want only the clean one", got)
	}
	if got := f.count(ctx, t, `SELECT count(*) FROM board_columns bc JOIN repos r ON r.id = bc.repo_id
		JOIN forge_connections c ON c.id = r.connection_id WHERE c.user_id = $1 AND r.path_with_namespace = $2`,
		f.userID, f.libPath()); got != 0 {
		t.Fatalf("the refused repo has %d board columns, want 0", got)
	}

	// Example 1: protection fixed, run again. The connection is present, both repos enable.
	f.forge.protection[f.libID] = cleanProtection
	code, rep, stdout, _ = f.run(ctx, f.env(f.pool), "another-"+uuid.NewString(), "--repos", repos)
	if code != 0 {
		t.Fatalf("re-run exit = %d, want 0\n%s", code, stdout)
	}
	if rep.Forge.Status != setupcmd.StatusPresent {
		t.Fatalf("forge = %+v, want present", rep.Forge)
	}
	for _, p := range []string{f.appPath(), f.libPath()} {
		if repoState(rep, p).Status != setupcmd.StatusEnabled {
			t.Fatalf("%s = %+v", p, repoState(rep, p))
		}
	}
	if got := f.count(ctx, t, `SELECT count(*) FROM board_columns bc JOIN repos r ON r.id = bc.repo_id
		JOIN forge_connections c ON c.id = r.connection_id WHERE c.user_id = $1`, f.userID); got != 2*len(forgesvc.DefaultColumns) {
		t.Fatalf("board columns = %d, want %d", got, 2*len(forgesvc.DefaultColumns))
	}

	// A third run changes nothing.
	code, _, stdout, _ = f.run(ctx, f.env(f.pool), token, "--repos", repos)
	if code != 0 || enabledCount() != 2 {
		t.Fatalf("third run exit = %d enabled = %d\n%s", code, enabledCount(), stdout)
	}
}

func TestSetupCmdOwnerPreconditionsLiveDB(t *testing.T) {
	ctx := context.Background()
	for name, mut := range map[string]func(f *setupFixture){
		"owner missing": func(f *setupFixture) { f.email = "nobody-" + uuid.NewString() + "@example.com" },
		"owner not admin": func(f *setupFixture) {
			if _, err := f.pool.Exec(ctx, `UPDATE users SET is_admin = false WHERE id = $1`, f.userID); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSetupFixture(ctx, t, true)
			mut(f)
			code, rep, stdout, _ := f.run(ctx, f.env(f.pool), "tok", "--repos", f.appPath())
			if code != 2 || rep.Status != setupcmd.ReportRejected {
				t.Fatalf("exit = %d, want 2\n%s", code, stdout)
			}
			if n := f.count(ctx, t, `SELECT count(*) FROM forge_connections WHERE user_id = $1`, f.userID); n != 0 {
				t.Fatalf("a refused run wrote %d connections", n)
			}
		})
	}
}

// setup never migrates: a database behind the binary's head is refused with
// exit 2 and nothing written. It uses its own throwaway database so the shared
// one keeps its head.
func TestSetupCmdRefusesASchemaBehindHeadLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSetupFixture(ctx, t, true)
	applied, head, atHead, err := store.SchemaVersionStatus(ctx, f.pool)
	if err != nil || !atHead {
		t.Fatalf("shared database must be at head: applied=%d head=%d err=%v", applied, head, err)
	}

	u, err := url.Parse(os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil || u.Scheme == "" {
		t.Skip("UZI_TEST_DATABASE_URL is not a URL; cannot derive a sibling database")
	}
	name := "setup_behind_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	if _, err := f.pool.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		t.Skipf("cannot create a sibling database: %v", err)
	}
	t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`) })
	u.Path = "/" + name
	behindDSN := u.String()
	if err := store.MigrateTo(ctx, behindDSN, head-1); err != nil {
		t.Fatalf("migrate sibling to head-1: %v", err)
	}
	behind, err := store.OpenPool(ctx, behindDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(behind.Close)
	if _, _, atHead, _ := store.SchemaVersionStatus(ctx, behind); atHead {
		t.Fatal("the sibling database must be behind head")
	}

	code, rep, stdout, _ := f.run(ctx, f.env(behind), "tok", "--repos", f.appPath())
	if code != 2 || !strings.Contains(rep.Error, "schema") {
		t.Fatalf("exit = %d, want 2 with a schema reason\n%s", code, stdout)
	}
}

// The create-only connection write: concurrent and later invocations with
// different tokens never rotate the stored ciphertext or the identity.
func TestSetupCmdConcurrentConnectsNeverRotateLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSetupFixture(ctx, t, true)
	env := f.env(f.pool)

	round := func(n int) (connected int) {
		var (
			wg    sync.WaitGroup
			mu    sync.Mutex
			start = make(chan struct{})
		)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				code, rep, stdout, _ := f.run(ctx, env, fmt.Sprintf("tok-%d-%s", i, uuid.NewString()))
				if code != 0 {
					t.Errorf("exit = %d\n%s", code, stdout)
					return
				}
				mu.Lock()
				if rep.Forge.Status == setupcmd.StatusConnected {
					connected++
				}
				mu.Unlock()
			}(i)
		}
		close(start)
		wg.Wait()
		return connected
	}
	readConn := func() store.ForgeConnection {
		conns, err := f.q.ListForgeConnectionsByUser(ctx, f.userID)
		if err != nil || len(conns) != 1 {
			t.Fatalf("connections = %d err = %v, want exactly one", len(conns), err)
		}
		return conns[0]
	}

	if got := round(8); got != 1 {
		t.Fatalf("%d invocations reported connected, want exactly 1 winner", got)
	}
	winner := readConn()

	round(4)
	after := readConn()
	if after.ID != winner.ID || !bytes.Equal(after.TokenCiphertext, winner.TokenCiphertext) ||
		after.BotUsername != winner.BotUsername || !after.LastVerifiedAt.Time.Equal(winner.LastVerifiedAt.Time) {
		t.Fatal("a later setup run rotated the stored connection")
	}
}

// PRD #3760 M3, acceptance 1 and 3: the worker is created once, the join token
// appears once and only on stdout, its hash is what is stored, and the re-run
// exits 3 with no token and no second worker.
func TestSetupCmdWorkerCreateThenExistsLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSetupFixture(ctx, t, true)
	repos := f.appPath() + "," + f.libPath()
	token := "tok-" + uuid.NewString()

	code, rep, stdout, stderr := f.run(ctx, f.env(f.pool), token, "--repos", repos, "--worker-name", "vps-1")
	if code != 0 || rep.Worker == nil || rep.Worker.Status != setupcmd.StatusCreated {
		t.Fatalf("exit = %d, want 0 with a created worker\n%s", code, stdout)
	}
	join := rep.Worker.JoinToken
	if join == "" {
		t.Fatal("no join token in the report")
	}
	if n := strings.Count(stdout, join); n != 1 {
		t.Fatalf("join token appears %d times on stdout, want 1", n)
	}
	if strings.Contains(stderr, join) || strings.Contains(stdout+stderr, token) {
		t.Fatal("a token reached the wrong stream")
	}
	var stored []byte
	if err := f.pool.QueryRow(ctx, `SELECT token_hash FROM workers WHERE user_id = $1`, f.userID).Scan(&stored); err != nil {
		t.Fatalf("worker row: %v", err)
	}
	if !jointoken.Equal(stored, jointoken.Hash(join)) {
		t.Fatal("the stored hash is not the hash of the printed token")
	}

	code, rep, stdout, stderr = f.run(ctx, f.env(f.pool), token, "--repos", repos, "--worker-name", "vps-1")
	if code != 3 || rep.Worker.Status != setupcmd.StatusExists || rep.Worker.JoinToken != "" {
		t.Fatalf("re-run exit = %d, want 3 with an existing worker and no token\n%s", code, stdout)
	}
	if strings.Contains(stdout+stderr, join) {
		t.Fatal("the re-run printed the first run's token")
	}
	var after []byte
	if err := f.pool.QueryRow(ctx, `SELECT token_hash FROM workers WHERE user_id = $1`, f.userID).Scan(&after); err != nil || !bytes.Equal(after, stored) {
		t.Fatalf("the re-run changed the worker token (err=%v)", err)
	}
	if n := f.count(ctx, t, `SELECT count(*) FROM workers WHERE user_id = $1`, f.userID); n != 1 {
		t.Fatalf("workers = %d, want 1", n)
	}
}

// Concurrent invocations must not mint two first workers: exactly one run
// creates, every other exits 3, and exactly one join token exists across all
// the outputs.
func TestSetupCmdConcurrentWorkerCreateMakesOneLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSetupFixture(ctx, t, true)
	env := f.env(f.pool)

	const n = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		start   = make(chan struct{})
		created int
		exists  int
		joins   []string
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			code, rep, stdout, _ := f.run(ctx, env, fmt.Sprintf("tok-%d-%s", i, uuid.NewString()), "--worker-name", "vps-1")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case code == 0 && rep.Worker != nil && rep.Worker.Status == setupcmd.StatusCreated:
				created++
				joins = append(joins, rep.Worker.JoinToken)
			case code == 3 && rep.Worker != nil && rep.Worker.Status == setupcmd.StatusExists && rep.Worker.JoinToken == "":
				exists++
			default:
				t.Errorf("unexpected outcome: exit %d\n%s", code, stdout)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if created != 1 || exists != n-1 {
		t.Fatalf("created=%d exists=%d, want 1 and %d", created, exists, n-1)
	}
	if got := f.count(ctx, t, `SELECT count(*) FROM workers WHERE user_id = $1`, f.userID); got != 1 {
		t.Fatalf("workers = %d, want exactly 1", got)
	}
	var stored []byte
	if err := f.pool.QueryRow(ctx, `SELECT token_hash FROM workers WHERE user_id = $1`, f.userID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(joins) != 1 || !jointoken.Equal(stored, jointoken.Hash(joins[0])) {
		t.Fatal("the one printed token does not match the stored worker")
	}
}

// A worker request behind a refused repo mints nothing: no worker row exists.
func TestSetupCmdRefusedRepoMintsNoWorkerLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newSetupFixture(ctx, t, true)
	f.forge.protection[f.libID] = pushableProtection
	code, rep, stdout, _ := f.run(ctx, f.env(f.pool), "tok-"+uuid.NewString(),
		"--repos", f.appPath()+","+f.libPath(), "--worker-name", "vps-1")
	if code != 1 || rep.Worker.Status != setupcmd.StatusSkippedDependency {
		t.Fatalf("exit = %d, want 1 with a skipped worker\n%s", code, stdout)
	}
	if got := f.count(ctx, t, `SELECT count(*) FROM workers WHERE user_id = $1`, f.userID); got != 0 {
		t.Fatalf("workers = %d, want 0", got)
	}
}
