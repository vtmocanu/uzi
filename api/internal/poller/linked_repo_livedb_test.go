package poller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/healthsvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4173 M4 through the poller: a repo linked to an issue tracker still runs every
// code-host step when the issue side is unavailable, never runs autopilot, and records
// an issue run's MR state through the board-free lane with no cached card.

// trustFake lets the real YouTrack driver (which takes no TLS option by design) trust
// the youtracktest certificate for this test only.
func trustFake(t *testing.T, srv *youtracktest.Server) {
	t.Helper()
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })
}

type refusingDrivers struct{ calls int }

func (r *refusingDrivers) DriverFor(context.Context, uuid.UUID, uuid.UUID, issuetracker.Binding, issuetracker.StateMapping) (issuetracker.Driver, error) {
	r.calls++
	return nil, errors.New("base URL is not in the allowed issue tracker list")
}

type oneRepoStore struct {
	q    *store.Queries
	repo uuid.UUID
}

func (r oneRepoStore) ListEnabledReposWithConnections(ctx context.Context) ([]store.ListEnabledReposWithConnectionsRow, error) {
	rows, err := r.q.ListEnabledReposWithConnections(ctx)
	var out []store.ListEnabledReposWithConnectionsRow
	for _, row := range rows {
		if row.ID == r.repo {
			out = append(out, row)
		}
	}
	return out, err
}

type linkedEnv struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	q       *store.Queries
	svc     *forgesvc.Service
	rec     *forgetest.Recorder
	forge   *recordedPollForge
	engine  *Engine
	starter *apRuns
	user    uuid.UUID
	repo    uuid.UUID
	run7    uuid.UUID
}

// newLinkedEnv seeds one enabled repo linked to a real issuetrackersvc connection to
// srv, with an autopilot-labelled card cached (issue 5) and a completed issue run
// (issue 7, MR 13 recorded opened, forge reports it closed) with NO cached card.
func newLinkedEnv(t *testing.T, srv *youtracktest.Server, drivers forgesvc.IssueTrackerDrivers) *linkedEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run e2e/run-store-it.sh")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	fake := &recordedPollForge{}
	rec := forgetest.NewRecorder(fake)
	svc := forgesvc.NewWithForgeBuilder(q, box, time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) {
		return rec, nil
	})
	sealed, err := svc.EncryptToken("fixture")
	if err != nil {
		t.Fatal(err)
	}
	env := &linkedEnv{ctx: ctx, pool: pool, q: q, svc: svc, rec: rec, forge: fake, user: uuid.New(), repo: uuid.New(), run7: uuid.New()}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("INSERT INTO users(id,email,password_hash,autopilot_enabled) VALUES($1,$2,'x',true)", env.user, fmt.Sprintf("%s@linked.test", env.user))
	conn := uuid.New()
	exec("INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,human_username,token_ciphertext) VALUES($1,$2,'gitlab',$3,'bot',1,'alice',$4)", conn, env.user, "https://"+uuid.NewString()+".forge.test", sealed)
	exec("INSERT INTO user_secrets(user_id,kind,label,is_default,ciphertext,sealed_with) VALUES($1,'anthropic_token','default',true,$2,'master')", env.user, []byte{2})

	itConn := uuid.New()
	if srv != nil {
		itSvc := issuetrackersvc.New(q, box, func(u string) bool { return u == srv.URL }, issuetracker.New)
		c, err := itSvc.Connect(ctx, env.user, "youtrack", srv.URL, srv.Token)
		if err != nil {
			t.Fatal(err)
		}
		itConn = c.ID
		if drivers == nil {
			drivers = itSvc
		}
	} else {
		exec(`INSERT INTO issue_tracker_connections (id, user_id, kind, base_url, account_id, account_login, token_ciphertext)
		      VALUES ($1, $2, 'youtrack', $3, '1-1', 'bot', $4)`, itConn, env.user, "https://"+uuid.NewString()+".yt.test", []byte{1})
	}
	svc.SetIssueTrackerDrivers(drivers)
	exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled,
	        issue_tracker_connection_id, issue_tracker_project_id, issue_tracker_project_short, eligibility_tag_id, eligibility_tag_name, issue_tracker_linked_at)
	      VALUES($1,$2,42,$3,'https://forge.test/g/r','main',true,$4,$5,$6,$7,$8,now())`,
		env.repo, conn, env.repo.String(), itConn, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)
	exec("INSERT INTO board_columns(repo_id,label_name,position) VALUES($1,'In Progress',0),($1,'Human Review',1)", env.repo)
	// A cached card carrying the autopilot label: an unlinked repo would run the detector on it.
	exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, 5, 'autopilot', 'opened', '["uzi","autopilot"]', 'https://forge.test/i/5', now(), now())`, env.repo)
	exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,branch,mr_iid,mr_state,status,created_at) VALUES($1,$2,$3,'issue',7,'t','d',$4,13,'opened','completed','2020-01-01')",
		env.run7, env.user, env.repo, "agent/issue-7-"+env.repo.String())

	// A merged run that declared a moved PRD: SyncPRDLinkPatches has a candidate and
	// reads the issue side for it, which would panic on a nil tracker.
	exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,branch,mr_iid,mr_state,prd_done_path,status,created_at) VALUES($1,$2,$3,'issue',8,'t','Implements prds/72-x.md end to end.',$4,31,'merged','prds/done/72-x.md','completed','2020-01-01')",
		uuid.New(), env.user, env.repo, "agent/issue-8-"+env.repo.String())

	env.engine = New(svc, oneRepoStore{q, env.repo}, time.Minute, 3)
	env.engine.SetPipelineWatch(time.Hour, 5)
	env.starter = &apRuns{}
	env.engine.SetAutopilot(NewAutopilot(q, env.starter, nil))
	return env
}

