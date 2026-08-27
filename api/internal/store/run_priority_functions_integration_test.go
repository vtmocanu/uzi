package store_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Truth table for the PRD #320 M1 priority functions against a REAL Postgres.
// fn_run_priority (the claim-ORDER-BY rank) and fn_run_priority_class (the
// display class) share ONE demotion predicate, so this asserts BOTH side by
// side over the same (kind, priority, is_stale) inputs in one loop — they can
// never be validated apart, which is the D1/D8 "one source" guarantee.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; the
// store e2e runner (e2e/run-store-it.sh) provides one.
func TestRunPriorityFunctionsTruthTableLiveDB(t *testing.T) {
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
	t.Cleanup(pool.Close)

	// priorityArg builds the nullable SMALLINT argument: nil override (NULL) or a
	// concrete value.
	null := func() pgtype.Int2 { return pgtype.Int2{} }
	val := func(v int16) pgtype.Int2 { return pgtype.Int2{Int16: v, Valid: true} }

	cases := []struct {
		name      string
		kind      string
		priority  pgtype.Int2
		isStale   bool
		wantRank  int16
		wantClass string
	}{
		{"judge_default_fresh", "judge", null(), false, 0, "background"},
		{"judge_default_stale", "judge", null(), true, 1, "restored"},
		{"self_improve_default_fresh", "self_improve", null(), false, 0, "background"},
		{"self_improve_default_stale", "self_improve", null(), true, 1, "restored"},
		{"issue_default", "issue", null(), false, 1, "normal"},
		{"ci_fix_default", "ci_fix", null(), false, 1, "normal"},
		{"prompt_default", "prompt", null(), false, 1, "normal"},
		{"issue_expedited", "issue", val(2), false, 2, "expedited"},
		{"judge_expedited", "judge", val(2), false, 2, "expedited"},
		{"judge_override_background", "judge", val(0), false, 0, "background"},
		{"issue_override_background", "issue", val(0), false, 0, "background"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var gotRank int16
			if err := pool.QueryRow(ctx,
				"SELECT fn_run_priority($1, $2, $3)", tc.kind, tc.priority, tc.isStale,
			).Scan(&gotRank); err != nil {
				t.Fatalf("fn_run_priority(%q, %v, %v): %v", tc.kind, tc.priority, tc.isStale, err)
			}
			if gotRank != tc.wantRank {
				t.Errorf("fn_run_priority(%q, %v, %v) = %d, want %d",
					tc.kind, tc.priority, tc.isStale, gotRank, tc.wantRank)
			}

			var gotClass string
			if err := pool.QueryRow(ctx,
				"SELECT fn_run_priority_class($1, $2, $3)", tc.kind, tc.priority, tc.isStale,
			).Scan(&gotClass); err != nil {
				t.Fatalf("fn_run_priority_class(%q, %v, %v): %v", tc.kind, tc.priority, tc.isStale, err)
			}
			if gotClass != tc.wantClass {
				t.Errorf("fn_run_priority_class(%q, %v, %v) = %q, want %q",
					tc.kind, tc.priority, tc.isStale, gotClass, tc.wantClass)
			}

			// The load-bearing cross-check: rank 0 must always read as background.
			if gotRank == 0 && gotClass != "background" {
				t.Errorf("rank 0 must read as background, got class %q", gotClass)
			}
		})
	}
}
