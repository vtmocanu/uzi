package forgesvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSyncPipelinesEmptyKeepSetEvictsLiveDB pins the reconcile eviction against real
// Postgres when NOTHING is kept (default branch has no CI, no watched run refs). The
// keep-set must reach the DELETEs as '{}', not as a nil slice: pgx encodes nil as SQL
// NULL, `ref <> ALL(NULL)` is NULL for every row, and the stale badge and ci_autofix
// ledger rows would silently survive. A fake store cannot see that, so this reads both
// tables back. A second repo's rows are the control: eviction stays repo-scoped.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// runs it.
func TestSyncPipelinesEmptyKeepSetEvictsLiveDB(t *testing.T) {
	pool, q := closeReopenLiveDB(t)
	ctx := context.Background()
	repoID := seedCloseReopenRepo(ctx, t, pool)
	otherRepoID := seedCloseReopenRepo(ctx, t, pool)

	const staleRef = "agent/issue-7"
	for _, id := range []uuid.UUID{repoID, otherRepoID} {
		for _, ref := range []string{"main", staleRef} {
			if _, err := q.UpsertPipelineStatus(ctx, store.UpsertPipelineStatusParams{
				RepoID: id, Ref: ref, PipelineID: 1, Sha: "sha", Status: "failed", WebUrl: "https://forge.e2e/p/1",
				ForgeUpdatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
			}); err != nil {
				t.Fatalf("seed pipeline status %s: %v", ref, err)
			}
		}
		if err := q.UpsertCIAutofixAttempt(ctx, store.UpsertCIAutofixAttemptParams{RepoID: id, Ref: staleRef}); err != nil {
			t.Fatalf("seed ci_autofix attempt: %v", err)
		}
	}

	// Default branch has no CI (no scripted pipeline → ErrNoPipeline) and the repo has
	// no runs, so the reconcile keep-set is empty.
	svc := New(q, nil, time.Second, nil)
	if err := svc.SyncPipelines(ctx, repoID, 100, &fakeForge{}, syncOpts(true)); err != nil {
		t.Fatalf("SyncPipelines: %v", err)
	}

	for _, ref := range []string{"main", staleRef} {
		if _, err := q.GetPipelineStatusByRef(ctx, store.GetPipelineStatusByRefParams{RepoID: repoID, Ref: ref}); !errors.Is(err, pgx.ErrNoRows) {
			t.Errorf("pipeline status %q must be evicted by an empty keep-set, got err=%v", ref, err)
		}
		if _, err := q.GetPipelineStatusByRef(ctx, store.GetPipelineStatusByRefParams{RepoID: otherRepoID, Ref: ref}); err != nil {
			t.Errorf("another repo's pipeline status %q must survive, got %v", ref, err)
		}
	}
	if _, err := q.GetCIAutofixAttempt(ctx, store.GetCIAutofixAttemptParams{RepoID: repoID, Ref: staleRef}); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("ci_autofix ledger row must be evicted by an empty keep-set, got err=%v", err)
	}
	if _, err := q.GetCIAutofixAttempt(ctx, store.GetCIAutofixAttemptParams{RepoID: otherRepoID, Ref: staleRef}); err != nil {
		t.Errorf("another repo's ci_autofix ledger row must survive, got %v", err)
	}
}
