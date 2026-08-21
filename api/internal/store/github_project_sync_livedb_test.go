package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #364 M2 against a REAL Postgres. Exercises the github_project_links and
// github_project_items persistence end to end: upsert (insert + conflict-update),
// read-back, the poller's list/diff queries, the error set/clear observability
// path, the status-marker advance, and delete. sqlc/go build/go vet all pass
// regardless of whether the ON CONFLICT targets, the cascade, and the composite PK
// actually behave — only executing the SQL proves them.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.
func TestGithubProjectSyncLiveDB(t *testing.T) {
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("gps364-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// --- link upsert (insert) ---------------------------------------------
	link, err := q.UpsertGithubProjectLink(ctx, store.UpsertGithubProjectLinkParams{
		RepoID:        repoID,
		ProjectNodeID: "PVT_node_1",
		ProjectNumber: 7,
		StatusFieldID: "PVTSSF_field_1",
		StatusOptions: []byte(`{"To Do":"opt_todo","Done":"opt_done"}`),
		OwnedByUzi:    true,
	})
	if err != nil {
		t.Fatalf("UpsertGithubProjectLink insert: %v", err)
	}
	if link.ProjectNumber != 7 || link.ProjectNodeID != "PVT_node_1" || !link.OwnedByUzi {
		t.Fatalf("link after insert = %+v; want project_number 7, node PVT_node_1, owned_by_uzi true", link)
	}
	if link.LastSyncedAt.Valid {
		t.Errorf("last_synced_at should be NULL right after a link insert, got %v", link.LastSyncedAt)
	}

	// --- link upsert (conflict update) ------------------------------------
	link2, err := q.UpsertGithubProjectLink(ctx, store.UpsertGithubProjectLinkParams{
		RepoID:        repoID,
		ProjectNodeID: "PVT_node_2",
		ProjectNumber: 9,
		StatusFieldID: "PVTSSF_field_2",
		StatusOptions: []byte(`{"In Progress":"opt_wip"}`),
		OwnedByUzi:    false,
	})
	if err != nil {
		t.Fatalf("UpsertGithubProjectLink update: %v", err)
	}
	if link2.ID != link.ID {
		t.Errorf("conflict update created a new row (id %s != %s); ON CONFLICT (repo_id) did not fire", link2.ID, link.ID)
	}
	if link2.ProjectNumber != 9 || link2.ProjectNodeID != "PVT_node_2" || link2.OwnedByUzi {
		t.Fatalf("link after update = %+v; want project_number 9, node PVT_node_2, owned_by_uzi false", link2)
	}

	got, err := q.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("GetGithubProjectLinkByRepo: %v", err)
	}
	if got.ProjectNumber != 9 {
		t.Errorf("GetGithubProjectLinkByRepo project_number = %d, want 9", got.ProjectNumber)
	}

	// --- error set / clear -------------------------------------------------
	if err := q.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
		RepoID:    repoID,
		LastError: pgtype.Text{String: "boom", Valid: true},
	}); err != nil {
		t.Fatalf("SetGithubProjectLinkError: %v", err)
	}
	got, err = q.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("GetGithubProjectLinkByRepo after error: %v", err)
	}
	if got.LastError.String != "boom" || !got.LastError.Valid {
		t.Errorf("last_error = %+v, want set to \"boom\"", got.LastError)
	}
	if !got.LastSyncedAt.Valid {
		t.Error("last_synced_at should be stamped by SetGithubProjectLinkError")
	}
	if err := q.ClearGithubProjectLinkError(ctx, repoID); err != nil {
		t.Fatalf("ClearGithubProjectLinkError: %v", err)
	}
	got, err = q.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		t.Fatalf("GetGithubProjectLinkByRepo after clear: %v", err)
	}
	if got.LastError.Valid {
		t.Errorf("last_error should be NULL after clear, got %+v", got.LastError)
	}

	// --- item upsert (insert, then conflict update) ------------------------
	item, err := q.UpsertGithubProjectItem(ctx, store.UpsertGithubProjectItemParams{
		RepoID:             repoID,
		ForgeIssueIid:      101,
		ItemNodeID:         "PVTI_item_101",
		LastStatusOptionID: pgtype.Text{}, // nullable: no Status written yet
	})
	if err != nil {
		t.Fatalf("UpsertGithubProjectItem insert: %v", err)
	}
	if item.LastStatusOptionID.Valid {
		t.Errorf("last_status_option_id should be NULL on first insert, got %+v", item.LastStatusOptionID)
	}
	if _, err := q.UpsertGithubProjectItem(ctx, store.UpsertGithubProjectItemParams{
		RepoID:             repoID,
		ForgeIssueIid:      101,
		ItemNodeID:         "PVTI_item_101b",
		LastStatusOptionID: pgtype.Text{String: "opt_todo", Valid: true},
	}); err != nil {
		t.Fatalf("UpsertGithubProjectItem update: %v", err)
	}
	// A second, distinct issue, to prove the list is repo-scoped and multi-row.
	if _, err := q.UpsertGithubProjectItem(ctx, store.UpsertGithubProjectItemParams{
		RepoID:        repoID,
		ForgeIssueIid: 102,
		ItemNodeID:    "PVTI_item_102",
	}); err != nil {
		t.Fatalf("UpsertGithubProjectItem 102: %v", err)
	}

	one, err := q.GetGithubProjectItem(ctx, store.GetGithubProjectItemParams{RepoID: repoID, ForgeIssueIid: 101})
	if err != nil {
		t.Fatalf("GetGithubProjectItem: %v", err)
	}
	if one.ItemNodeID != "PVTI_item_101b" || one.LastStatusOptionID.String != "opt_todo" {
		t.Fatalf("item 101 after upsert-update = %+v; want node PVTI_item_101b, marker opt_todo", one)
	}

	items, err := q.ListGithubProjectItems(ctx, repoID)
	if err != nil {
		t.Fatalf("ListGithubProjectItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("ListGithubProjectItems = %d rows, want 2 (composite PK dedups the 101 upserts)", len(items))
	}
	if items[0].ForgeIssueIid != 101 || items[1].ForgeIssueIid != 102 {
		t.Errorf("ListGithubProjectItems order = [%d,%d], want [101,102] (forge_issue_iid ASC)",
			items[0].ForgeIssueIid, items[1].ForgeIssueIid)
	}

	// --- status marker advance (diff basis) --------------------------------
	if err := q.SetGithubProjectItemStatusMarker(ctx, store.SetGithubProjectItemStatusMarkerParams{
		RepoID:             repoID,
		ForgeIssueIid:      101,
		LastStatusOptionID: pgtype.Text{String: "opt_done", Valid: true},
	}); err != nil {
		t.Fatalf("SetGithubProjectItemStatusMarker: %v", err)
	}
	one, err = q.GetGithubProjectItem(ctx, store.GetGithubProjectItemParams{RepoID: repoID, ForgeIssueIid: 101})
	if err != nil {
		t.Fatalf("GetGithubProjectItem after marker: %v", err)
	}
	if one.LastStatusOptionID.String != "opt_done" {
		t.Errorf("marker = %q, want opt_done", one.LastStatusOptionID.String)
	}
	if one.ItemNodeID != "PVTI_item_101b" {
		t.Errorf("SetGithubProjectItemStatusMarker must not touch item_node_id, got %q", one.ItemNodeID)
	}

	// --- item delete -------------------------------------------------------
	if err := q.DeleteGithubProjectItem(ctx, store.DeleteGithubProjectItemParams{RepoID: repoID, ForgeIssueIid: 101}); err != nil {
		t.Fatalf("DeleteGithubProjectItem: %v", err)
	}
	if _, err := q.GetGithubProjectItem(ctx, store.GetGithubProjectItemParams{RepoID: repoID, ForgeIssueIid: 101}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetGithubProjectItem after delete err = %v, want pgx.ErrNoRows", err)
	}

	// --- link delete -------------------------------------------------------
	if err := q.DeleteGithubProjectLink(ctx, repoID); err != nil {
		t.Fatalf("DeleteGithubProjectLink: %v", err)
	}
	if _, err := q.GetGithubProjectLinkByRepo(ctx, repoID); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("GetGithubProjectLinkByRepo after delete err = %v, want pgx.ErrNoRows", err)
	}

	// --- repo-cascade: items drop when the repo is deleted -----------------
	// Re-seed one item, delete the repo, and confirm the FK ON DELETE CASCADE
	// removed the item (the migration keys the item table off repos, not the link).
	if _, err := q.UpsertGithubProjectItem(ctx, store.UpsertGithubProjectItemParams{
		RepoID: repoID, ForgeIssueIid: 200, ItemNodeID: "PVTI_cascade",
	}); err != nil {
		t.Fatalf("UpsertGithubProjectItem for cascade: %v", err)
	}
	mustExec(ctx, t, pool, `DELETE FROM repos WHERE id = $1`, repoID)
	if _, err := q.GetGithubProjectItem(ctx, store.GetGithubProjectItemParams{RepoID: repoID, ForgeIssueIid: 200}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("item should have cascaded away with the repo, GetGithubProjectItem err = %v, want pgx.ErrNoRows", err)
	}
}
