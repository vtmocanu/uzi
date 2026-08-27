package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB coverage for ListAutopilotCandidateIssues' PRD-label predicate (PRD #102
// M6, Decision 11b).
//
// This is here rather than in a poller unit test because the predicate IS the SQL.
// The poller's fake store cannot filter — it returns whatever it was scripted with
// — so the most a unit test can pin there is that the resolved label reaches the
// query. Whether the query then excludes a non-PRD issue is a question only a real
// server answers.
//
// It is also the CLAUDE.md rule about sqlc: a green generate is not evidence the
// statement runs. This query gained a second jsonb_exists on a second named param,
// and until it has been prepared and executed against Postgres the change is
// unverified however green the ordinary gate looks.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// e2e/run-store-it.sh provides one.

// apCandidateFixture seeds one repo holding four issues that differ ONLY in their
// labels and state, so each assertion below isolates exactly one predicate.
//
// The fixture is built around the accident Decision 11b describes: issue 20 is a
// stranger's issue that carries the autopilot label and nothing else. Under the
// pre-M6 query it is a candidate, and the detector goes on to read its label events
// and either run it or comment on it. It is the row that must NOT come back.
func apCandidateFixture(ctx context.Context, t *testing.T) (*store.Queries, uuid.UUID) {
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
		userID, fmt.Sprintf("apcand-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', $3, $4, $5)`,
		connID, userID, "bot-apcand", 7001, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
		 VALUES ($1, $2, 7001, 'g/apcand', 'https://forge.e2e/g/apcand', true)`,
		repoID, connID)

	seed := func(iid int64, state, labels string) {
		mustExec(ctx, t, pool,
			`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
			 VALUES ($1, $2, 't', $3, $4::jsonb, 'https://x', true, now(), now())`,
			repoID, iid, state, labels)
	}
	seed(10, "opened", `["PRD","autopilot"]`) // the only candidate
	seed(20, "opened", `["autopilot"]`)       // Decision 11b's accident: not uzi's issue
	seed(30, "opened", `["PRD"]`)             // uzi's, but not autopiloted
	seed(40, "closed", `["PRD","autopilot"]`) // closed: autopilot drives open work only
	seed(70, "opened", `["bug","autopilot"]`) // PRD #196: run-eligible by a NON-PRIMARY label, but NOT the primary
	return store.New(pool), repoID
}

func candidateIIDs(t *testing.T, q *store.Queries, repoID uuid.UUID, autopilotLabel, prdLabel string) []int64 {
	t.Helper()
	rows, err := q.ListAutopilotCandidateIssues(context.Background(), store.ListAutopilotCandidateIssuesParams{
		RepoID:   repoID,
		Label:    autopilotLabel,
		PrdLabel: prdLabel,
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

func TestAutopilotCandidatesRequireBothLabelsLiveDB(t *testing.T) {
	ctx := context.Background()
	q, repoID := apCandidateFixture(ctx, t)

	got := candidateIIDs(t, q, repoID, "autopilot", "PRD")
	if len(got) != 1 || got[0] != 10 {
		t.Fatalf("candidates = %v, want [10] only; 20 is an autopilot-labelled issue that is NOT uzi's and must never reach the detector", got)
	}
}

// TestAutopilotCandidacyIgnoresRunEligibleSetPrimaryOnlyLiveDB is PRD #196 M4 guard
// test 2, live-DB half. PRD #196 widens the MANUAL run gate to accept any run-eligible
// label (default {PRD, bug}), but autopilot candidacy must keep matching the PRIMARY
// only (Decision 6): a bug-labelled issue carrying the autopilot label is run-eligible
// for a human, yet must NOT become an unattended autopilot candidate.
//
// The reason is in the name because the query is unchanged by design — this test
// exists so a later cleanup that threads the eligible set into the SQL is caught. Row
// 70 (bug+autopilot) is run-eligible by the non-primary "bug" but carries no primary,
// so it must be absent from the candidate set exactly as it was before PRD #196.
func TestAutopilotCandidacyIgnoresRunEligibleSetPrimaryOnlyLiveDB(t *testing.T) {
	ctx := context.Background()
	q, repoID := apCandidateFixture(ctx, t)

	got := candidateIIDs(t, q, repoID, "autopilot", "PRD")
	if len(got) != 1 || got[0] != 10 {
		t.Fatalf("candidates = %v, want [10] only; issue 70 (bug+autopilot) is run-eligible for a human but must NOT be an autopilot candidate — the query matches the PRIMARY, never the run-eligible set", got)
	}
}

// TestAutopilotCandidatesHonourARenamedPRDLabelLiveDB: prd_label is
// operator-configurable, so the predicate has to be the PARAM and not a literal.
// After a rename the issues carrying the old label stop being candidates and the
// ones carrying the new label start — which also means this test discriminates a
// hardcoded 'PRD' in the SQL, where the test above would not.
func TestAutopilotCandidatesHonourARenamedPRDLabelLiveDB(t *testing.T) {
	ctx := context.Background()
	q, repoID := apCandidateFixture(ctx, t)

	if got := candidateIIDs(t, q, repoID, "autopilot", "Feature"); len(got) != 0 {
		t.Fatalf("candidates = %v, want none: no issue carries the renamed label", got)
	}
}

// TestAutopilotCandidatesExcludeALabelLessIssueLiveDB covers the jsonb shapes the
// additive fetch newly makes reachable. A label-less issue reaches the cache as the
// jsonb array [] and, on rows written before the DTO fix, as the jsonb scalar null
// — which SQL NOT NULL does not exclude. Neither may satisfy the predicate, and
// jsonb_exists over a scalar null returns false rather than erroring, so both fail
// closed.
func TestAutopilotCandidatesExcludeALabelLessIssueLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	q, repoID := apCandidateFixture(ctx, t)
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	mustExec(ctx, t, pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, 50, 't', 'opened', '[]'::jsonb, 'https://x', true, now(), now()),
		        ($1, 60, 't', 'opened', 'null'::jsonb, 'https://x', true, now(), now())`,
		repoID)

	got := candidateIIDs(t, q, repoID, "autopilot", "PRD")
	if len(got) != 1 || got[0] != 10 {
		t.Fatalf("candidates = %v, want [10]: neither an empty array nor a jsonb null may satisfy the predicate", got)
	}
}
