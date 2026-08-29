package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for ListAutopilotCandidateIssues' PRD #767 M3 widening: the
// eligibility half is now "carries the uzi label OR is assigned to the uzi-bot",
// while the autopilot label stays required (the trigger is unchanged).
//
// This must run under a real Postgres, not just a green sqlc generate, because of the
// jsonb numeric-membership trap (PRD #767 R3): assignee_ids holds NUMERIC ids, and the
// string-membership form the label predicates use (jsonb_exists) does NOT match a JSON
// number. Only `assignee_ids @> to_jsonb(@bot_id::bigint)` matches, and sqlc's type
// deduction is not Postgres's — so the predicate is unproven until executed live.
//
// Mutation calibration: dropping the @label (autopilot) predicate would wrongly admit
// issue 24 (autopilot removed but bot-assigned) — see the "no autopilot" row below.
// Dropping the assignee predicate would drop issue 22, the new positive. The
// human-assigned row 23 guards the jsonb numeric trap: a non-bot numeric id must not
// match. Row 25 guards the @bot_id > 0 guard: with BotID = 0 the assignment branch
// must stay off.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.

const (
	apAssignBotID   int64 = 8080 // the uzi-bot's forge user id
	apAssignHumanID int64 = 9090 // a different (human) assignee id
)

// apAssignmentFixture seeds one repo whose issues cover the full assignment-eligibility
// matrix. Every row carries a distinct combination of {autopilot label, uzi label,
// assignee_ids} so each assertion isolates exactly one predicate.
func apAssignmentFixture(ctx context.Context, t *testing.T) (*store.Queries, uuid.UUID) {
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
		userID, fmt.Sprintf("apassign-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', $3, $4, $5)`,
		connID, userID, "bot-apassign", apAssignBotID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 8001, 'g/apassign', 'https://forge.e2e/g/apassign', true)`,
		repoID, connID)

	seed := func(iid int64, state, labels, assignees string) {
		mustExec(ctx, t, pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, assignee_ids, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, $2, 't', $3, $4::jsonb, $5::jsonb, 'https://x', true, now(), now())`,
			repoID, iid, state, labels, assignees)
	}
	// 21: autopilot + uzi, no assignee            → candidate (uzi-label branch).
	seed(21, "opened", `["uzi","autopilot"]`, `[]`)
	// 22: autopilot + bot-assigned, NO uzi        → candidate (the NEW positive, assignment branch).
	seed(22, "opened", `["autopilot"]`, fmt.Sprintf("[%d]", apAssignBotID))
	// 23: autopilot + a DIFFERENT (human) assignee, no uzi → NOT a candidate (numeric bot id must match).
	seed(23, "opened", `["autopilot"]`, fmt.Sprintf("[%d]", apAssignHumanID))
	// 24: bot-assigned + uzi, but NO autopilot    → NOT a candidate (autopilot label still required).
	seed(24, "opened", `["uzi"]`, fmt.Sprintf("[%d]", apAssignBotID))
	// 25: autopilot + assigned to id 0, no uzi    → NOT a candidate when BotID = 0 (the @bot_id > 0 guard).
	seed(25, "opened", `["autopilot"]`, `[0]`)
	// 26: autopilot only, no uzi, not bot-assigned → NOT a candidate (neither eligibility branch fires).
	seed(26, "opened", `["autopilot"]`, `[]`)
	return store.New(pool), repoID
}

func assignmentCandidateIIDs(t *testing.T, q *store.Queries, repoID uuid.UUID, autopilotLabel, uziLabel string, botID int64) []int64 {
	t.Helper()
	rows, err := q.ListAutopilotCandidateIssues(context.Background(), store.ListAutopilotCandidateIssuesParams{
		RepoID:   repoID,
		Label:    autopilotLabel,
		UziLabel: uziLabel,
		BotID:    botID,
	})
	if err != nil {
		t.Fatalf("ListAutopilotCandidateIssues: %v", err)
	}
	out := make([]int64, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ForgeIssueIid)
	}
	return out
}

// TestListAutopilotCandidateIssuesAssignmentEligibilityLiveDB asserts the exact
// candidate set for the full matrix with the real bot id. It fails against pre-change
// code, where issue 22 (autopilot + bot-assigned, no uzi label) could never be a
// candidate because the query required the uzi label.
func TestListAutopilotCandidateIssuesAssignmentEligibilityLiveDB(t *testing.T) {
	ctx := context.Background()
	q, repoID := apAssignmentFixture(ctx, t)

	got := assignmentCandidateIIDs(t, q, repoID, "autopilot", "uzi", apAssignBotID)

	// Expected: 21 (uzi-label branch) and 22 (assignment branch), in iid order.
	if len(got) != 2 || got[0] != 21 || got[1] != 22 {
		t.Fatalf("candidates = %v, want [21 22]:\n"+
			"  21 autopilot+uzi (uzi branch) → candidate\n"+
			"  22 autopilot+bot-assigned, no uzi (assignment branch) → the NEW candidate\n"+
			"  23 autopilot+human-assigned → NOT (numeric bot id must match — jsonb trap guard)\n"+
			"  24 bot-assigned+uzi, no autopilot → NOT (autopilot label still required)\n"+
			"  25 autopilot+assignee 0 → NOT under bot id %d (only excluded by the value, re-checked in the BotID=0 test)\n"+
			"  26 autopilot only, unassigned → NOT (neither eligibility branch fires)", got, apAssignBotID)
	}
}

// TestListAutopilotCandidateIssuesBotIDZeroGuardLiveDB pins the `@bot_id > 0` guard: a
// zero/unresolved bot id must never make the assignment branch fire, even for an issue
// whose assignee_ids literally contains 0. Only the uzi-label branch may match, so with
// BotID = 0 the sole candidate is 21 (autopilot + uzi); issue 25 (assignee [0]) stays out.
func TestListAutopilotCandidateIssuesBotIDZeroGuardLiveDB(t *testing.T) {
	ctx := context.Background()
	q, repoID := apAssignmentFixture(ctx, t)

	got := assignmentCandidateIIDs(t, q, repoID, "autopilot", "uzi", 0)

	if len(got) != 1 || got[0] != 21 {
		t.Fatalf("candidates = %v, want [21] only: with BotID = 0 the @bot_id > 0 guard keeps the assignment branch off, so issue 25 (assignee [0]) must NOT match and only the uzi-label branch stands", got)
	}
}
