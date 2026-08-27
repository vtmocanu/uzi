package store_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestAgentSourceStagedLiveDB pins the PRD #602 (M3) staging table: the singleton
// upsert round-trips the roles/diff jsonb, a second upsert updates the ONE row
// rather than inserting a second, the singleton UNIQUE + CHECK actually rejects a
// raw second row, and DeleteAgentSourceStaged clears it. The in-memory fakes cannot
// model jsonb NOT NULL columns or the singleton constraint, so this exercises the
// real SQL. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestAgentSourceStagedLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	// Start from a clean table so the singleton assertions are deterministic across
	// a re-used database.
	if err := q.DeleteAgentSourceStaged(ctx); err != nil {
		t.Fatalf("clear staged: %v", err)
	}

	roles1 := []byte(`[{"name":"coder","ok":true,"description":"builds"},{"name":"bad","ok":false,"reason":"invalid"}]`)
	diff1 := []byte(`[{"name":"coder","action":"add"}]`)

	fetchedAt := time.Now().UTC().Truncate(time.Second)
	first, err := q.UpsertAgentSourceStaged(ctx, store.UpsertAgentSourceStagedParams{
		FetchedAt:  pgtype.Timestamptz{Time: fetchedAt, Valid: true},
		FetchedSha: "sha-one",
		SourceUrl:  "https://example.test/agents.git",
		SourceRef:  "v1.0.0",
		Roles:      roles1,
		Diff:       diff1,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if first.FetchedSha != "sha-one" || first.SourceRef != "v1.0.0" {
		t.Fatalf("first upsert stored wrong scalars: %+v", first)
	}
	if !first.FetchedAt.Valid || !first.FetchedAt.Time.Equal(fetchedAt) {
		t.Errorf("fetched_at round-trip mismatch: got %v want %v", first.FetchedAt.Time, fetchedAt)
	}

	// jsonb normalizes whitespace/key-order, so assert structurally, not byte-wise.
	var gotRoles []map[string]any
	if err := json.Unmarshal(first.Roles, &gotRoles); err != nil {
		t.Fatalf("roles jsonb did not round-trip as JSON: %v", err)
	}
	if len(gotRoles) != 2 || gotRoles[0]["name"] != "coder" || gotRoles[1]["reason"] != "invalid" {
		t.Errorf("roles jsonb round-trip mismatch: %v", gotRoles)
	}

	read, err := q.GetAgentSourceStaged(ctx)
	if err != nil {
		t.Fatalf("get after first upsert: %v", err)
	}
	if read.ID != first.ID || read.FetchedSha != "sha-one" {
		t.Errorf("get returned a different row than upsert: read=%+v", read)
	}

	// Second upsert with a new SHA must UPDATE the one row (same id), not insert.
	second, err := q.UpsertAgentSourceStaged(ctx, store.UpsertAgentSourceStagedParams{
		FetchedAt:  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
		FetchedSha: "sha-two",
		SourceUrl:  "https://example.test/agents.git",
		SourceRef:  "main",
		Roles:      []byte(`[]`),
		Diff:       []byte(`[]`),
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("singleton upsert must keep the same row id; first=%d second=%d", first.ID, second.ID)
	}
	if second.FetchedSha != "sha-two" || second.SourceRef != "main" {
		t.Errorf("second upsert did not overwrite the row: %+v", second)
	}

	// The singleton UNIQUE + CHECK must reject a raw second row.
	if _, err := pool.Exec(ctx,
		`INSERT INTO agent_source_staged (fetched_sha, source_url, source_ref) VALUES ('x','https://x.test','y')`,
	); err == nil {
		t.Errorf("a raw second insert must violate the singleton UNIQUE constraint")
	}

	// Delete clears it; a subsequent Get returns no rows.
	if err := q.DeleteAgentSourceStaged(ctx); err != nil {
		t.Fatalf("delete staged: %v", err)
	}
	if _, err := q.GetAgentSourceStaged(ctx); err != pgx.ErrNoRows {
		t.Errorf("get after delete must be pgx.ErrNoRows; got %v", err)
	}
}
