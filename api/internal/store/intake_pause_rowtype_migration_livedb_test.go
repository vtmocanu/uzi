package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const intakePauseBody = "SELECT r.claim_generation > 0 OR r.kind = 'cross_check';"

// Keep the old definition independent of the migrations under test. Every entry
// point must repair an already-applied 00358, not just a freshly edited 00358.
const legacyIntakePauseFunction = `CREATE OR REPLACE FUNCTION fn_run_admitted_during_intake_pause(r runs) RETURNS boolean
LANGUAGE sql STABLE PARALLEL SAFE
BEGIN ATOMIC
 SELECT r.claim_generation > 0 OR r.kind = 'cross_check';
END;`

type intakePauseFunctionIdentity struct {
	OID, Owner uint32
	ACL        string
}

func intakePauseIdentity(t *testing.T, ctx context.Context, pool *pgxpool.Pool, parsed bool) intakePauseFunctionIdentity {
	t.Helper()
	var id intakePauseFunctionIdentity
	var hasParsedBody bool
	var body, volatility, parallel string
	var nonStrict, booleanResult, inputName bool
	if err := pool.QueryRow(ctx, `SELECT oid, proowner, COALESCE(proacl::text,''), prosqlbody IS NOT NULL,
 prosrc, provolatile::text, proparallel::text, NOT proisstrict,
 prorettype='boolean'::regtype, proargnames=ARRAY['r']::text[]
 FROM pg_proc WHERE oid='fn_run_admitted_during_intake_pause(runs)'::regprocedure`).Scan(
		&id.OID, &id.Owner, &id.ACL, &hasParsedBody, &body, &volatility, &parallel, &nonStrict, &booleanResult, &inputName); err != nil {
		t.Fatal(err)
	}
	if hasParsedBody != parsed || volatility != "s" || parallel != "s" || !nonStrict || !booleanResult || !inputName {
		t.Fatalf("function contract: parsed=%v (want %v), volatility=%q parallel=%q nonStrict=%v boolean=%v inputName=%v",
			hasParsedBody, parsed, volatility, parallel, nonStrict, booleanResult, inputName)
	}
	if !parsed && strings.TrimSpace(body) != intakePauseBody {
		t.Fatalf("string body=%q, want %q", body, intakePauseBody)
	}
	return id
}

func installLegacyIntakePause(t *testing.T, ctx context.Context, pool *pgxpool.Pool) intakePauseFunctionIdentity {
	t.Helper()
	mustExec(ctx, t, pool, legacyIntakePauseFunction)
	// A nondefault ACL makes a DROP/recreate detectably wrong, even when the same
	// migration owner would otherwise restore default PUBLIC execution privileges.
	mustExec(ctx, t, pool, `REVOKE EXECUTE ON FUNCTION fn_run_admitted_during_intake_pause(runs) FROM PUBLIC`)
	mustExec(ctx, t, pool, `GRANT EXECUTE ON FUNCTION fn_run_admitted_during_intake_pause(runs) TO pg_monitor`)
	return intakePauseIdentity(t, ctx, pool, true)
}

// Locate by slug so merge-time migration renumbering cannot silently leave the
// already-at-head entry point untested.
func intakePauseRepairVersion(t *testing.T) int64 {
	t.Helper()
	paths, err := filepath.Glob("migrations/*_intake_pause_string_body.sql")
	if err != nil || len(paths) != 1 {
		t.Fatalf("find intake-pause repair migration: %v, matches=%v", err, paths)
	}
	var version int64
	if _, err := fmt.Sscanf(filepath.Base(paths[0]), "%d_", &version); err != nil {
		t.Fatal(err)
	}
	return version
}

