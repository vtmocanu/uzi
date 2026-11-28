package store

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// The Codex provider arm as 00545 left it, and as the checker migration rewrites it.
	codexProviderCheckerOldArm = "OR (r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities))))"
	codexProviderCheckerNewArm = "OR (r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities)) AND (r.kind <> 'cross_check' OR 'codex_provider_checker_v1' = ANY(w.protocol_capabilities))))"
)

// codexProviderCheckerMigrationDB migrates a fresh child database to the highest version below the
// Codex checker capability migration and returns it with that predecessor and the migration's own
// version, both looked up by name so a renumber on landing does not break the test.
func codexProviderCheckerMigrationDB(t *testing.T) (*pgxpool.Pool, string, int64, int64) {
	t.Helper()
	if os.Getenv("UZI_TEST_DATABASE_URL") == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run through owned store-IT runner")
	}
	v := migrationVersionByName(t, "placement_codex_provider_checker_capability")
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
	pool, dsn := codexJudgeMigrationDB(t, predecessor)
	return pool, dsn, predecessor, v
}

// Up adds only the Codex checker clause to the Codex provider arm and keeps the judge clauses and the
// run_env_v1 clause; Down restores the 00545 definition byte for byte; a retried Up lands the same
// definition; Goose records each step.
func TestPlacementCodexProviderCheckerMigrationRoundTripLiveDB(t *testing.T) {
	pool, dsn, p, v := codexProviderCheckerMigrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	before := runtimeV3Definition(t, pool)
	if strings.Contains(before, "codex_provider_checker_v1") || strings.Count(before, codexProviderCheckerOldArm) != 1 {
		t.Fatal("predecessor definition is not the pre-Codex-checker-capability function")
	}
	if strings.Count(before, claudeProviderJudgeKeptArm) != 1 || !strings.Contains(before, runEnvKeptClause) {
		t.Fatal("predecessor definition lacks the Claude judge or run_env_v1 clause")
	}
	runtimeV3AssertVersion(t, pool, p)
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatal(err)
	}
	after := runtimeV3Definition(t, pool)
	if strings.Count(after, "codex_provider_checker_v1") != 1 || strings.Count(after, codexProviderCheckerNewArm) != 1 {
		t.Fatal("committed Up did not add the Codex checker clause once")
	}
	if strings.Count(after, claudeProviderJudgeKeptArm) != 1 || !strings.Contains(after, runEnvKeptClause) {
		t.Fatal("committed Up lost the Claude judge or run_env_v1 clause")
	}
	if strings.Replace(after, codexProviderCheckerNewArm, codexProviderCheckerOldArm, 1) != before {
		t.Fatal("Up changed more than the Codex provider arm")
	}
	runtimeV3AssertVersion(t, pool, v)
	if err := MigrateDownTo(ctx, dsn, p); err != nil {
		t.Fatal(err)
	}
	if down := runtimeV3Definition(t, pool); down != before {
		t.Fatal("committed Down did not restore the exact 00545 function definition")
	}
	runtimeV3AssertVersion(t, pool, p)
	var applied bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=$1 AND is_applied)", v).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("Goose Down metadata still marks the Codex checker capability migration applied")
	}
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatalf("retry Up: %v", err)
	}
	if runtimeV3Definition(t, pool) != after {
		t.Fatal("retry Up changed the function definition")
	}
	runtimeV3AssertVersion(t, pool, v)
}
