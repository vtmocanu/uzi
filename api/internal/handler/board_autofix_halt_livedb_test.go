package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PRD #1650 D3a (M3): every card-returning response carries ci_autofix_halted and
// ci_autofix_attempts, read from the ledger row of the card's LATEST run's branch
// (ListCIAutofixHaltsForRepo), never through the pipeline cache. The board list is
// driven through SetBoardOrder (it serves buildBoard, the same payload GetBoard
// returns, without GetBoard's forge-backed column seeding); the single-card refreshes
// through MoveIssue and PromoteIssue.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// autofixCard is the slice of a card these tests read.
type autofixCard struct {
	IID      int64 `json:"iid"`
	Halted   *bool `json:"ci_autofix_halted"`
	Attempts *int  `json:"ci_autofix_attempts"`
}

// seedAutofixRun inserts an issue run on branch (created `ago` before now) and, when
// attempts >= 0, the branch's ledger row with the given halt latch.
func seedAutofixRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, owner, repoID uuid.UUID, iid int64, branch, ago string, attempts int, halted bool) {
	t.Helper()
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, branch, created_at)
		 VALUES ($1, $2, 'issue', $3, 't', 'd', 'completed', $4, now() - $5::interval)`,
		owner, repoID, iid, branch, ago)
	if attempts >= 0 {
		mustExecT(ctx, t, pool,
			`INSERT INTO ci_autofix_attempts (repo_id, ref, attempt_count, halt_notified) VALUES ($1, $2, $3, $4)`,
			repoID, branch, attempts, halted)
	}
}

func assertAutofix(t *testing.T, c autofixCard, wantHalted bool, wantAttempts int) {
	t.Helper()
	if c.Halted == nil || c.Attempts == nil {
		t.Fatalf("card %d missing ci_autofix_halted/ci_autofix_attempts keys: %+v", c.IID, c)
	}
	if *c.Halted != wantHalted || *c.Attempts != wantAttempts {
		t.Fatalf("card %d autofix = (halted %v, attempts %d), want (%v, %d)", c.IID, *c.Halted, *c.Attempts, wantHalted, wantAttempts)
	}
}

func decodeSingleAutofixCard(t *testing.T, rr *httptest.ResponseRecorder) autofixCard {
	t.Helper()
	var resp struct {
		Card autofixCard `json:"card"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode card: %v; body=%s", err, rr.Body.String())
	}
	return resp.Card
}

func TestBoardCardAutofixHaltLiveDB(t *testing.T) {
	ctx := context.Background()
	f := newBoardOrderFixture(ctx, t, 1, 2, 3, 4)

	// 1: newest run's branch halted, NO pipeline_statuses row at all.
	seedAutofixRun(ctx, t, f.pool, f.owner.ID, f.repoID, 1, "agent/issue-1", "1 hour", 2, true)
	// 2: ledger row present but not halted.
	seedAutofixRun(ctx, t, f.pool, f.owner.ID, f.repoID, 2, "agent/issue-2", "1 hour", 1, false)
	// 3: an OLDER run's branch is halted; the newest run is on another branch.
	seedAutofixRun(ctx, t, f.pool, f.owner.ID, f.repoID, 3, "agent/issue-3-old", "2 hours", 2, true)
	seedAutofixRun(ctx, t, f.pool, f.owner.ID, f.repoID, 3, "agent/issue-3", "1 hour", -1, false)
	// 4: never ran.

	w := f.call(t, f.owner, f.repoID, `{"iids":[1,2,3,4]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var resp struct {
		Board struct {
			Cards []autofixCard `json:"cards"`
		} `json:"board"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode board: %v (body %s)", err, w.Body.String())
	}
	byIID := map[int64]autofixCard{}
	for _, c := range resp.Board.Cards {
		byIID[c.IID] = c
	}
	if len(byIID) != 4 {
		t.Fatalf("board cards = %+v, want 4", resp.Board.Cards)
	}
	assertAutofix(t, byIID[1], true, 2)
	assertAutofix(t, byIID[2], false, 0)
	assertAutofix(t, byIID[3], false, 0)
	assertAutofix(t, byIID[4], false, 0)
}

func TestMoveIssueCarriesAutofixHaltLiveDB(t *testing.T) {
	ctx := context.Background()
	stub := &moveStub{}
	f := newMoveFixture(ctx, t, stub)
	mustExecT(ctx, t, f.pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, 310, 't', 'opened', '[]'::jsonb, 'https://x', false, now(), now())`, f.repoID)
	seedAutofixRun(ctx, t, f.pool, f.user.ID, f.repoID, 310, "agent/issue-310", "1 hour", 2, true)

	rr := httptest.NewRecorder()
	f.h.MoveIssue(rr, boardWriterReq(f.user, f.repoID, "310", `{"to_column":"Planned"}`))
	if rr.Code != http.StatusOK {
		t.Fatalf("move status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertAutofix(t, decodeSingleAutofixCard(t, rr), true, 2)
}

func TestPromoteIssueCarriesAutofixHaltLiveDB(t *testing.T) {
	ctx := context.Background()
	stub := &boardWriterStub{}
	f := newBoardWriterFixture(ctx, t, stub)
	mustExecT(ctx, t, f.pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		 VALUES ($1, 5, 't', 'opened', '["bug"]'::jsonb, 'https://x', false, now(), now())`, f.repoID)
	seedAutofixRun(ctx, t, f.pool, f.user.ID, f.repoID, 5, "agent/issue-5", "1 hour", 3, true)

	rr := httptest.NewRecorder()
	f.h.PromoteIssue(rr, boardWriterReq(f.user, f.repoID, "5", ""))
	if rr.Code != http.StatusOK {
		t.Fatalf("promote status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	assertAutofix(t, decodeSingleAutofixCard(t, rr), true, 3)
}
