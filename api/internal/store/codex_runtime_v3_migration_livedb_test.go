package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const runtimeV3Function = "public.fn_placement_admitting_workers(public.runs,text,timestamptz,timestamptz,interval,boolean,text[])"

// Require the runner's marker BEFORE creating any child database.
func runtimeV3MigrationDB(t *testing.T) (*pgxpool.Pool, string, int64, int64) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run through owned store-IT runner")
	}
	parent := cjOpenPool(t, dsn)
	var marker string
	if err := parent.QueryRow(context.Background(), "SELECT marker FROM uzi_test_harness.database_marker WHERE marker='recovery-reset-v1'").Scan(&marker); err != nil {
		t.Fatalf("owned runner database marker required: %v", err)
	}
	v := migrationVersionByName(t, "codex_runtime_v3")
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var predecessor int64
	for _, entry := range entries {
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if n < v && n > predecessor {
			predecessor = n
		}
	}
	if predecessor == 0 {
		t.Fatal("no predecessor migration")
	}
	pool, childDSN := codexJudgeMigrationDB(t, predecessor)
	return pool, childDSN, predecessor, v
}

func runtimeV3Definition(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var definition string
	if err := pool.QueryRow(context.Background(), "SELECT pg_get_functiondef($1::regprocedure)", runtimeV3Function).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	return definition
}

// Snapshot only row values: sequence advancement is intentionally not transactional.
func runtimeV3Metadata(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var rows string
	if err := pool.QueryRow(context.Background(), "SELECT coalesce(jsonb_agg(to_jsonb(v) ORDER BY id)::text,'[]') FROM goose_db_version v").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

func runtimeV3AssertVersion(t *testing.T, pool *pgxpool.Pool, want int64) {
	t.Helper()
	var got int64
	if err := pool.QueryRow(context.Background(), "SELECT coalesce(max(version_id),0) FROM goose_db_version WHERE is_applied").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Goose version=%d want=%d", got, want)
	}
}

func runtimeV3AssertProviderClauses(t *testing.T, definition string) {
	t.Helper()
	for _, clause := range []string{
		"AND (r.model_provider_key_mode IS NULL",
		"OR (r.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities))",
		"OR (r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities)))",
	} {
		if !strings.Contains(definition, clause) {
			t.Fatalf("placement definition lost engine-aware provider clause %q", clause)
		}
	}
}

func TestCodexRuntimeV3MigrationRoundTripLiveDB(t *testing.T) {
	pool, dsn, p, v := runtimeV3MigrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	before := runtimeV3Definition(t, pool)
	runtimeV3AssertProviderClauses(t, before)
	runtimeV3AssertVersion(t, pool, p)
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatal(err)
	}
	after := runtimeV3Definition(t, pool)
	runtimeV3AssertProviderClauses(t, after)
	if !strings.Contains(after, "'codex_runtime_v3'") {
		t.Fatal("committed Up did not require v3")
	}
	if strings.ReplaceAll(after, " AND 'codex_runtime_v3' = ANY(w.protocol_capabilities)", "") != before {
		t.Fatal("Up changed more than the runtime baseline")
	}
	runtimeV3AssertVersion(t, pool, v)
	if err := MigrateDownTo(ctx, dsn, p); err != nil {
		t.Fatal(err)
	}
	if runtimeV3Definition(t, pool) != before {
		t.Fatal("committed Down did not restore exact predecessor function definition")
	}
	runtimeV3AssertVersion(t, pool, p)
	var applied bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=$1 AND is_applied)", v).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("Goose Down metadata still marks v3 applied")
	}
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatalf("retry Up: %v", err)
	}
	if runtimeV3Definition(t, pool) != after {
		t.Fatal("retry Up changed function definition")
	}
	runtimeV3AssertVersion(t, pool, v)
}

func TestCodexRuntimeV3MigrationFailureRollbackLiveDB(t *testing.T) {
	pool, dsn, _, v := runtimeV3MigrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	before, metadata := runtimeV3Definition(t, pool), runtimeV3Metadata(t, pool)
	// The trigger runs after replacement but before Goose can record and commit Up.
	trigger := fmt.Sprintf(`CREATE FUNCTION public.runtime_v3_fail_metadata() RETURNS trigger LANGUAGE plpgsql AS $body$
 BEGIN
 IF NEW.version_id = %d AND NEW.is_applied THEN
 IF position('codex_runtime_v3' in pg_get_functiondef('%s'::regprocedure)) = 0 THEN
 RAISE EXCEPTION USING ERRCODE='P0002', MESSAGE='replacement not visible';
 END IF;
 RAISE EXCEPTION USING ERRCODE='P0001', MESSAGE='runtime-v3-after-replacement';
 END IF;
 RETURN NEW;
 END $body$;
 CREATE TRIGGER runtime_v3_fail_metadata BEFORE INSERT ON public.goose_db_version
 FOR EACH ROW EXECUTE FUNCTION public.runtime_v3_fail_metadata();`, v, runtimeV3Function)
	if _, err := pool.Exec(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	err := MigrateTo(ctx, dsn, v)
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "P0001" || pgerr.Message != "runtime-v3-after-replacement" {
		t.Fatalf("want post-replacement sentinel P0001, got %v", err)
	}
	if runtimeV3Definition(t, pool) != before || runtimeV3Metadata(t, pool) != metadata {
		t.Fatal("failed Goose Up did not roll back function AND metadata")
	}
	if _, err := pool.Exec(ctx, "DROP TRIGGER runtime_v3_fail_metadata ON public.goose_db_version; DROP FUNCTION public.runtime_v3_fail_metadata()"); err != nil {
		t.Fatal(err)
	}
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatalf("retry Up: %v", err)
	}
	runtimeV3AssertVersion(t, pool, v)
}

func TestCodexRuntimeV3MigrationLockTimeoutLiveDB(t *testing.T) {
	pool, dsn, _, v := runtimeV3MigrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	before, metadata := runtimeV3Definition(t, pool), runtimeV3Metadata(t, pool)
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	var holderPID int32
	if err := holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holderPID); err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "ALTER FUNCTION "+runtimeV3Function+" COST 101"); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	app := "runtime-v3-lock-" + uuid.NewString()
	query := u.Query()
	query.Set("application_name", app)
	u.RawQuery = query.Encode()
	result := make(chan error, 1)
	go func() { result <- MigrateTo(ctx, u.String(), v) }()
	// Always join migration before this test releases its resources.
	joined := false
	defer func() {
		if !joined {
			cancel()
			if err := <-result; err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("join canceled migration: %v", err)
			}
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
   WHERE application_name=$1 AND wait_event_type='Lock'
   AND $2::int = ANY(pg_blocking_pids(pid)) AND query ILIKE '%CREATE OR REPLACE FUNCTION%')`, app, holderPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-result:
			joined = true
			t.Fatalf("migration returned before observed catalog lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	err = <-result
	joined = true
	var pgerr *pgconn.PgError
	if !errors.As(err, &pgerr) || pgerr.Code != "55P03" || !strings.Contains(pgerr.Message, "lock timeout") {
		t.Fatalf("want migration SET LOCAL lock timeout 55P03, got %v", err)
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if runtimeV3Definition(t, pool) != before || runtimeV3Metadata(t, pool) != metadata {
		t.Fatal("lock timeout changed function or Goose metadata")
	}
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatalf("retry after releasing catalog lock: %v", err)
	}
	runtimeV3AssertVersion(t, pool, v)
}
