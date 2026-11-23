package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexPlanningPairAtomicLiveDB(t *testing.T) {
	f := newFleetFixture(t)
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	set := func(model, effort string) error {
		_, err := f.q.SetUserPlanningCodex(f.ctx, store.SetUserPlanningCodexParams{
			ID: f.userID, SetModel: true, SetEffort: true,
			PlanningCodexModel: text(model), PlanningCodexEffort: text(effort),
		})
		return err
	}
	if err := set("gpt-6-astra", "high"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		<-start
		for i := 0; i < 40; i++ {
			model, effort := "gpt-6-astra", "high"
			if i%2 == 1 {
				model, effort = "gpt-6.1-sol", "medium"
			}
			if err := set(model, effort); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	go func() {
		<-start
		for i := 0; i < 40; i++ {
			pair, err := f.q.GetUserPlanningCodex(f.ctx, f.userID)
			if err != nil {
				done <- err
				return
			}
			valid := pair.PlanningCodexModel == text("gpt-6-astra") && pair.PlanningCodexEffort == text("high") ||
				pair.PlanningCodexModel == text("gpt-6.1-sol") && pair.PlanningCodexEffort == text("medium")
			if !valid {
				done <- fmt.Errorf("observed partial planning pair: %v / %v", pair.PlanningCodexModel, pair.PlanningCodexEffort)
				return
			}
		}
		done <- nil
	}()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}

	// A false presence flag preserves the row, even with a different argument; a true NULL clears.
	pair, err := f.q.SetUserPlanningCodex(f.ctx, store.SetUserPlanningCodexParams{
		ID: f.userID, SetModel: true, PlanningCodexEffort: text("low"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pair.PlanningCodexModel.Valid || pair.PlanningCodexEffort != text("medium") {
		t.Fatalf("presence/null update = %v / %v", pair.PlanningCodexModel, pair.PlanningCodexEffort)
	}
	if _, err := f.q.GetUserPlanningCodex(f.ctx, uuid.New()); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("strict missing-user read = %v, want pgx.ErrNoRows", err)
	}
}
