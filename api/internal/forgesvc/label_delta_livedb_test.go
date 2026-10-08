package forgesvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Each fixture owns a fresh repo, so the sweep's positive control cannot select
// a sibling test's issue.
func seedLabelDeltaLiveDB(t *testing.T) (context.Context, *store.Queries, *pgxpool.Pool, store.Issue) {
	t.Helper()
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
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("label-delta-%s@e2e", userID)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 4242, $3)`,
		connID, userID, []byte{0x1}); err != nil {
		t.Fatalf("seed connection: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 100, 'g/r', 'https://forge.e2e/g/r', true)`, repoID, connID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	row, err := q.UpsertIssue(ctx, store.UpsertIssueParams{
		RepoID: repoID, ForgeIssueIid: 31,
		Title: "stored title", State: "opened",
		Labels:         []byte(`["on-deck","uzi","unrelated","Planned"]`),
		AssigneeIds:    []byte("[4242,8181]"),
		WebUrl:         "https://forge.e2e/stored",
		Author:         pgtype.Text{String: "stored author", Valid: true},
		HasPrdLink:     false,
		ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	if err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET board_position = 731, synced_at = '2021-01-01T00:00:00Z' WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("seed manual position and sync timestamp: %v", err)
	}
	return ctx, q, pool, readLabelDeltaIssue(t, ctx, q, row)
}

func readLabelDeltaIssue(t *testing.T, ctx context.Context, q *store.Queries, key store.Issue) store.Issue {
	t.Helper()
	row, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: key.RepoID, ForgeIssueIid: key.ForgeIssueIid})
	if err != nil {
		t.Fatalf("read issue: %v", err)
	}
	return row
}

func assertLabelDeltaLabels(t *testing.T, where string, row store.Issue, present, absent []string) {
	t.Helper()
	var labels []string
	if err := json.Unmarshal(row.Labels, &labels); err != nil {
		t.Fatalf("%s labels: %v", where, err)
	}
	for _, label := range absent {
		if slices.Contains(labels, label) {
			t.Errorf("%s labels resurrected/retained forbidden %q: %v", where, label, labels)
		}
	}
	for _, label := range present {
		if !slices.Contains(labels, label) {
			t.Errorf("%s labels lost required %q: %v", where, label, labels)
		}
	}
}

func assertLabelDeltaIdentity(t *testing.T, where string, got, stored store.Issue) {
	t.Helper()
	if got.ID != stored.ID || got.RepoID != stored.RepoID || got.ForgeIssueIid != stored.ForgeIssueIid {
		t.Errorf("%s identity = (%s, %s, %d), want (%s, %s, %d)", where,
			got.ID, got.RepoID, got.ForgeIssueIid, stored.ID, stored.RepoID, stored.ForgeIssueIid)
	}
	var assignees []int64
	if err := json.Unmarshal(got.AssigneeIds, &assignees); err != nil {
		t.Fatalf("%s assignees: %v", where, err)
	}
	if !slices.Equal(assignees, []int64{4242, 8181}) {
		t.Errorf("%s assignees = %v, want [4242 8181]", where, assignees)
	}
	if got.BoardPosition != stored.BoardPosition {
		t.Errorf("%s board_position = %v, want %v", where, got.BoardPosition, stored.BoardPosition)
	}
}

func TestAutoMoveStaleSnapshotDoesNotResurrectOnDeckLiveDB(t *testing.T) {
	ctx, q, _, stored := seedLabelDeltaLiveDB(t)
	svc := New(q, nil, time.Second, nil)
	stale := stored
	stale.AssigneeIds = []byte("[]")
	stale.BoardPosition = pgtype.Int8{Int64: 999, Valid: true}

	assertSweep := func(stage string, want bool) {
		t.Helper()
		rows, err := q.ListSweepCandidateIssues(ctx, store.ListSweepCandidateIssuesParams{
			RepoID: stored.RepoID, Selector: "label", Labels: []byte(`["on-deck"]`),
			UziLabel: "uzi", BotID: 4242,
		})
		if err != nil {
			t.Fatalf("%s sweep selector: %v", stage, err)
		}
		found := slices.ContainsFunc(rows, func(row store.ListSweepCandidateIssuesRow) bool {
			return row.ForgeIssueIid == stored.ForgeIssueIid
		})
		if found != want {
			t.Errorf("%s sweep selected IID %d = %t, want %t", stage, stored.ForgeIssueIid, found, want)
		}
	}
	assertSweep("before removal", true)
	removed, err := svc.SetIssueLabel(ctx, &fakeForge{}, 100, stored, "on-deck", "", false)
	if err != nil {
		t.Fatalf("remove on-deck: %v", err)
	}
	assertLabelDeltaLabels(t, "removal returned", removed, []string{"uzi", "unrelated", "Planned"}, []string{"on-deck"})
	assertLabelDeltaLabels(t, "removal persisted", readLabelDeltaIssue(t, ctx, q, stored),
		[]string{"uzi", "unrelated", "Planned"}, []string{"on-deck"})
	assertSweep("immediately after removal", false)

	moved, err := svc.AutoMove(ctx, &fakeForge{}, 100, stale, boardColumns(), "In Progress")
	if err != nil {
		t.Fatalf("stale AutoMove: %v", err)
	}
	for _, result := range []struct {
		where string
		row   store.Issue
	}{{"stale move returned", moved}, {"stale move persisted", readLabelDeltaIssue(t, ctx, q, stored)}} {
		assertLabelDeltaLabels(t, result.where, result.row,
			[]string{"uzi", "unrelated", "In Progress"}, []string{"on-deck", "Planned"})
		assertLabelDeltaIdentity(t, result.where, result.row, stored)
	}
	assertSweep("after stale move", false)
}

