package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for the PRD #767 M4 sweep selector widening: ListSweepCandidateIssues
// and CountSweepCandidateIssues gained a @selector discriminator + a @bot_id param, so a
// sweep can select either by label (unchanged) or by "assigned to the uzi-bot account".
//
// This must run under a real Postgres, not just a green sqlc generate, because of the jsonb
// numeric-membership trap (PRD #767 R3): assignee_ids holds NUMERIC ids, and the
// string-membership form the label predicates use (jsonb_exists) does NOT match a JSON
// number. Only `assignee_ids @> to_jsonb(@bot_id::bigint)` matches, and sqlc's type
// deduction is not Postgres's — the predicate is unproven until executed live. It also
// exercises the assigned positive that could NOT exist under the pre-change label-only query
// (which had no Selector/BotID params and no assignee predicate at all).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.

const (
	sweepAssignBotID   int64 = 6060 // the uzi-bot's forge user id
	sweepAssignHumanID int64 = 7070 // a different (human) assignee id
)

// sweepAssignmentFixture seeds one repo whose issues cover the assigned/label sweep matrix.
func sweepAssignmentFixture(ctx context.Context, t *testing.T) (*store.Queries, uuid.UUID) {
	t.Helper()
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

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("sweepassign-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', $3, $4, $5)`,
		connID, userID, "bot-sweepassign", sweepAssignBotID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 6001, 'g/sweepassign', 'https://forge.e2e/g/sweepassign', true)`,
		repoID, connID)

	seed := func(iid int64, state, labels, assignees string) {
		mustExec(ctx, t, pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, assignee_ids, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, $2, 't', $3, $4::jsonb, $5::jsonb, 'https://x', true, now(), now())`,
			repoID, iid, state, labels, assignees)
	}
	// 41: bot-assigned, no uzi label                → assigned candidate (the NEW positive).
	seed(41, "opened", `[]`, fmt.Sprintf("[%d]", sweepAssignBotID))
	// 42: bot-assigned AND uzi label                → assigned candidate (also matched by label).
	seed(42, "opened", `["uzi"]`, fmt.Sprintf("[%d]", sweepAssignBotID))
	// 43: human-assigned only (different numeric id) → NOT assigned (jsonb numeric trap guard).
	seed(43, "opened", `[]`, fmt.Sprintf("[%d]", sweepAssignHumanID))
	// 44: uzi label only, not assigned              → label candidate, NOT assigned.
	seed(44, "opened", `["uzi"]`, `[]`)
	// 45: bot-assigned but CLOSED                    → excluded from both (state gate).
	seed(45, "closed", `[]`, fmt.Sprintf("[%d]", sweepAssignBotID))
	return store.New(pool), repoID
}

func sweepAssignedIIDs(t *testing.T, q *store.Queries, repoID uuid.UUID, selector string, labels []byte, botID int64) []int64 {
	t.Helper()
	rows, err := q.ListSweepCandidateIssues(context.Background(), store.ListSweepCandidateIssuesParams{
		RepoID:   repoID,
		Selector: selector,
		Labels:   labels,
		BotID:    botID,
	})
	if err != nil {
		t.Fatalf("ListSweepCandidateIssues: %v", err)
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ForgeIssueIid)
	}
	return out
}

func sweepAssignedCount(t *testing.T, q *store.Queries, repoID uuid.UUID, selector string, labels []byte, botID int64) int64 {
	t.Helper()
	n, err := q.CountSweepCandidateIssues(context.Background(), store.CountSweepCandidateIssuesParams{
		RepoID:   repoID,
		Selector: selector,
		Labels:   labels,
		BotID:    botID,
	})
	if err != nil {
		t.Fatalf("CountSweepCandidateIssues: %v", err)
	}
	return n
}

// TestListSweepCandidateIssuesAssignedSelectorLiveDB asserts the exact candidate set for the
// assigned selector with the real numeric bot id. It fails against pre-change code, where
// ListSweepCandidateIssues had no Selector/BotID params and matched only by label, so an
// unlabelled bot-assigned issue (41) could never be a candidate.
func TestListSweepCandidateIssuesAssignedSelectorLiveDB(t *testing.T) {
	ctx := context.Background()
	q, repoID := sweepAssignmentFixture(ctx, t)

	// Assigned selector, real bot id: the two OPEN bot-assigned issues, in iid order.
	got := sweepAssignedIIDs(t, q, repoID, "assigned", []byte("[]"), sweepAssignBotID)
	if len(got) != 2 || got[0] != 41 || got[1] != 42 {
		t.Fatalf("assigned candidates = %v, want [41 42]:\n"+
			"  41 bot-assigned, no uzi → the NEW assigned candidate\n"+
			"  42 bot-assigned + uzi   → assigned candidate\n"+
			"  43 human-assigned       → NOT (numeric bot id must match — jsonb trap guard)\n"+
			"  44 uzi label only       → NOT (not assigned)\n"+
			"  45 bot-assigned CLOSED  → NOT (state gate)", got)
	}
	if n := sweepAssignedCount(t, q, repoID, "assigned", []byte("[]"), sweepAssignBotID); n != 2 {
		t.Fatalf("assigned count = %d, want 2 (matches the list)", n)
	}

	// BotID = 0: the assigned branch's `@bot_id > 0` guard keeps it off — nothing matches,
	// even though issue 45's assignee list is non-empty.
	if got := sweepAssignedIIDs(t, q, repoID, "assigned", []byte("[]"), 0); len(got) != 0 {
		t.Fatalf("assigned candidates with BotID=0 = %v, want none (the @bot_id > 0 guard)", got)
	}
	if n := sweepAssignedCount(t, q, repoID, "assigned", []byte("[]"), 0); n != 0 {
		t.Fatalf("assigned count with BotID=0 = %d, want 0", n)
	}
}

// TestListSweepCandidateIssuesLabelSelectorUnchangedLiveDB pins that the label selector is
// byte-for-byte the old behavior: label containment, and a bot-assigned-only issue (41) is
// excluded because it carries no uzi label. BotID is passed but must be ignored on the label
// branch.
func TestListSweepCandidateIssuesLabelSelectorUnchangedLiveDB(t *testing.T) {
	ctx := context.Background()
	q, repoID := sweepAssignmentFixture(ctx, t)

	uzi := []byte(`["uzi"]`)
	got := sweepAssignedIIDs(t, q, repoID, "label", uzi, sweepAssignBotID)
	// The uzi-labelled OPEN issues (42, 44), in iid order; 41 (bot-assigned, no label) is out.
	if len(got) != 2 || got[0] != 42 || got[1] != 44 {
		t.Fatalf("label candidates = %v, want [42 44] (label containment; bot-assigned-only 41 excluded)", got)
	}
	if n := sweepAssignedCount(t, q, repoID, "label", uzi, sweepAssignBotID); n != 2 {
		t.Fatalf("label count = %d, want 2 (matches the list)", n)
	}
}
