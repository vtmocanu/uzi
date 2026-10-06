package store_test

import (
	"context"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"testing"
)

func TestScheduleCapacityConstraintLiveDB(t *testing.T) {
	ctx := context.Background()
	q, owner, repo := schedFixture(ctx, t)
	for _, tc := range []struct {
		name  string
		c, k  pgtype.Int4
		valid bool
	}{
		{"both null", pgtype.Int4{}, pgtype.Int4{}, true},
		{"room null", pgtype.Int4{Int32: 4, Valid: true}, pgtype.Int4{}, false},
		{"limit null", pgtype.Int4{}, pgtype.Int4{Int32: 2, Valid: true}, false},
		{"zero", pgtype.Int4{Int32: 4, Valid: true}, pgtype.Int4{Int32: 0, Valid: true}, false},
		{"inverted", pgtype.Int4{Int32: 1, Valid: true}, pgtype.Int4{Int32: 2, Valid: true}, false},
		{"over bound", pgtype.Int4{Int32: 51, Valid: true}, pgtype.Int4{Int32: 2, Valid: true}, false},
		{"valid", pgtype.Int4{Int32: 50, Valid: true}, pgtype.Int4{Int32: 50, Valid: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{UserID: owner, RepoID: repo, Target: "sweep", Labels: []byte(`["uzi"]`), Timing: "recurring", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC", CapacityLimit: tc.c, CapacityRoomNeeded: tc.k})
			if (err == nil) != tc.valid {
				t.Fatalf("insert err=%v valid=%v", err, tc.valid)
			}
			if tc.valid && (row.CapacityLimit != tc.c || row.CapacityRoomNeeded != tc.k) {
				t.Fatal("columns lost")
			}
		})
	}
}
