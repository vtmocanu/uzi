package poller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ci_autofix_override_livedb_test.go pins issue #4617 end to end through the poller detector
// against a REAL Postgres and the REAL workersvc create: the in-transaction recheck refuses a
// ci_fix whose branch stopped being eligible between detection and creation, and the detector
// then leaves the ledger untouched. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres; run via ./e2e/run-store-it.sh.

const (
	ovMrIID       int64 = 4617001
	ovSeedPipe    int64 = 1111
	ovCandPipe    int64 = 9001
	ovSeedSig           = "seed-sig"
	ovSeedAttempt int32 = 1
)

// ovForge is the detector's code host / issue tracker (cfForge) plus the forge.Forge surface
// the workersvc PR binding reads: every recorded PR answers opened on the fixture's branch.
type ovForge struct {
	*cfForge
	ref string
}

func (f ovForge) GetMergeRequestSummary(_ context.Context, _ int64, iid int64) (forge.MergeRequestSummary, error) {
	return forge.MergeRequestSummary{IID: iid, State: forge.MRStateOpened, SourceBranch: f.ref}, nil
}

type ovBuilder struct{ f forge.Forge }

func (b ovBuilder) ForgeForConnection(string, string, []byte) (forge.Forge, error) { return b.f, nil }

// ovStarter runs a pre-hook, then delegates to the real service and records its error.
type ovStarter struct {
	svc   *workersvc.Service
	pre   func()
	calls int
	err   error
}

func (s *ovStarter) CreateAutoCIFixRun(ctx context.Context, userID, repoID uuid.UUID, ref, title, desc string, snap workersvc.FailureSnapshot, paths []string) (store.Run, error) {
	s.calls++
	if s.pre != nil {
		s.pre()
	}
	run, err := s.svc.CreateAutoCIFixRun(ctx, userID, repoID, ref, title, desc, snap, paths)
	s.err = err
	return run, err
}

type ovFix struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	q       *store.Queries
	owner   uuid.UUID
	repo    uuid.UUID
	ref     string
	source  uuid.UUID // the selected run
	forge   *cfForge
	starter *ovStarter
	det     *CIAutoFix
}

func (f *ovFix) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

// newOvFix seeds a user with an Anthropic token, a repo, one source run on ref (kind issue,
// the given status, branch and mr_iid recorded), a failed pipeline, and a prior ledger row
// (count 1, a different last pipeline, signature seed-sig, halt latched), and wires the real
// workersvc create behind a starter wrapper.
func newOvFix(t *testing.T, pool *pgxpool.Pool, iid int64, sourceStatus string, adminEnabled bool) *ovFix {
	t.Helper()
	ctx := context.Background()
	f := &ovFix{t: t, ctx: ctx, pool: pool, q: store.New(pool), owner: uuid.New(), repo: uuid.New(), source: uuid.New()}
	f.ref = fmt.Sprintf("agent/issue-%d", iid)
	conn := uuid.New()
	f.exec("INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", f.owner, fmt.Sprintf("%s@4617.test", f.owner))
	f.exec("INSERT INTO user_secrets (user_id,kind,label,is_default,ciphertext,sealed_with) VALUES ($1,'anthropic_token','default',true,$2,'master')", f.owner, []byte{2})
	f.exec("INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES ($1,$2,'gitlab','https://forge.test','bot',1,$3)", conn, f.owner, []byte{1})
	f.exec("INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled) VALUES ($1,$2,42,$3,'https://forge.test/g/r','main',true)", f.repo, conn, f.repo.String())
	f.exec(`INSERT INTO runs (id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,branch,mr_iid,mr_state,status,created_at)
	        VALUES ($1,$2,$3,'issue',$4,'t','d',$5,$6,'opened',$7,now() - interval '1 hour')`, f.source, f.owner, f.repo, iid, f.ref, ovMrIID, sourceStatus)
	f.exec("INSERT INTO pipeline_statuses (repo_id,ref,pipeline_id,sha,status,web_url,synced_at) VALUES ($1,$2,$3,'deadbeef','failed','https://forge.test/p',now())", f.repo, f.ref, ovCandPipe)
	f.exec("INSERT INTO ci_autofix_attempts (repo_id,ref,attempt_count,last_signature,last_pipeline_id,halt_notified) VALUES ($1,$2,$3,$4,$5,true)", f.repo, f.ref, ovSeedAttempt, ovSeedSig, ovSeedPipe)

	f.forge = &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}
	svc := workersvc.New(f.q, nil, workersvc.Params{})
	svc.SetTxBeginner(pool)
	svc.SetForges(ovBuilder{ovForge{cfForge: f.forge, ref: f.ref}})
	f.starter = &ovStarter{svc: svc}
	f.det = NewCIAutoFix(f.q, f.starter, nil, cfSettings{enabled: adminEnabled}, 5, 4096, 2, []string{".gitlab-ci.yml"})
	return f
}