// poll runs one tick with the default logger captured and fails if the repo's sync
// panicked (guardPanic would then silently skip every later code-host step).
func (e *linkedEnv) poll(t *testing.T) {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	e.engine.PollOnce(e.ctx)
	if strings.Contains(logs.String(), "repo sync panicked") {
		t.Fatalf("the repo sync panicked: %s", logs.String())
	}
}

func (e *linkedEnv) assertCodeHostOnly(t *testing.T) {
	t.Helper()
	var state *string
	if err := e.pool.QueryRow(e.ctx, `SELECT mr_state FROM runs WHERE id = $1`, e.run7).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == nil || *state != forge.MRStateClosed {
		t.Fatalf("issue run's mr_state = %v, want closed: the board-free lane must record a linked repo's issue run with no cached card", state)
	}
	calls := e.rec.Calls()
	if !containsPrefix(calls, "GetMergeRequest(") {
		t.Fatalf("the code-host steps did not run: %v", calls)
	}
	// The last code-host step of the pipeline watch ran: a panic in an earlier issue-side
	// step would have skipped it.
	if !containsPrefix(calls, "LatestPipeline(") {
		t.Fatalf("SyncPipelines did not run after the issue-side steps: %v", calls)
	}
	for _, c := range calls {
		for _, issueSide := range []string{"ListIssues(", "GetIssue(", "ListIssueLabelEvents(", "EnsureLabels(", "UpdateIssueLabels(", "ListLabels("} {
			if strings.HasPrefix(c, issueSide) {
				t.Errorf("the forge saw an issue-side call %q for a linked repo", c)
			}
		}
	}
	if len(e.starter.calls) != 0 {
		t.Fatalf("autopilot ran on a linked repo: %+v", e.starter.calls)
	}
}

