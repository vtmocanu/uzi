package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUpsertIssueRoundTripsAssigneeIdsLiveDB is PRD #767 M1's live-DB round-trip
// proof: an issue upserted with assignee_ids = [42, 99] reads the SAME numeric ids
// back out through GetIssueByIID (which SELECTs the new column), and an unassigned
// issue reads back as an empty jsonb array `[]`. This fails against pre-change code
// (net-new column + the GetIssueByIID select of it). It lives here rather than in a
// fake-store unit test because the assignee_ids column, its NOT NULL DEFAULT '[]',
// and the SELECT * surfacing are all SQL — sqlc's type deduction is not Postgres's,
// so only a real server proves the column reaches the gate.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. `go test ./...` without it SKIPs.
func TestUpsertIssueRoundTripsAssigneeIdsLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)

	// Fresh ids per run so re-runs against the same database do not collide.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("assignee-ids-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 42, $3)`,
		connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 100, 'g/r', 'https://forge.e2e/g/r', true)`,
		repoID, connID)

	// (1) Assigned issue: [42, 99] must survive the round trip verbatim.
	if _, err := q.UpsertIssue(ctx, store.UpsertIssueParams{
		RepoID:         repoID,
		ForgeIssueIid:  11,
		Title:          "assigned",
		State:          "opened",
		Labels:         []byte("[]"),
		AssigneeIds:    []byte("[42,99]"),
		WebUrl:         "https://x",
		HasPrdLink:     false,
		ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpsertIssue (assigned): %v", err)
	}
	assigned, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: 11})
	if err != nil {
		t.Fatalf("GetIssueByIID (assigned): %v", err)
	}
	var gotIDs []int64
	if err := json.Unmarshal(assigned.AssigneeIds, &gotIDs); err != nil {
		t.Fatalf("assigned assignee_ids not valid jsonb: %q (%v)", assigned.AssigneeIds, err)
	}
	if len(gotIDs) != 2 || gotIDs[0] != 42 || gotIDs[1] != 99 {
		t.Fatalf("assignee ids did not round-trip: got %v, want [42 99]", gotIDs)
	}

	// (2) Unassigned issue: an empty array must survive as [], not NULL, not null.
	if _, err := q.UpsertIssue(ctx, store.UpsertIssueParams{
		RepoID:         repoID,
		ForgeIssueIid:  12,
		Title:          "unassigned",
		State:          "opened",
		Labels:         []byte("[]"),
		AssigneeIds:    []byte("[]"),
		WebUrl:         "https://x",
		HasPrdLink:     false,
		ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpsertIssue (unassigned): %v", err)
	}
	unassigned, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: 12})
	if err != nil {
		t.Fatalf("GetIssueByIID (unassigned): %v", err)
	}
	var emptyIDs []int64
	if err := json.Unmarshal(unassigned.AssigneeIds, &emptyIDs); err != nil {
		t.Fatalf("unassigned assignee_ids not valid jsonb: %q (%v)", unassigned.AssigneeIds, err)
	}
	if len(emptyIDs) != 0 {
		t.Fatalf("unassigned issue must round-trip an empty assignee set, got %v", emptyIDs)
	}

	// (3) ON CONFLICT UPDATE must overwrite assignee_ids: re-upsert issue 11 unassigned.
	if _, err := q.UpsertIssue(ctx, store.UpsertIssueParams{
		RepoID:         repoID,
		ForgeIssueIid:  11,
		Title:          "assigned",
		State:          "opened",
		Labels:         []byte("[]"),
		AssigneeIds:    []byte("[]"),
		WebUrl:         "https://x",
		HasPrdLink:     false,
		ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}); err != nil {
		t.Fatalf("UpsertIssue (re-upsert 11): %v", err)
	}
	recleared, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: 11})
	if err != nil {
		t.Fatalf("GetIssueByIID (re-upsert 11): %v", err)
	}
	var reclearedIDs []int64
	if err := json.Unmarshal(recleared.AssigneeIds, &reclearedIDs); err != nil {
		t.Fatalf("re-upserted assignee_ids not valid jsonb: %q (%v)", recleared.AssigneeIds, err)
	}
	if len(reclearedIDs) != 0 {
		t.Fatalf("ON CONFLICT must overwrite assignee_ids to [], got %v", reclearedIDs)
	}
}