func (f *ovFix) detect() {
	f.t.Helper()
	row := store.ListEnabledReposWithConnectionsRow{ID: f.repo, ForgeProjectID: 42, PathWithNamespace: f.repo.String()}
	f.det.detect(f.ctx, row, f.forge, f.forge)
}

func (f *ovFix) ciFixRows() (n int, enabled pgtype.Bool) {
	f.t.Helper()
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runs WHERE repo_id = $1 AND kind = 'ci_fix'`, f.repo).Scan(&n); err != nil {
		f.t.Fatalf("count ci_fix: %v", err)
	}
	if n > 0 {
		if err := f.pool.QueryRow(f.ctx, `SELECT ci_autofix_enabled FROM runs WHERE repo_id = $1 AND kind = 'ci_fix' ORDER BY created_at DESC LIMIT 1`, f.repo).Scan(&enabled); err != nil {
			f.t.Fatalf("read ci_fix override: %v", err)
		}
	}
	return n, enabled
}

func (f *ovFix) assertLedgerUnchanged() {
	f.t.Helper()
	got, err := f.q.GetCIAutofixAttempt(f.ctx, store.GetCIAutofixAttemptParams{RepoID: f.repo, Ref: f.ref})
	if err != nil {
		f.t.Fatalf("read ledger: %v", err)
	}
	if got.AttemptCount != ovSeedAttempt || !got.LastSignature.Valid || got.LastSignature.String != ovSeedSig ||
		!got.LastPipelineID.Valid || got.LastPipelineID.Int64 != ovSeedPipe || !got.HaltNotified {
		f.t.Fatalf("ledger changed: count=%d sig=%+v pipeline=%+v halt=%v, want count=%d sig=%s pipeline=%d halt=true",
			got.AttemptCount, got.LastSignature, got.LastPipelineID, got.HaltNotified, ovSeedAttempt, ovSeedSig, ovSeedPipe)
	}
}

func (f *ovFix) assertNoRun() {
	f.t.Helper()
	if n, _ := f.ciFixRows(); n != 0 {
		f.t.Fatalf("ci_fix runs = %d, want 0", n)
	}
	if len(f.forge.notes) != 0 {
		f.t.Fatalf("issue comments = %d, want none", len(f.forge.notes))
	}
}

func ovPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set a throwaway database: UZI_TEST_DATABASE_URL=<throwaway DSN> go test -buildvcs=false -race -count=1 -p 1 -v -run 'TestCIAutofixOverride.*LiveDB$' ./internal/poller")
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
	return pool
}

// (a) An owner opts the selected run out after the detector listed the branch but before the
// create transaction reads it: the create refuses, the ledger is untouched, and the next tick's
// listing no longer returns the branch.
func TestCIAutofixOverrideOptOutRaceLiveDB(t *testing.T) {
	f := newOvFix(t, ovPool(t), 46181, "completed", true)
	f.starter.pre = func() {
		n, err := f.q.SetRunCiAutofixEnabled(f.ctx, store.SetRunCiAutofixEnabledParams{
			CiAutofixEnabled: pgtype.Bool{Bool: false, Valid: true}, ID: f.source, UserID: f.owner,
		})
		if err != nil || n != 1 {
			t.Fatalf("opt-out rows = %d, %v; want 1", n, err)
		}
	}
	f.detect()
	if f.starter.calls != 1 {
		t.Fatalf("starter calls = %d, want 1", f.starter.calls)
	}
	if !errors.Is(f.starter.err, workersvc.ErrCIAutofixIneligible) {
		t.Fatalf("starter err = %v, want ErrCIAutofixIneligible", f.starter.err)
	}
	f.assertNoRun()
	f.assertLedgerUnchanged()

	f.detect()
	if f.starter.calls != 1 {
		t.Fatalf("second tick called the starter again (%d calls): the opted-out branch must not be listed", f.starter.calls)
	}
}

// (b) The only qualifying run is cancelled after detection: with no selected run left the
// create refuses and the ledger is untouched.
func TestCIAutofixOverrideCancelledAfterDetectionLiveDB(t *testing.T) {
	f := newOvFix(t, ovPool(t), 46182, "paused", true)
	f.starter.pre = func() {
		active, err := f.q.CountActiveRunsWithBranch(f.ctx, store.CountActiveRunsWithBranchParams{
			RepoID: f.repo, Branch: pgtype.Text{String: f.ref, Valid: true},
		})
		if err != nil || active != 1 {
			t.Fatalf("active runs on the branch = %d, %v; want 1 before the cancel", active, err)
		}
		n, err := f.q.CancelRunServerSide(f.ctx, store.CancelRunServerSideParams{ID: f.source, UserID: f.owner})
		if err != nil || n != 1 {
			t.Fatalf("cancel rows = %d, %v; want 1", n, err)
		}
	}
	f.detect()
	if f.starter.calls != 1 {
		t.Fatalf("starter calls = %d, want 1", f.starter.calls)
	}
	if !errors.Is(f.starter.err, workersvc.ErrCIAutofixIneligible) {
		t.Fatalf("starter err = %v, want ErrCIAutofixIneligible", f.starter.err)
	}
	f.assertNoRun()
	f.assertLedgerUnchanged()
}

// (c) The admin switch is a hard gate no run override can lift: with the run override true and
// the owner default false, admin ON starts exactly one fix carrying true, admin OFF never reaches
// the starter.
func TestCIAutofixOverrideAdminGateLiveDB(t *testing.T) {
	pool := ovPool(t)
	setup := func(iid int64, admin bool) *ovFix {
		f := newOvFix(t, pool, iid, "completed", admin)
		f.exec(`UPDATE users SET ci_autofix_enabled = false WHERE id = $1`, f.owner)
		f.exec(`UPDATE runs SET ci_autofix_enabled = true WHERE id = $1`, f.source)
		return f
	}

	t.Run("admin on", func(t *testing.T) {
		f := setup(46183, true)
		f.detect()
		if f.starter.calls != 1 || f.starter.err != nil {
			t.Fatalf("starter calls=%d err=%v, want one successful create", f.starter.calls, f.starter.err)
		}
		n, enabled := f.ciFixRows()
		if n != 1 || !enabled.Valid || !enabled.Bool {
			t.Fatalf("ci_fix rows = %d override = %+v, want exactly one carrying true", n, enabled)
		}
		got, err := f.q.GetCIAutofixAttempt(f.ctx, store.GetCIAutofixAttemptParams{RepoID: f.repo, Ref: f.ref})
		if err != nil || got.AttemptCount != ovSeedAttempt+1 || got.LastPipelineID.Int64 != ovCandPipe {
			t.Fatalf("ledger = %+v, %v; want attempt_count %d and last pipeline %d", got, err, ovSeedAttempt+1, ovCandPipe)
		}
	})
	t.Run("admin off", func(t *testing.T) {
		f := setup(46184, false)
		f.detect()
		if f.starter.calls != 0 {
			t.Fatalf("starter calls = %d, want 0 with the admin switch off", f.starter.calls)
		}
		f.assertNoRun()
		f.assertLedgerUnchanged()
	})
}
