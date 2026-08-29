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

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUpsertIssueLabelsPreservesAssigneesLiveDB is PRD #767's guard against the
// stale-snapshot clobber: SetIssueLabel and AutoMove are label-only cache writes, and
// both carry the CALLER's in-memory store.Issue. If that snapshot is stale — a label
// mutation racing a concurrent forge sync — its assignee_ids must NOT overwrite the
// DB's freshly-synced ones, or "assigned-to-bot" run-eligibility momentarily drops
// until the next poll. The fix routes both paths through UpsertIssueLabels, whose
// query omits assignee_ids from both its INSERT and its ON CONFLICT DO UPDATE SET, so
// the DB row's assignees are preserved atomically.
//
// This is a live-DB test, modelled on TestUpsertIssuePreservesBoardPositionLiveDB: the
// preservation IS the SQL (an omitted ON CONFLICT column), which a fake store cannot
// reproduce, and a green sqlc generate is not evidence the omission survives at the
// server. The stale snapshot carries AssigneeIds = [] while the DB holds [botID]; a
// regression that re-added assignee_ids to the query (or reverted the call site to
// UpsertIssue) would write the [] through and redden the assertion.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. `go test ./...` without it SKIPs.
func TestUpsertIssueLabelsPreservesAssigneesLiveDB(t *testing.T) {
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

	const botID = 4242

	// Fresh ids per run so re-runs against the same database do not collide.
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("label-preserve-%s@e2e", userID)); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', $3, $4)`,
		connID, userID, botID, []byte{0x1}); err != nil {
		t.Fatalf("seed forge_connection: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 100, 'g/r', 'https://forge.e2e/g/r', true)`,
		repoID, connID); err != nil {
		t.Fatalf("seed repo: %v", err)
	}

	// seedAssignedIssue persists a row whose stored assignee_ids = [botID] with the
	// given labels — the "freshly synced" state a racing label mutation must not clobber.
	seedAssignedIssue := func(t *testing.T, iid int64, labels ...string) {
		t.Helper()
		lj, _ := json.Marshal(labels)
		if _, err := q.UpsertIssue(ctx, store.UpsertIssueParams{
			RepoID:         repoID,
			ForgeIssueIid:  iid,
			Title:          "seeded",
			State:          "opened",
			Labels:         lj,
			AssigneeIds:    []byte(fmt.Sprintf("[%d]", botID)),
			WebUrl:         "https://x",
			HasPrdLink:     false,
			ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}); err != nil {
			t.Fatalf("seed assigned issue %d: %v", iid, err)
		}
	}

	// readAssignees reads assignee_ids back through GetIssueByIID (which SELECTs the column).
	readAssignees := func(t *testing.T, iid int64) []int64 {
		t.Helper()
		row, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: iid})
		if err != nil {
			t.Fatalf("GetIssueByIID %d: %v", iid, err)
		}
		var ids []int64
		if err := json.Unmarshal(row.AssigneeIds, &ids); err != nil {
			t.Fatalf("assignee_ids for %d not valid jsonb: %q (%v)", iid, row.AssigneeIds, err)
		}
		return ids
	}
	readLabels := func(t *testing.T, iid int64) []string {
		t.Helper()
		row, err := q.GetIssueByIID(ctx, store.GetIssueByIIDParams{RepoID: repoID, ForgeIssueIid: iid})
		if err != nil {
			t.Fatalf("GetIssueByIID %d: %v", iid, err)
		}
		var labels []string
		if err := json.Unmarshal(row.Labels, &labels); err != nil {
			t.Fatalf("labels for %d not valid jsonb: %q (%v)", iid, row.Labels, err)
		}
		return labels
	}

	svc := New(q, nil, time.Second, nil)

	t.Run("SetIssueLabel keeps forge-synced assignees under a stale snapshot", func(t *testing.T) {
		const iid = 11
		seedAssignedIssue(t, iid, "PRD")

		// The caller's snapshot is STALE: it never saw the bot assignment (AssigneeIds
		// = []). Adding the uzi label must not write that empty set through.
		stale := store.Issue{
			RepoID:         repoID,
			ForgeIssueIid:  iid,
			Title:          "seeded",
			State:          "opened",
			Labels:         []byte(`["PRD"]`),
			AssigneeIds:    []byte("[]"),
			HasPrdLink:     false,
			ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}
		if _, err := svc.SetIssueLabel(ctx, &fakeForge{}, 100, stale, "uzi", PromoteLabelColor, true); err != nil {
			t.Fatalf("SetIssueLabel: %v", err)
		}

		// Positive control: the label write really landed (else the assertion is vacuous).
		if got := readLabels(t, iid); !slices.Equal(got, []string{"PRD", "uzi"}) {
			t.Fatalf("labels = %v, want [PRD uzi] — the label-only upsert must apply", got)
		}
		// The point: the DB's forge-synced assignee survived the stale [] snapshot.
		if got := readAssignees(t, iid); !slices.Equal(got, []int64{botID}) {
			t.Fatalf("assignee_ids = %v, want [%d] — a label mutation must not clobber forge-synced assignees", got, botID)
		}
	})

	t.Run("AutoMove keeps forge-synced assignees under a stale snapshot", func(t *testing.T) {
		const iid = 12
		seedAssignedIssue(t, iid, "Later")

		stale := store.Issue{
			RepoID:         repoID,
			ForgeIssueIid:  iid,
			Title:          "seeded",
			State:          "opened",
			Labels:         []byte(`["Later"]`),
			AssigneeIds:    []byte("[]"),
			HasPrdLink:     false,
			ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
		}
		if _, err := svc.AutoMove(ctx, &fakeForge{}, 100, stale, boardColumns(), "Planned"); err != nil {
			t.Fatalf("AutoMove: %v", err)
		}

		// Positive control: the column move landed (Later stripped, Planned present).
		got := readLabels(t, iid)
		if slices.Contains(got, "Later") || !slices.Contains(got, "Planned") {
			t.Fatalf("labels = %v, want the move to Planned applied (Later stripped)", got)
		}
		if got := readAssignees(t, iid); !slices.Equal(got, []int64{botID}) {
			t.Fatalf("assignee_ids = %v, want [%d] — a column move must not clobber forge-synced assignees", got, botID)
		}
	})
}
