package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestScheduledMRStateWatchCandidatesLiveDB pins ListScheduledMRStateWatchCandidates (PRD
// #908) against a REAL Postgres. The board-free MR-state watch enumerates the scheduled
// lanes (kind IN ('prompt','self_improve')) whose MR is still transient so the recorder can
// keep runs.mr_state fresh; a run self-evicts from the set once its mr_state is terminal
// (merged/closed). The query is:
//
//	SELECT id, branch, mr_iid, mr_state FROM runs
//	WHERE repo_id=$1 AND kind IN ('prompt','self_improve') AND status='completed'
//	  AND mr_iid IS NOT NULL AND (mr_state IS NULL OR mr_state IN ('opened','locked'))
//	ORDER BY created_at DESC LIMIT 100
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-it sweep
// (e2e/run-store-it.sh, task gate:api in CI) provides one. `go test ./...` without it SKIPs.
// A package that prints `ok` with PASS=0 is INVALID, not green.
func TestScheduledMRStateWatchCandidatesLiveDB(t *testing.T) {
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

	owner, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("sched-mrstate-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/sched', 'https://forge.e2e/g/sched', 'main', true)`, repoID, connID)

	// seedRun inserts one completed scheduled-or-issue run. kind decides the shape:
	// prompt/task carry issue_iid NULL, issue/self_improve a non-null issue_iid (00167
	// runs_kind_shape). mrIID nil ⇒ SQL NULL mr_iid; mrState nil ⇒ SQL NULL mr_state.
	seedRun := func(kind, branch string, issueIID *int64, mrIID *int64, mrState *string, status string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
			 VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, $8, $9)`,
			id, owner, repoID, kind, issueIID, branch, mrIID, mrState, status)
		return id
	}
	sp := func(s string) *string { return &s }
	ip := func(n int64) *int64 { return &n }

	// INCLUDED — the transient-MR scheduled runs the recorder must keep polling:
	//   prompt, mr_iid set, mr_state NULL → the bootstrap candidate (first observation
	//   records without acting).
	promptBootstrap := seedRun("prompt", "uzi/prompt-"+uuid.New().String(), nil, ip(9101), nil, "completed")
	//   self_improve, mr_iid set, mr_state='opened' → transient, included.
	selfImpOpened := seedRun("self_improve", "uzi/self-improve/"+uuid.New().String(), ip(9102), ip(9102), sp("opened"), "completed")
	//   prompt, mr_state='locked' → still transient (mid-merge), included.
	promptLocked := seedRun("prompt", "uzi/prompt-"+uuid.New().String(), nil, ip(9103), sp("locked"), "completed")

	// EXCLUDED — one per gate:
	//   mr_state='merged' → terminal, self-eviction.
	seedRun("prompt", "uzi/prompt-"+uuid.New().String(), nil, ip(9104), sp("merged"), "completed")
	//   mr_state='closed' → terminal.
	seedRun("self_improve", "uzi/self-improve/"+uuid.New().String(), ip(9105), ip(9105), sp("closed"), "completed")
	//   kind='issue' with mr_iid + mr_state NULL → the kind filter drops it (issue runs are
	//   watched by the board-coupled Lane A, not this board-free lane).
	seedRun("issue", "agent/issue-9106", ip(9106), ip(9106), nil, "completed")
	//   mr_iid NULL → nothing to watch.
	seedRun("prompt", "uzi/prompt-"+uuid.New().String(), nil, nil, sp("opened"), "completed")
	//   status='running' → the run has not completed, no MR yet.
	seedRun("prompt", "uzi/prompt-"+uuid.New().String(), nil, ip(9108), sp("opened"), "running")

	cands, err := q.ListScheduledMRStateWatchCandidates(ctx, repoID)
	if err != nil {
		t.Fatalf("ListScheduledMRStateWatchCandidates: %v", err)
	}
	byID := map[uuid.UUID]store.ListScheduledMRStateWatchCandidatesRow{}
	for _, c := range cands {
		byID[c.ID] = c
	}
	wantIDs := []uuid.UUID{promptBootstrap, selfImpOpened, promptLocked}
	if len(cands) != len(wantIDs) {
		t.Fatalf("want exactly %d scheduled watch candidates (NULL/opened/locked), got %d: %+v", len(wantIDs), len(cands), cands)
	}
	for _, id := range wantIDs {
		if _, ok := byID[id]; !ok {
			t.Errorf("run %s must be a scheduled MR-state watch candidate, got set %+v", id, cands)
		}
	}
	// The projection carries branch/mr_iid/mr_state through untouched (the recorder needs
	// them to read the forge and detect the transition). Spot-check the bootstrap row's NULL
	// mr_state survives as an invalid pgtype.Text rather than being coerced.
	if bc := byID[promptBootstrap]; bc.MrState.Valid {
		t.Errorf("bootstrap candidate mr_state must project as NULL, got %+v", bc.MrState)
	}
	if oc := byID[selfImpOpened]; !oc.MrState.Valid || oc.MrState.String != "opened" || oc.MrIid.Int64 != 9102 {
		t.Errorf("opened candidate must project mr_state='opened' + mr_iid=9102, got %+v", oc)
	}
}

// TestScheduledBranchConcurrencyGuardsLiveDB proves the create-time cross-kind branch guard
// and the per-MR uniqueness index BOTH hold for a SCHEDULED-lane branch (uzi/prompt-…),
// exactly as they do for an agent/issue-N branch — PRD #908's scheduled lanes reuse the same
// runs table, the same uq_runs_one_active_branch_ref spanning index, and the same
// uq_runs_one_active_mr_rework (repo_id, mr_iid) index.
//
// Mechanism exercised (store-level, both arms):
//
//   - CROSS-KIND branch guard: an active ci_fix run occupies pipeline_ref=<uzi/prompt branch>,
//     so CreateAutoMRReworkRun for that SAME ref matches zero rows through its atomic
//     INSERT … WHERE NOT EXISTS and the generated :one returns pgx.ErrNoRows (the caller maps
//     that to ErrBranchInUse). This is the sequential committed-path arm — no goroutine.
//   - PER-MR uniqueness: on a DIFFERENT scheduled branch with no cross-kind occupant, the
//     FIRST CreateAutoMRReworkRun succeeds; a SECOND for the same (repo_id, mr_iid) falls
//     THROUGH the (cross-kind-only) WHERE NOT EXISTS and is rejected by the
//     uq_runs_one_active_mr_rework partial unique index → pgconn 23505.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestScheduledBranchConcurrencyGuardsLiveDB(t *testing.T) {
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

	owner, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("sched-guard-%s@e2e", owner))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, owner, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/sguard', 'https://forge.e2e/g/sguard', 'main', true)`, repoID, connID)

	// A completed source prompt run (issue_iid NULL) with an opened MR on the branch, serving
	// as target_run_id (runs_kind_shape requires an mr_rework to carry a non-null target).
	occupiedBranch := "uzi/prompt-" + uuid.New().String()
	const occupiedMR int64 = 8600
	srcOccupied := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
		 VALUES ($1, $2, $3, 'prompt', NULL, 't', 'd', $4, $5, 'opened', 'completed')`,
		srcOccupied, owner, repoID, occupiedBranch, occupiedMR)

	// ── CROSS-KIND branch guard ──
	// An active ci_fix run occupies the scheduled branch. Its distinct mr_iid=99 means the
	// ONLY thing blocking the mr_rework insert is the occupied branch, not the same-MR index.
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_id, pipeline_ref, status)
		 VALUES ($1, $2, $3, 'ci_fix', 't', 'd', 4242, $4, 'running')`,
		uuid.New(), owner, repoID, occupiedBranch)
	if _, err := q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
		UserID:           owner,
		RepoID:           repoID,
		IssueTitle:       "Rework scheduled MR (occupied by ci_fix)",
		IssueDescription: "d",
		PipelineRef:      pgtype.Text{String: occupiedBranch, Valid: true},
		MrIid:            pgtype.Int8{Int64: occupiedMR, Valid: true},
		TargetRunID:      pgtype.UUID{Bytes: srcOccupied, Valid: true},
		ReviewComments:   []byte(`{"comments":[{"id":1}],"truncated":false}`),
		WaitOnLimit:      false,
	}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("CreateAutoMRReworkRun on a ci_fix-occupied scheduled branch: err = %v, want pgx.ErrNoRows (cross-kind WHERE NOT EXISTS → 0 rows)", err)
	}

	// ── PER-MR uniqueness ──
	// A fresh scheduled branch with no cross-kind occupant. The first create succeeds; the
	// second, same (repo_id, mr_iid), is rejected by uq_runs_one_active_mr_rework.
	freeBranch := "uzi/prompt-" + uuid.New().String()
	const freeMR int64 = 8700
	srcFree := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
		 VALUES ($1, $2, $3, 'prompt', NULL, 't', 'd', $4, $5, 'opened', 'completed')`,
		srcFree, owner, repoID, freeBranch, freeMR)

	first, err := q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
		UserID:           owner,
		RepoID:           repoID,
		IssueTitle:       "Rework scheduled MR",
		IssueDescription: "d",
		PipelineRef:      pgtype.Text{String: freeBranch, Valid: true},
		MrIid:            pgtype.Int8{Int64: freeMR, Valid: true},
		TargetRunID:      pgtype.UUID{Bytes: srcFree, Valid: true},
		ReviewComments:   []byte(`{"comments":[{"id":2}],"truncated":false}`),
		WaitOnLimit:      false,
	})
	if err != nil {
		t.Fatalf("first CreateAutoMRReworkRun on a free scheduled branch: %v", err)
	}
	if first.Kind != "mr_rework" || !first.AutoApprove {
		t.Fatalf("mr_rework run shape wrong: kind=%q auto_approve=%v", first.Kind, first.AutoApprove)
	}

	_, err = q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
		UserID:           owner,
		RepoID:           repoID,
		IssueTitle:       "Rework scheduled MR (duplicate)",
		IssueDescription: "d",
		PipelineRef:      pgtype.Text{String: freeBranch, Valid: true},
		MrIid:            pgtype.Int8{Int64: freeMR, Valid: true},
		TargetRunID:      pgtype.UUID{Bytes: srcFree, Valid: true},
		ReviewComments:   []byte(`{"comments":[{"id":3}],"truncated":false}`),
		WaitOnLimit:      false,
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("second CreateAutoMRReworkRun (same MR) err = %v, want a 23505 unique violation", err)
	}
	if pgErr.ConstraintName != "uq_runs_one_active_mr_rework" {
		t.Fatalf("same-MR duplicate violated %q, want uq_runs_one_active_mr_rework", pgErr.ConstraintName)
	}
}
