package store_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
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
// Issue #1543 added an eligibility predicate (uzi label OR bot assignment) that both
// queries apply before the LIMIT, and turned the count into an {Eligible, Matched} row.
// The fixture's 46-49 rows exercise it under a non-uzi ["bug"] selector.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.

const (
	sweepAssignBotID   int64 = 6060 // the uzi-bot's forge user id
	sweepAssignHumanID int64 = 7070 // a different (human) assignee id
	// sweepAssignUziLabel is the eligibility label the helpers pass as @uzi_label.
	sweepAssignUziLabel = "uzi"
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
	// 46-49 carry a non-uzi "bug" selector label (issue #1543 eligibility-before-LIMIT):
	// 46: bug + uzi, not assigned                   → eligible via the uzi label.
	seed(46, "opened", `["bug","uzi"]`, `[]`)
	// 47: bug, bot-assigned, no uzi label           → eligible via bot assignment.
	seed(47, "opened", `["bug"]`, fmt.Sprintf("[%d]", sweepAssignBotID))
	// 48: bug only, unassigned                      → selector match, NOT eligible.
	seed(48, "opened", `["bug"]`, `[]`)
	// 49: bug, human-assigned only                  → selector match, NOT eligible (numeric id).
	seed(49, "opened", `["bug"]`, fmt.Sprintf("[%d]", sweepAssignHumanID))
	return store.New(pool), repoID
}

