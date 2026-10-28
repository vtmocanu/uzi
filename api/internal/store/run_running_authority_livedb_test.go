package store_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestSetRunRunningOrdinaryAuthorityLiveDB pins optional-G compatibility and
// unsolicited-R refusal through the generated query, with SQL-NULL receipt.
// No accepted manifest is manufactured and no receipt trigger is bypassed.
func TestSetRunRunningOrdinaryAuthorityLiveDB(t *testing.T) {
	for _, status := range []string{"claimed", "running"} {
		t.Run(status, func(t *testing.T) {
			for _, name := range []string{"absent_G_absent_R", "correct_G_absent_R", "wrong_G", "unsolicited_R"} {
				t.Run(name, func(t *testing.T) {
					fx := newFleetFixture(t)
					worker := fx.worker("running-authority", nil, false)
					run := ownedRun(fx, worker, []string{})
					mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status=$2,claim_generation=7 WHERE id=$1`, run, status)
					p := store.SetRunRunningParams{
						ID: run, WorkerID: pgtype.UUID{Bytes: worker, Valid: true},
						RunMaxIterations: 5, RunTimeoutSeconds: 7200,
						MilestoneBudgetCap: 12, BudgetWallCeilingSeconds: 28800, SizeBudgetFactorL: 5,
					}
					want := int64(1)
					switch name {
					case "correct_G_absent_R":
						p.ClaimGeneration = pgtype.Int8{Int64: 7, Valid: true}
					case "wrong_G":
						p.ClaimGeneration = pgtype.Int8{Int64: 6, Valid: true}
						want = 0
					case "unsolicited_R":
						p.ClaimGeneration = pgtype.Int8{Int64: 7, Valid: true}
						p.PolicyStopResolutionID = pgtype.Text{String: "11111111-1111-4111-8111-111111111111", Valid: true}
						want = 0
					}
					snapshot := func() string {
						t.Helper()
						var row string
						if err := fx.pool.QueryRow(fx.ctx, `SELECT to_jsonb(r)::text FROM runs r WHERE id=$1`, run).Scan(&row); err != nil {
							t.Fatal(err)
						}
						return row
					}
					before := snapshot()
					rows, err := fx.q.SetRunRunning(fx.ctx, p)
					if err != nil || rows != want {
						t.Fatalf("ordinary running authority %s: rows=%d err=%v, want %d and nil", name, rows, err, want)
					}
					if want == 0 && before != snapshot() {
						t.Fatal("ordinary running refusal changed entire run row including status/clocks")
					}
					if want == 1 {
						var persisted string
						if err := fx.pool.QueryRow(fx.ctx, `SELECT status FROM runs WHERE id=$1`, run).Scan(&persisted); err != nil {
							t.Fatal(err)
						}
						if persisted != "running" {
							t.Fatalf("ordinary running authority status=%q, want running", persisted)
						}
					}
				})
			}
		})
	}
}
