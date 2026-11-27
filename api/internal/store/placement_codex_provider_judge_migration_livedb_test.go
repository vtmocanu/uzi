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
	codexProviderJudgeOldArm = "OR (r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities)))"
	codexProviderJudgeNewArm = "OR (r.harness = 'codex' AND 'codex_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'codex_provider_judge_v1' = ANY(w.protocol_capabilities))))"
	// The run_env_v1 claim clause 00544 added; the Codex migration must keep it.
	runEnvKeptClause = "'run_env_v1' = ANY("
	// The Claude judge clause 00534 added; the Codex migration must keep it byte for byte.
	claudeProviderJudgeKeptArm = "OR (r.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities)))"
)

// codexProviderJudgeMigrationDB migrates a fresh child database to the highest version below the Codex
// judge capability migration and returns it with that predecessor and the migration's own version.
func codexProviderJudgeMigrationDB(t *testing.T) (*pgxpool.Pool, string, int64, int64) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run through owned store-IT runner")
	}
	v := migrationVersionByName(t, "placement_codex_provider_judge_capability")
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

// Up adds only the Codex judge clause to the Codex provider arm and keeps the Claude judge clause;
// Down restores the predecessor (00544 body: Claude judge and run_env_v1 clauses kept) function byte for byte; a
// retried Up lands the same definition; Goose records each step.
func TestPlacementCodexProviderJudgeMigrationRoundTripLiveDB(t *testing.T) {
	pool, dsn, p, v := codexProviderJudgeMigrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	before := runtimeV3Definition(t, pool)
	if strings.Contains(before, "codex_provider_judge_v1") || strings.Count(before, codexProviderJudgeOldArm) != 1 {
		t.Fatal("predecessor definition is not the pre-Codex-judge-capability function")
	}
	if strings.Count(before, claudeProviderJudgeKeptArm) != 1 {
		t.Fatal("predecessor definition lacks the Claude judge clause")
	}
	if !strings.Contains(before, runEnvKeptClause) {
		t.Fatal("predecessor definition lacks the 00544 run_env_v1 clause")
	}
	runtimeV3AssertVersion(t, pool, p)
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatal(err)
	}
	after := runtimeV3Definition(t, pool)
	if strings.Count(after, "codex_provider_judge_v1") != 1 || strings.Count(after, codexProviderJudgeNewArm) != 1 {
		t.Fatal("committed Up did not add the Codex judge clause once")
	}
	if strings.Count(after, claudeProviderJudgeKeptArm) != 1 {
		t.Fatal("committed Up lost the Claude judge clause")
	}
	if !strings.Contains(after, runEnvKeptClause) {
		t.Fatal("committed Up lost the 00544 run_env_v1 clause")
	}
	if strings.Replace(after, codexProviderJudgeNewArm, codexProviderJudgeOldArm, 1) != before {
		t.Fatal("Up changed more than the Codex provider arm")
	}
	runtimeV3AssertVersion(t, pool, v)
	if err := MigrateDownTo(ctx, dsn, p); err != nil {
		t.Fatal(err)
	}
	down := runtimeV3Definition(t, pool)
	if down != before {
		t.Fatal("committed Down did not restore the exact predecessor function definition")
	}
	if strings.Contains(down, "codex_provider_judge_v1") || strings.Count(down, claudeProviderJudgeKeptArm) != 1 {
		t.Fatal("Down must keep the Claude judge clause and drop the Codex one")
	}
	runtimeV3AssertVersion(t, pool, p)
	var applied bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=$1 AND is_applied)", v).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("Goose Down metadata still marks the Codex judge capability migration applied")
	}
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatalf("retry Up: %v", err)
	}
	if runtimeV3Definition(t, pool) != after {
		t.Fatal("retry Up changed the function definition")
	}
	runtimeV3AssertVersion(t, pool, v)
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=$1 AND is_applied)", v).Scan(&applied); err != nil || !applied {
		t.Fatalf("Goose row for the retried Up applied=%v err=%v", applied, err)
	}
}