func sweepAssignedIIDs(t *testing.T, q *store.Queries, repoID uuid.UUID, selector string, labels []byte, botID int64) []int64 {
	t.Helper()
	rows, err := q.ListSweepCandidateIssues(context.Background(), store.ListSweepCandidateIssuesParams{
		RepoID:   repoID,
		Selector: selector,
		Labels:   labels,
		BotID:    botID,
		UziLabel: sweepAssignUziLabel,
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

func sweepAssignedCount(t *testing.T, q *store.Queries, repoID uuid.UUID, selector string, labels []byte, botID int64) store.CountSweepCandidateIssuesRow {
	t.Helper()
	row, err := q.CountSweepCandidateIssues(context.Background(), store.CountSweepCandidateIssuesParams{
		RepoID:   repoID,
		Selector: selector,
		Labels:   labels,
		BotID:    botID,
		UziLabel: sweepAssignUziLabel,
	})
	if err != nil {
		t.Fatalf("CountSweepCandidateIssues: %v", err)
	}
	return row
}

// TestListSweepCandidateIssuesAssignedSelectorLiveDB asserts the exact candidate set for the
// assigned selector with the real numeric bot id. It fails against pre-change code, where
// ListSweepCandidateIssues had no Selector/BotID params and matched only by label, so an
// unlabelled bot-assigned issue (41) could never be a candidate.
func TestListSweepCandidateIssuesAssignedSelectorLiveDB(t *testing.T) {
	ctx := context.Background()
	q, repoID := sweepAssignmentFixture(ctx, t)

	// Assigned selector, real bot id: the three OPEN bot-assigned issues, in iid order.
	got := sweepAssignedIIDs(t, q, repoID, "assigned", []byte("[]"), sweepAssignBotID)
	if !reflect.DeepEqual(got, []int64{41, 42, 47}) {
		t.Fatalf("assigned candidates = %v, want [41 42 47]:\n"+
			"  41 bot-assigned, no uzi       → the NEW assigned candidate\n"+
			"  42 bot-assigned + uzi         → assigned candidate\n"+
			"  43 human-assigned             → NOT (numeric bot id must match — jsonb trap guard)\n"+
			"  44 uzi label only             → NOT (not assigned)\n"+
			"  45 bot-assigned CLOSED        → NOT (state gate)\n"+
			"  47 bot-assigned, bug label    → assigned candidate (labels ignored)", got)
	}
	// The assigned selector is eligible by construction: every match counts as eligible.
	if c := sweepAssignedCount(t, q, repoID, "assigned", []byte("[]"), sweepAssignBotID); c.Eligible != 3 || c.Matched != 3 {
		t.Fatalf("assigned count = %+v, want {Eligible:3 Matched:3} (matches the list; assigned is always eligible)", c)
	}

	// BotID = 0: the assigned branch's `@bot_id > 0` guard keeps it off — nothing matches,
	// even though issue 45's assignee list is non-empty.
	if got := sweepAssignedIIDs(t, q, repoID, "assigned", []byte("[]"), 0); len(got) != 0 {
		t.Fatalf("assigned candidates with BotID=0 = %v, want none (the @bot_id > 0 guard)", got)
	}
	if c := sweepAssignedCount(t, q, repoID, "assigned", []byte("[]"), 0); c.Eligible != 0 || c.Matched != 0 {
		t.Fatalf("assigned count with BotID=0 = %+v, want {Eligible:0 Matched:0}", c)
	}
}

// TestListSweepCandidateIssuesLabelSelectorMatchVsEligibilityLiveDB separates the label
// selector's two filters (issue #1543). Selector MATCHING is label containment over @labels;
// ELIGIBILITY (uzi label OR bot assignment) is a second predicate applied before the LIMIT.
// A selector match that is not eligible is dropped from the list but still counted in
// Matched; an issue that does not match the selector is in neither, however eligible it is.
func TestListSweepCandidateIssuesLabelSelectorMatchVsEligibilityLiveDB(t *testing.T) {
	ctx := context.Background()
	q, repoID := sweepAssignmentFixture(ctx, t)

	// Non-uzi selector ["bug"]: 46-49 match; 46 (uzi) and 47 (bot-assigned) are eligible.
	bug := []byte(`["bug"]`)
	if got := sweepAssignedIIDs(t, q, repoID, "label", bug, sweepAssignBotID); !reflect.DeepEqual(got, []int64{46, 47}) {
		t.Fatalf("bug-selector candidates = %v, want [46 47]:\n"+
			"  46 bug + uzi              → eligible (uzi label)\n"+
			"  47 bug + bot-assigned     → eligible (bot assignment)\n"+
			"  48 bug only               → NOT (selector match, ineligible)\n"+
			"  49 bug + human-assigned   → NOT (selector match, ineligible)\n"+
			"  41 bot-assigned, no bug   → NOT (does not match the selector)", got)
	}
	if c := sweepAssignedCount(t, q, repoID, "label", bug, sweepAssignBotID); c.Eligible != 2 || c.Matched != 4 {
		t.Fatalf("bug-selector count = %+v, want {Eligible:2 Matched:4} (48, 49 ineligible; 41 not matched)", c)
	}

	// Same selector with BotID = 0: bot assignment no longer confers eligibility, so 47 drops
	// out of the list but stays a selector match.
	if got := sweepAssignedIIDs(t, q, repoID, "label", bug, 0); !reflect.DeepEqual(got, []int64{46}) {
		t.Fatalf("bug-selector candidates with BotID=0 = %v, want [46] (the @bot_id > 0 guard)", got)
	}
	if c := sweepAssignedCount(t, q, repoID, "label", bug, 0); c.Eligible != 1 || c.Matched != 4 {
		t.Fatalf("bug-selector count with BotID=0 = %+v, want {Eligible:1 Matched:4}", c)
	}

	// Default ["uzi"] selector: every match is eligible by its uzi label, so the result is the
	// uzi-labelled OPEN issues; 41 (bot-assigned, unlabelled) is out because it does not match
	// the selector, not because it is ineligible.
	uzi := []byte(`["uzi"]`)
	if got := sweepAssignedIIDs(t, q, repoID, "label", uzi, sweepAssignBotID); !reflect.DeepEqual(got, []int64{42, 44, 46}) {
		t.Fatalf("uzi-selector candidates = %v, want [42 44 46] (41 does not match the selector)", got)
	}
	if c := sweepAssignedCount(t, q, repoID, "label", uzi, sweepAssignBotID); c.Eligible != 3 || c.Matched != 3 {
		t.Fatalf("uzi-selector count = %+v, want {Eligible:3 Matched:3}", c)
	}
}
