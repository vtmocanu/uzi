package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/config"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/handler"
	"github.com/vtmocanu/uzi/api/internal/hub"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Instrument below sqlc, preserving *store.Queries and the real transactional
// createRunResolved path. The loser pauses AFTER scanning a genuine false
// precheck; an independent connection commits the interposed creator.
type handoffRaceDB struct {
	*pgxpool.Conn
	ready, release chan struct{}
	ctx            context.Context
	raw            chan error
}
type handoffRaceRow struct {
	pgx.Row
	after func(error) error
}

func (r handoffRaceRow) Scan(dest ...any) error { return r.after(r.Row.Scan(dest...)) }
func (d *handoffRaceDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	row := d.Conn.QueryRow(ctx, sql, args...)
	if !strings.HasPrefix(sql, "-- name: HasActiveRunForIssue ") {
		return row
	}
	return handoffRaceRow{row, func(err error) error {
		if err != nil {
			return err
		}
		close(d.ready)
		select {
		case <-d.release:
			return nil
		case <-d.ctx.Done():
			return d.ctx.Err()
		}
	}}
}

type handoffRaceTx struct {
	pgx.Tx
	raw chan error
}

func (d *handoffRaceDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := d.Conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return handoffRaceTx{tx, d.raw}, nil
}
func (tx handoffRaceTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return handoffRaceRow{tx.Tx.QueryRow(ctx, sql, args...), func(err error) error {
		var pgerr *pgconn.PgError
		if errors.As(err, &pgerr) {
			tx.raw <- err
		}
		return err
	}}
}

type handoffRaceForge struct{ forgetest.BaseFake }

func (handoffRaceForge) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{IID: 42, State: "opened", Title: "candidate", Description: "body", Labels: []string{"uzi"}}, nil
}

type handoffRaceBuilder struct{}

func (handoffRaceBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return &handoffRaceForge{}, nil
}