func TestSetIssueLabelStaleApplyDoesNotResurrectOnDeckLiveDB(t *testing.T) {
	ctx, q, pool, stored := seedLabelDeltaLiveDB(t)
	svc := New(q, nil, time.Second, nil)
	stale := stored
	// UpsertIssueLabels omits ID, assignee_ids and board_position on conflict.
	// RepoID/IID identify that conflict; metadata still comes from the caller.
	stale.ID = uuid.New()
	stale.AssigneeIds = []byte("[]")
	stale.BoardPosition = pgtype.Int8{Int64: 999, Valid: true}
	stale.Title = "caller title"
	stale.State = "closed"
	stale.WebUrl = "https://forge.e2e/caller"
	stale.Author = pgtype.Text{String: "caller author", Valid: true}
	stale.HasPrdLink = true
	stale.ForgeUpdatedAt = pgtype.Timestamptz{Time: time.Date(2022, 2, 2, 0, 0, 0, 0, time.UTC), Valid: true}
	stale.SyncedAt = pgtype.Timestamptz{Time: time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true}
	removed, err := svc.SetIssueLabel(ctx, &fakeForge{}, 100, stored, "on-deck", "", false)
	if err != nil {
		t.Fatalf("remove on-deck: %v", err)
	}
	assertLabelDeltaLabels(t, "removal returned", removed, []string{"uzi", "unrelated", "Planned"}, []string{"on-deck"})
	assertLabelDeltaLabels(t, "removal persisted", readLabelDeltaIssue(t, ctx, q, stored),
		[]string{"uzi", "unrelated", "Planned"}, []string{"on-deck"})
	// Simulate another writer after the stale snapshot was captured.
	if _, err := pool.Exec(ctx,
		`UPDATE issues SET labels = labels || '["concurrent-unrelated"]'::jsonb,
		 synced_at = '2023-03-03T00:00:00Z' WHERE id = $1`, stored.ID); err != nil {
		t.Fatalf("concurrent unrelated addition: %v", err)
	}
	current := readLabelDeltaIssue(t, ctx, q, stored)
	assertLabelDeltaLabels(t, "before stale apply", current,
		[]string{"concurrent-unrelated"}, []string{"on-deck"})
	applied, err := svc.SetIssueLabel(ctx, &fakeForge{}, 100, stale, "another-label", PromoteLabelColor, true)
	if err != nil {
		t.Fatalf("stale SetIssueLabel apply: %v", err)
	}
	for _, result := range []struct {
		where string
		row   store.Issue
	}{{"stale apply returned", applied}, {"stale apply persisted", readLabelDeltaIssue(t, ctx, q, stored)}} {
		got := result.row
		assertLabelDeltaLabels(t, result.where, got,
			[]string{"uzi", "unrelated", "Planned", "concurrent-unrelated", "another-label"}, []string{"on-deck"})
		assertLabelDeltaIdentity(t, result.where, got, stored)
		if got.Title != stale.Title || got.State != stale.State || got.WebUrl != stale.WebUrl ||
			got.Author != stale.Author || got.HasPrdLink != stale.HasPrdLink ||
			got.ForgeUpdatedAt.Valid != stale.ForgeUpdatedAt.Valid || got.ForgeUpdatedAt.InfinityModifier != stale.ForgeUpdatedAt.InfinityModifier ||
			!got.ForgeUpdatedAt.Time.Equal(stale.ForgeUpdatedAt.Time) {
			t.Errorf("%s metadata did not retain caller winners: got %+v, caller %+v", result.where, got, stale)
		}
		if !got.SyncedAt.Valid || !got.SyncedAt.Time.After(current.SyncedAt.Time) || got.SyncedAt.Time.Equal(stale.SyncedAt.Time) {
			t.Errorf("%s synced_at = %v, want refreshed after stored %v and different from caller %v",
				result.where, got.SyncedAt, current.SyncedAt, stale.SyncedAt)
		}
	}
}
