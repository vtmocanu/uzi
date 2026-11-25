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
	providerJudgeOldArm = "OR (r.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities))"
	providerJudgeNewArm = "OR (r.harness = 'claude' AND 'claude_provider_v1' = ANY(w.protocol_capabilities) AND (r.kind <> 'judge' OR 'claude_provider_judge_v1' = ANY(w.protocol_capabilities)))"
)

// providerJudgeMigrationDB migrates a fresh child database to the highest version below the judge
// capability migration and returns it with that predecessor and the migration's own version.
func providerJudgeMigrationDB(t *testing.T) (*pgxpool.Pool, string, int64, int64) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run through owned store-IT runner")
	}
	v := migrationVersionByName(t, "placement_provider_judge_capability")
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

// Up adds only the judge clause to the Claude provider arm; Down restores the predecessor function
// byte for byte; a retried Up lands the same definition; Goose records each step.
func TestPlacementProviderJudgeMigrationRoundTripLiveDB(t *testing.T) {
	pool, dsn, p, v := providerJudgeMigrationDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	before := runtimeV3Definition(t, pool)
	if strings.Contains(before, "claude_provider_judge_v1") || strings.Count(before, providerJudgeOldArm) != 1 {
		t.Fatal("predecessor definition is not the pre-judge-capability function")
	}
	runtimeV3AssertVersion(t, pool, p)
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatal(err)
	}
	after := runtimeV3Definition(t, pool)
	if strings.Count(after, "claude_provider_judge_v1") != 1 || strings.Count(after, providerJudgeNewArm) != 1 {
		t.Fatal("committed Up did not add the judge clause once")
	}
	if strings.Replace(after, providerJudgeNewArm, providerJudgeOldArm, 1) != before {
		t.Fatal("Up changed more than the Claude provider arm")
	}
	runtimeV3AssertVersion(t, pool, v)
	if err := MigrateDownTo(ctx, dsn, p); err != nil {
		t.Fatal(err)
	}
	if runtimeV3Definition(t, pool) != before {
		t.Fatal("committed Down did not restore the exact predecessor function definition")
	}
	runtimeV3AssertVersion(t, pool, p)
	var applied bool
	if err := pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM goose_db_version WHERE version_id=$1 AND is_applied)", v).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("Goose Down metadata still marks the judge capability migration applied")
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
