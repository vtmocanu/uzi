package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestScheduleCapacityConstraintLiveDB(t *testing.T) {
	ctx := context.Background()
	q, owner, repo := schedFixture(ctx, t)
	assertResult := func(err error, valid bool) {
		t.Helper()
		if valid {
			if err != nil {
				t.Fatalf("valid capacity rejected: %v", err)
			}
			return
		}
		var pgerr *pgconn.PgError
		if !errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != "run_schedules_capacity_check" {
			t.Fatalf("want capacity CHECK violation (23514), got %v", err)
		}
	}
	for _, tc := range []struct {
		name  string
		c, k  pgtype.Int4
		valid bool
	}{
		{"both null", pgtype.Int4{}, pgtype.Int4{}, true},
		{"room null", pgtype.Int4{Int32: 4, Valid: true}, pgtype.Int4{}, false},
		{"limit null", pgtype.Int4{}, pgtype.Int4{Int32: 2, Valid: true}, false},
		{"zero room", pgtype.Int4{Int32: 4, Valid: true}, pgtype.Int4{Int32: 0, Valid: true}, false},
		{"zero limit", pgtype.Int4{Int32: 0, Valid: true}, pgtype.Int4{Int32: 1, Valid: true}, false},
		{"inverted", pgtype.Int4{Int32: 1, Valid: true}, pgtype.Int4{Int32: 2, Valid: true}, false},
		{"over bound", pgtype.Int4{Int32: 51, Valid: true}, pgtype.Int4{Int32: 2, Valid: true}, false},
		{"lower boundary", pgtype.Int4{Int32: 1, Valid: true}, pgtype.Int4{Int32: 1, Valid: true}, true},
		{"upper boundary", pgtype.Int4{Int32: 50, Valid: true}, pgtype.Int4{Int32: 50, Valid: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := store.CreateRunScheduleParams{UserID: owner, RepoID: repo, Target: "sweep", Labels: []byte(`["uzi"]`), Timing: "recurring", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC", CapacityLimit: tc.c, CapacityRoomNeeded: tc.k}
			row, err := q.CreateRunSchedule(ctx, params)
			assertResult(err, tc.valid)
			if tc.valid && (row.CapacityLimit != tc.c || row.CapacityRoomNeeded != tc.k) {
				t.Fatal("insert lost capacity columns")
			}

			params.CapacityLimit = pgtype.Int4{Int32: 4, Valid: true}
			params.CapacityRoomNeeded = pgtype.Int4{Int32: 2, Valid: true}
			original, err := q.CreateRunSchedule(ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			updated, err := q.UpdateRunSchedule(ctx, store.UpdateRunScheduleParams{
				ID: original.ID, UserID: owner, RepoID: repo, Target: "sweep",
				Labels: params.Labels, Timing: "recurring", CronExpr: params.CronExpr, Timezone: "UTC",
				CapacityLimit: tc.c, CapacityRoomNeeded: tc.k,
			})
			assertResult(err, tc.valid)
			if tc.valid {
				if updated.CapacityLimit != tc.c || updated.CapacityRoomNeeded != tc.k {
					t.Fatal("update lost capacity columns")
				}
			} else {
				retained, err := q.GetRunSchedule(ctx, original.ID)
				if err != nil {
					t.Fatal(err)
				}
				if retained.CapacityLimit != original.CapacityLimit || retained.CapacityRoomNeeded != original.CapacityRoomNeeded {
					t.Fatal("rejected update changed the gate")
				}
			}
		})
	}
}