func TestIntakePauseRowtypeMigrationWalkLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "intake_rowtype_walk_")
	if err := store.MigrateTo(ctx, dsn, 361); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, version := range []int64{362, 363, 370, 394, 397, 407, intakePauseRepairVersion(t)} {
		if !t.Run(fmt.Sprintf("pending_%d", version), func(t *testing.T) {
			before := installLegacyIntakePause(t, ctx, pool)
			if err := store.MigrateTo(ctx, dsn, version); err != nil {
				t.Fatalf("migration %d must repair the legacy function before altering runs: %v", version, err)
			}
			if after := intakePauseIdentity(t, ctx, pool, false); after != before {
				t.Fatalf("identity/owner/ACL changed: before=%+v after=%+v", before, after)
			}
		}) {
			// Later steps depend on the preceding migration; do not manufacture
			// cascading failures after the first missing preamble.
			return
		}
	}
}

func TestIntakePauseRowtypeStableUpgradeLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "intake_rowtype_stable_")
	if err := store.MigrateTo(ctx, dsn, 403); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	before := installLegacyIntakePause(t, ctx, pool)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("upgrade from stable schema 403: %v", err)
	}
	if after := intakePauseIdentity(t, ctx, pool, false); after != before {
		t.Fatalf("stable upgrade changed function identity: before=%+v after=%+v", before, after)
	}
}

func TestIntakePauseRowtypeFreshCatalogLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "intake_rowtype_fresh_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("fresh migrate to head: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	// Deliberately conservative fresh-schema policy (#4273): reject every parsed
	// function body or argument default in public, not only table-rowtype inputs.
	// This is broader than the minimal predicate for this PostgreSQL refusal.
	var parsed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc
 WHERE pronamespace='public'::regnamespace AND (prosqlbody IS NOT NULL OR proargdefaults IS NOT NULL)`).Scan(&parsed); err != nil {
		t.Fatal(err)
	}
	if parsed != 0 {
		t.Fatalf("public functions with parsed bodies/defaults=%d, want 0 (#4273)", parsed)
	}
	intakePauseIdentity(t, ctx, pool, false)

	// Both SQL-body forms must retain the same three-valued admission rule.
	var semantics bool
	if err := pool.QueryRow(ctx, `SELECT bool_and(fn_run_admitted_during_intake_pause(
 jsonb_populate_record(NULL::runs, jsonb_build_object('kind',kind,'claim_generation',generation)))
 IS NOT DISTINCT FROM admitted) FROM (VALUES
 ('issue',0::bigint,false),('issue',1,true),('cross_check',0,true),
 ('cross_check',NULL,true),('issue',NULL,NULL),(NULL,0,NULL),('issue',-1,false)
 ) AS cases(kind,generation,admitted)`).Scan(&semantics); err != nil || !semantics {
		t.Fatalf("admission truth table: correct=%v err=%v", semantics, err)
	}

	// These are rolled back individually so neither test data nor schema leaks
	// to the catalog/rollback controls below. On affected PostgreSQL minors each
	// would refuse the original parsed rowtype function.
	for name, statements := range map[string][]string{
		"constant_default": {`ALTER TABLE runs ADD COLUMN rowtype_probe integer NOT NULL DEFAULT 0`},
		"volatile_default": {`ALTER TABLE runs ADD COLUMN rowtype_probe timestamptz DEFAULT clock_timestamp()`},
		"generated":        {`ALTER TABLE runs ADD COLUMN rowtype_probe bigint GENERATED ALWAYS AS (claim_generation+1) STORED`},
		"type_change":      {`ALTER TABLE runs ADD COLUMN rowtype_probe integer`, `ALTER TABLE runs ALTER COLUMN rowtype_probe TYPE bigint`},
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			for _, statement := range statements {
				if _, err := tx.Exec(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	before := intakePauseIdentity(t, ctx, pool, false)
	if err := store.MigrateDownTo(ctx, dsn, intakePauseRepairVersion(t)-1); err != nil {
		t.Fatal(err)
	}
	if after := intakePauseIdentity(t, ctx, pool, false); before != after {
		t.Fatalf("repair Down must keep compatible function: before=%+v after=%+v", before, after)
	}
}
