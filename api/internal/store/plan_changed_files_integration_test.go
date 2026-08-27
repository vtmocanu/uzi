package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #212 against a REAL Postgres. SetRunAwaitingApproval persists the plan-turn
// changed-file list via `plan_changed_files = COALESCE(narg('plan_changed_files')::text[],
// plan_changed_files)`. The COALESCE tri-state is the whole point of Decision 4 and is
// only knowable by executing it against Postgres's own array/NULL semantics — sqlc's type
// deduction and go build both pass on a statement whose runtime behavior differs.
//
// The three transitions asserted here, in order:
//   - nil-preserves:      a nil []string param encodes SQL NULL, COALESCE keeps the column.
//   - non-empty-replaces: a fresh non-nil list REPLACES the column with this round's set.
//   - empty-clears:       a non-nil EMPTY []string{} encodes '{}'::text[] (NOT NULL), so
//     COALESCE picks it and the column becomes empty — NOT the prior
//     list. This is the transition that fails if the implementer used
//     the required_tools empty->nil collapse (a revert-between-rounds
//     would then show a stale round-1 list); it is the load-bearing case.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the store-IT
// runner (e2e/run-store-it.sh) provides one.
func TestSetRunAwaitingApprovalPlanChangedFilesTriStateLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	f, done := setupAwaitingInput(ctx, t, dsn)
	defer done()

	set := func(t *testing.T, files []string) {
		t.Helper()
		rows, err := f.q.SetRunAwaitingApproval(ctx, store.SetRunAwaitingApprovalParams{
			PlanMd: pgT("# plan"), ID: f.runID, WorkerID: pgU(f.workerID),
			PlanChangedFiles: files,
		})
		if err != nil || rows != 1 {
			t.Fatalf("SetRunAwaitingApproval: rows=%d err=%v", rows, err)
		}
	}
	get := func(t *testing.T) []string {
		t.Helper()
		run, err := f.q.GetRunByID(ctx, f.runID)
		if err != nil {
			t.Fatalf("GetRunByID: %v", err)
		}
		return run.PlanChangedFiles
	}
	equal := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// Round 1: set a non-empty list.
	listA := []string{" M src/app.ts", "?? notes.md"}
	set(t, listA)
	if got := get(t); !equal(got, listA) {
		t.Fatalf("after set A: plan_changed_files = %v, want %v", got, listA)
	}

	// nil-preserves: a nil param (pre-#212 worker / report omitting the key) must NOT
	// disturb the column — it encodes SQL NULL and COALESCE keeps the existing list.
	set(t, nil)
	if got := get(t); !equal(got, listA) {
		t.Fatalf("nil-preserves failed: plan_changed_files = %v, want the prior %v (a nil param must COALESCE-preserve)", got, listA)
	}

	// non-empty-replaces: a different non-nil list REPLACES the column with this round's set.
	listB := []string{"A  x.go"}
	set(t, listB)
	if got := get(t); !equal(got, listB) {
		t.Fatalf("non-empty-replaces failed: plan_changed_files = %v, want %v", got, listB)
	}

	// empty-clears: a non-nil EMPTY slice must CLEAR the column to an empty array, not
	// preserve the prior list. '{}'::text[] is not NULL, so COALESCE picks it. This is the
	// case the required_tools empty->collapse would break (Decision 4).
	set(t, []string{})
	if got := get(t); len(got) != 0 {
		t.Fatalf("empty-clears failed: plan_changed_files = %v, want empty {} (a non-nil empty slice must REPLACE, not preserve)", got)
	}
}