func containsPrefix(ss []string, prefix string) bool {
	for _, s := range ss {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

func TestLinkedRepoCodeHostStepsRunWhenTheTrackerIsUnreachableLiveDB(t *testing.T) {
	srv := youtracktest.NewServer(t)
	trustFake(t, srv)
	env := newLinkedEnv(t, srv, nil)
	srv.Close() // the tracker is gone from here on

	env.poll(t)
	env.assertCodeHostOnly(t)
}

func TestLinkedRepoCodeHostStepsRunWhenTheResolverRefusesLiveDB(t *testing.T) {
	drivers := &refusingDrivers{}
	env := newLinkedEnv(t, nil, drivers)

	env.poll(t)
	if drivers.calls == 0 {
		t.Fatal("the resolver was never asked for the linked repo's issue side")
	}
	env.assertCodeHostOnly(t)
}

// With no driver source wired a linked repo fails closed on its issue side and never
// falls back to the forge, yet its code-host steps still run.
func TestLinkedRepoWithNoDriverSourceNeverFallsBackToTheForgeLiveDB(t *testing.T) {
	env := newLinkedEnv(t, nil, nil)
	env.svc.SetIssueTrackerDrivers(nil)

	env.poll(t)
	env.assertCodeHostOnly(t)
}

type syncOutcome struct {
	ok    bool
	class forge.ErrorClass
}

// observed is what a linked repo's two health observers saw: the raw outcomes, plus the
// real registries healthsvc reads, on a clock the test controls.
type observed struct {
	forge, issueTracker       []syncOutcome
	forgeReg, issueTrackerReg *healthsvc.SyncRegistry
	clock                     time.Time
}

// observe wires the engine's forge.sync and issue_tracker.sync observers to recorders and
// to real healthsvc registries.
func observe(e *linkedEnv) *observed {
	o := &observed{clock: time.Now()}
	now := func() time.Time { return o.clock }
	o.forgeReg, o.issueTrackerReg = healthsvc.NewSyncRegistry(now), healthsvc.NewSyncRegistry(now)
	e.engine.SetSyncAttempt(func(id uuid.UUID) func(bool, forge.ErrorClass) {
		reg := o.forgeReg.Begin(id)
		return func(ok bool, class forge.ErrorClass) {
			o.forge = append(o.forge, syncOutcome{ok, class})
			reg(ok, class)
		}
	})
	e.engine.SetIssueTrackerSyncAttempt(func(id uuid.UUID) func(bool, forge.ErrorClass) {
		reg := o.issueTrackerReg.Begin(id)
		return func(ok bool, class forge.ErrorClass) {
			o.issueTracker = append(o.issueTracker, syncOutcome{ok, class})
			reg(ok, class)
		}
	})
	return o
}

// authClassError is a classified forge error: forge.Class maps it to ErrorClassAuth, as it
// does a forge 401.
type authClassError struct{}

func (authClassError) Error() string           { return "401 Unauthorized" }
func (authClassError) Class() forge.ErrorClass { return forge.ErrorClassAuth }

// onlyLinkedRepo scopes the health checks' enumerations to the one seeded repo, so the
// shared test database's other repos do not leave the checks pending.
type onlyLinkedRepo struct {
	*store.Queries
	repo uuid.UUID
}

func (o onlyLinkedRepo) ListEnabledRepoIDs(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{o.repo}, nil
}

func (o onlyLinkedRepo) ListEnabledLinkedRepoIDs(context.Context) ([]uuid.UUID, error) {
	return []uuid.UUID{o.repo}, nil
}

// health ages the clock past the danger band, then evaluates admin health through
// healthsvc over the two registries the engine fed, returning the two sync checks.
func (o *observed) health(t *testing.T, e *linkedEnv) (forgeCheck, issueTrackerCheck apitypes.HealthCheckDTO) {
	t.Helper()
	o.clock = o.clock.Add(time.Hour)
	svc := healthsvc.New(healthsvc.Config{
		Store:                    onlyLinkedRepo{e.q, e.repo},
		Now:                      func() time.Time { return o.clock },
		ForgeSyncRegistry:        o.forgeReg,
		IssueTrackerSyncRegistry: o.issueTrackerReg,
		ForgeSyncInterval:        time.Minute,
	})
	doc, err := svc.Evaluate(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range doc.Checks {
		switch c.ID {
		case "forge.sync":
			forgeCheck = c
		case "issue_tracker.sync":
			issueTrackerCheck = c
		}
	}
	if forgeCheck.ID == "" || issueTrackerCheck.ID == "" {
		t.Fatalf("health doc lacks forge.sync or issue_tracker.sync: %+v", doc.Checks)
	}
	return forgeCheck, issueTrackerCheck
}

// A linked repo's forge.sync follows a code-host probe and its issue_tracker.sync follows
// the linked issue sync; every code-host step still runs whichever fails.
func TestLinkedRepoHealthSplitsForgeAndIssueTrackerLiveDB(t *testing.T) {
	t.Run("healthy reports ok on both", func(t *testing.T) {
		srv := youtracktest.NewServer(t)
		trustFake(t, srv)
		env := newLinkedEnv(t, srv, nil)
		o := observe(env)
		env.poll(t)
		if len(o.forge) != 1 || !o.forge[0].ok || len(o.issueTracker) != 1 || !o.issueTracker[0].ok {
			t.Fatalf("outcomes = forge %+v, issue tracker %+v, want one healthy report each", o.forge, o.issueTracker)
		}
		if !containsPrefix(env.rec.Calls(), "GetProject(") {
			t.Fatalf("no code-host probe: %v", env.rec.Calls())
		}
	})
	t.Run("a forge 401 turns forge.sync red with the forge action and leaves issue_tracker.sync ok", func(t *testing.T) {
		srv := youtracktest.NewServer(t)
		trustFake(t, srv)
		env := newLinkedEnv(t, srv, nil)
		env.forge.getProjectErr = authClassError{}
		o := observe(env)
		env.poll(t)
		if len(o.forge) != 1 || o.forge[0].ok || o.forge[0].class != forge.ErrorClassAuth {
			t.Fatalf("forge outcomes = %+v, want one auth failure", o.forge)
		}
		if len(o.issueTracker) != 1 || !o.issueTracker[0].ok {
			t.Fatalf("issue tracker outcomes = %+v, want one healthy report", o.issueTracker)
		}
		env.assertCodeHostOnly(t)
		fc, ic := o.health(t, env)
		if fc.Severity != "danger" || fc.Action == nil || !strings.Contains(*fc.Action, "re-check the forge connection") {
			t.Fatalf("forge.sync = %+v, want danger with the forge connection action", fc)
		}
		if ic.Severity != "ok" {
			t.Fatalf("issue_tracker.sync = %+v, want ok", ic)
		}
	})
	t.Run("a rejected issue tracker token fails issue_tracker.sync only", func(t *testing.T) {
		srv := youtracktest.NewServer(t)
		trustFake(t, srv)
		env := newLinkedEnv(t, srv, nil)
		o := observe(env)
		// The stored token no longer matches the one the fake accepts: YouTrack answers 401.
		srv.Token += "-rotated"
		env.poll(t)
		if len(o.forge) != 1 || !o.forge[0].ok {
			t.Fatalf("forge outcomes = %+v, want one healthy report", o.forge)
		}
		if len(o.issueTracker) != 1 || o.issueTracker[0].ok {
			t.Fatalf("issue tracker outcomes = %+v, want one failure", o.issueTracker)
		}
		env.assertCodeHostOnly(t)
		fc, ic := o.health(t, env)
		if fc.Severity != "ok" {
			t.Fatalf("forge.sync = %+v, want ok", fc)
		}
		if ic.Severity != "danger" || ic.Action == nil || strings.Contains(*ic.Action, "forge") {
			t.Fatalf("issue_tracker.sync = %+v, want danger with an issue tracker action", ic)
		}
		if o.issueTracker[0].class != forge.ErrorClassAuth {
			t.Fatalf("issue tracker class = %q, want %q", o.issueTracker[0].class, forge.ErrorClassAuth)
		}
		if !strings.Contains(*ic.Action, "re-check the issue tracker connection") {
			t.Fatalf("issue_tracker.sync action = %q, want the auth action naming the issue tracker connection", *ic.Action)
		}
	})
	t.Run("an unreachable issue tracker fails issue_tracker.sync, code-host steps still run", func(t *testing.T) {
		srv := youtracktest.NewServer(t)
		trustFake(t, srv)
		env := newLinkedEnv(t, srv, nil)
		o := observe(env)
		srv.Close()
		env.poll(t)
		if len(o.forge) != 1 || !o.forge[0].ok || len(o.issueTracker) != 1 || o.issueTracker[0].ok {
			t.Fatalf("outcomes = forge %+v, issue tracker %+v, want forge healthy and issue tracker failing", o.forge, o.issueTracker)
		}
		env.assertCodeHostOnly(t)
	})
	t.Run("a refused resolver fails issue_tracker.sync, code-host steps still run", func(t *testing.T) {
		env := newLinkedEnv(t, nil, &refusingDrivers{})
		o := observe(env)
		env.poll(t)
		if len(o.forge) != 1 || !o.forge[0].ok || len(o.issueTracker) != 1 || o.issueTracker[0].ok {
			t.Fatalf("outcomes = forge %+v, issue tracker %+v, want forge healthy and issue tracker failing", o.forge, o.issueTracker)
		}
		env.assertCodeHostOnly(t)
	})
}

// The poller's linked tick caches the eligibility tag's issues with their keys.
func TestLinkedRepoPollCachesTaggedIssuesLiveDB(t *testing.T) {
	srv := youtracktest.NewServer(t)
	trustFake(t, srv)
	env := newLinkedEnv(t, srv, nil)
	n := srv.SeedIssue("tagged", "body", youtracktest.EligibilityTagID)
	env.poll(t)
	var key *string
	if err := env.pool.QueryRow(env.ctx, `SELECT issue_key FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, env.repo, n).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%s-%d", youtracktest.ProjectShortName, n); key == nil || *key != want {
		t.Fatalf("issue_key = %v, want %s", key, want)
	}
}

// The poller hands a linked repo's issue side to the board-free MR-state recorder: an
// issue run whose MR closed moves its cached Human Review card to In Progress through the
// issue tracker.
func TestLinkedRepoPollMovesTheCardWhenItsMRClosesLiveDB(t *testing.T) {
	srv := youtracktest.NewServer(t)
	trustFake(t, srv)
	srv.SeedState("In Progress", false)
	srv.SeedState("In Review", false)
	env := newLinkedEnv(t, srv, nil)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := env.pool.Exec(env.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO repo_issue_tracker_states (repo_id, open_state, closed_state) VALUES ($1, 'Open', 'Fixed')`, env.repo)
	exec(`INSERT INTO repo_issue_tracker_column_states (repo_id, label_name, state_name) VALUES ($1, 'Human Review', 'In Review'), ($1, 'In Progress', 'In Progress')`, env.repo)

	n := srv.SeedIssue("a card", "body", youtracktest.EligibilityTagID)
	tr, err := env.svc.IssueTrackerForRepo(env.ctx, env.user, env.repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.MoveToColumn(env.ctx, 42, n, forge.ColumnMove{Column: "Human Review"}); err != nil {
		t.Fatal(err)
	}
	run := uuid.New()
	exec("INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,branch,mr_iid,mr_state,status,created_at) VALUES($1,$2,$3,'issue',$4,'t','d',$5,13,'opened','completed','2020-01-01')",
		run, env.user, env.repo, n, "agent/issue-"+run.String())

	env.poll(t)
	if got := srv.IssueState(n); got != "In Progress" {
		t.Fatalf("State = %q, want In Progress after the MR closed", got)
	}
	var column *string
	if err := env.pool.QueryRow(env.ctx, `SELECT issue_tracker_column FROM issues WHERE repo_id = $1 AND forge_issue_iid = $2`, env.repo, n).Scan(&column); err != nil {
		t.Fatal(err)
	}
	if column == nil || *column != "In Progress" {
		t.Fatalf("cached column = %v, want In Progress", column)
	}
	var state *string
	if err := env.pool.QueryRow(env.ctx, `SELECT mr_state FROM runs WHERE id = $1`, run).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state == nil || *state != forge.MRStateClosed {
		t.Fatalf("mr_state = %v, want closed", state)
	}
}