// Explicitly invoke ./cmd/uzi: the seven-package store-IT harness does NOT
// enumerate this CLI package. No external forge, push or dispatch is performed.
func TestHandoffSharedConflictBridgeLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("requires disposable UZI_TEST_DATABASE_URL and explicit ./cmd/uzi invocation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, winnerKind := range []string{"issue", "task"} {
		for _, surface := range []string{"service", "CLI", "issueCLI"} {
			if surface == "issueCLI" && winnerKind != "task" {
				continue
			}
			t.Run(winnerKind+"/"+surface, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
				defer cancel()
				user, conn, repo := uuid.New(), uuid.New(), uuid.New()
				exec := func(sql string, args ...any) {
					t.Helper()
					if _, err := pool.Exec(ctx, sql, args...); err != nil {
						t.Fatal(err)
					}
				}
				exec("INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", user, user.String()+"@m3.test")
				exec(`INSERT INTO user_secrets(id,user_id,kind,label,is_default,ciphertext,sealed_with)
     VALUES($1,$2,'anthropic_token',$3,true,$4,'master')`, uuid.New(), user, uuid.NewString(), []byte("fixture"))
				exec(`INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
     VALUES($1,$2,'github','https://github.com','bot',1,$3)`, conn, user, []byte("fixture"))
				exec(`INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
     VALUES($1,$2,42,'acme/widgets','https://github.com/acme/widgets','main',true)`, repo, conn)
				exec(`INSERT INTO issues(repo_id,forge_issue_iid,title,state,labels,web_url,has_prd_link,forge_updated_at,synced_at)
     VALUES($1,42,'candidate','opened','["uzi"]','https://github.com/acme/widgets/issues/42',false,now(),now())`, repo)
				c1, err := pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer c1.Release()
				c2, err := pool.Acquire(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer c2.Release()
				if c1.Conn().PgConn().PID() == c2.Conn().PgConn().PID() {
					t.Fatal("contenders share a DB connection")
				}
				db := &handoffRaceDB{Conn: c1, ready: make(chan struct{}), release: make(chan struct{}), ctx: ctx, raw: make(chan error, 4)}
				box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
				if err != nil {
					t.Fatal(err)
				}
				loser := workersvc.New(store.New(db), box, workersvc.Params{})
				loser.SetTxBeginner(db)
				loser.SetForges(handoffRaceBuilder{})
				winner := workersvc.New(store.New(c2), box, workersvc.Params{})
				winner.SetTxBeginner(c2)
				winner.SetForges(handoffRaceBuilder{})
				type outcome struct {
					run store.Run
					err error
				}
				committed := make(chan outcome, 1)
				go func() {
					defer close(db.release)
					select {
					case <-db.ready:
					case <-ctx.Done():
						committed <- outcome{err: ctx.Err()}
						return
					}
					var run store.Run
					var err error
					iid := int64(42)
					if winnerKind == "issue" {
						run, err = winner.CreateRun(ctx, user, repo, iid, "body", nil, nil, false, nil, nil, nil)
					} else {
						run, err = winner.CreateTaskRun(ctx, user, repo, "winner", "main", false, false, false, false, &iid)
					}
					committed <- outcome{run, err}
				}()
				joined := false
				defer func() {
					cancel()
					if !joined {
						<-committed
					}
				}()
				iid := int64(42)
				var serviceErr error
				var code int
				var stderr string
				var requests string
				if surface == "service" {
					_, serviceErr = loser.CreateTaskRun(ctx, user, repo, "loser", "main", false, false, false, false, &iid)
				} else {
					token, hash, prefix, err := clitoken.Generate(clitoken.ScopeUser)
					if err != nil {
						t.Fatal(err)
					}
					exec(`INSERT INTO cli_tokens(user_id,name,token_hash,token_prefix,scope)
      VALUES($1,'m3 bridge',$2,$3,$4)`, user, hash, prefix, clitoken.ScopeUser)
					q := store.New(pool)
					h := handler.New(pool, q, config.Config{JWTSecret: []byte("0123456789abcdef0123456789abcdef"), AuthTokenTTL: time.Hour}, box, nil, loser, nil, hub.New(), settings.New(q, time.Minute))
					lim := mw.NewLimiter(100000, time.Minute, nil)
					router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
					// A private server writes its own role/PID request receipt; assertions below
					// bind the observed 409 to this instrument rather than a responder on a port.
					log, err := os.CreateTemp(t.TempDir(), "m3-cli-requests-*.log")
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = log.Close() }()
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						rec := httptest.NewRecorder()
						router.ServeHTTP(rec, r)
						_, _ = fmt.Fprintf(log, "m3-cli pid=%d %s %s status=%d\n", os.Getpid(), r.Method, r.URL.Path, rec.Code)
						for k, values := range rec.Header() {
							for _, v := range values {
								w.Header().Add(k, v)
							}
						}
						w.WriteHeader(rec.Code)
						_, _ = w.Write(rec.Body.Bytes())
					}))
					client := &uzicli.HTTPClient{BaseURL: srv.URL, Token: token, HTTP: srv.Client()}
					rec := &handoffRecorder{}
					env, _ := handoffEnv(&uzicli.FakeClient{}, rec)
					env.NewClient = func(uzicli.Settings) uzicli.Client { return client }
					if surface == "issueCLI" {
						_, stderr, code = runCLI(t, env, "run", "create", "--repo", repo.String(), "--issue", "42")
					} else {
						_, stderr, code = runCLI(t, env, "handoff", "-m", "loser", "--issue", "42")
					}
					srv.Close()
					data, err := os.ReadFile(log.Name())
					if err != nil {
						t.Fatal(err)
					}
					requests = string(data)
					for _, call := range rec.gitCalls {
						if len(call) > 0 && call[0] == "push" {
							t.Errorf("refusal pushed seed: %v", call)
						}
					}
				}
				result := <-committed // join every contender before asserting or releasing conns
				joined = true
				if result.err != nil {
					t.Fatalf("interposed creator: %v", result.err)
				}
				if surface == "service" {
					if !errors.Is(serviceErr, workersvc.ErrActiveRunExists) {
						t.Fatalf("service error=%v want ErrActiveRunExists", serviceErr)
					}
				} else {
					endpoint := "task-runs"
					if surface == "issueCLI" {
						endpoint = "runs"
					}
					want := fmt.Sprintf("m3-cli pid=%d POST /api/repos/%s/%s status=409", os.Getpid(), repo, endpoint)
					if uzicli.ExitConflict != 5 {
						t.Fatal("ExitConflict must remain 5")
					}
					if code != uzicli.ExitConflict || !strings.Contains(requests, want) {
						t.Fatalf("CLI exit=%d stderr=%s requests=%s", code, stderr, requests)
					}
					if strings.Contains(requests, "/dispatch") {
						t.Fatalf("refusal dispatched: %s", requests)
					}
				}
				select {
				case err := <-db.raw:
					var pgerr *pgconn.PgError
					if !errors.As(err, &pgerr) || pgerr.Code != "23505" || pgerr.ConstraintName != "uq_runs_one_active_per_issue" {
						t.Fatalf("raw conflict=%v", err)
					}
					t.Logf("real SQLSTATE=%s index=%s; winner=%s", pgerr.Code, pgerr.ConstraintName, result.run.ID)
				default:
					t.Fatal("no real PostgreSQL conflict observed")
				}
				var count, dispatched int
				if err := pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE kind='task' AND dispatched_at IS NOT NULL)
     FROM runs WHERE repo_id=$1`, repo).Scan(&count, &dispatched); err != nil {
					t.Fatal(err)
				}
				if count != 1 || dispatched != 0 {
					t.Fatalf("committed runs=%d dispatched tasks=%d", count, dispatched)
				}
				var stored uuid.UUID
				if err := pool.QueryRow(ctx, "SELECT id FROM runs WHERE repo_id=$1", repo).Scan(&stored); err != nil || stored != result.run.ID {
					t.Fatalf("committed winner=%s want=%s error=%v", stored, result.run.ID, err)
				}
			})
		}
	}
}
