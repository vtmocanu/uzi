package store_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPlanningClaudeQueriesLiveDB(t *testing.T) {
	ctx, dsn := standHeadQueryDB(t, "planning_claude_")
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	q := store.New(pool)
	user, err := q.CreateUser(ctx, store.CreateUserParams{Email: fmt.Sprintf("planning-%s@example.com", uuid.New()), PasswordHash: pgtype.Text{String: "fixture", Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }

	got, err := q.GetUserPlanningClaude(ctx, user.ID)
	if err != nil || got.PlanningClaudeModel.Valid || got.PlanningClaudeEffort.Valid {
		t.Fatalf("fresh user = %+v, %v; want both NULL", got, err)
	}
	if _, err := q.SetUserPlanningClaude(ctx, store.SetUserPlanningClaudeParams{
		ID: user.ID, SetModel: true, PlanningClaudeModel: text("opus"), SetEffort: true, PlanningClaudeEffort: text("high"),
	}); err != nil {
		t.Fatal(err)
	}
	// Setting only the model keeps the effort.
	ret, err := q.SetUserPlanningClaude(ctx, store.SetUserPlanningClaudeParams{
		ID: user.ID, SetModel: true, PlanningClaudeModel: text("sonnet"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ret.PlanningClaudeModel.String != "sonnet" || ret.PlanningClaudeEffort.String != "high" {
		t.Fatalf("RETURNING pair = %+v/%+v, want sonnet/high", ret.PlanningClaudeModel, ret.PlanningClaudeEffort)
	}
	settings, err := q.GetUserSettings(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settings.PlanningClaudeModel.String != "sonnet" || settings.PlanningClaudeEffort.String != "high" {
		t.Fatalf("GetUserSettings pair = %+v/%+v, want sonnet/high", settings.PlanningClaudeModel, settings.PlanningClaudeEffort)
	}
	// A set flag with NULL clears that field only.
	if _, err := q.SetUserPlanningClaude(ctx, store.SetUserPlanningClaudeParams{ID: user.ID, SetEffort: true}); err != nil {
		t.Fatal(err)
	}
	got, err = q.GetUserPlanningClaude(ctx, user.ID)
	if err != nil || got.PlanningClaudeModel.String != "sonnet" || got.PlanningClaudeEffort.Valid {
		t.Fatalf("after clearing effort = %+v, %v", got, err)
	}
	// Neither flag set is a no-op write: set effort first so both fields hold a value.
	if _, err := q.SetUserPlanningClaude(ctx, store.SetUserPlanningClaudeParams{ID: user.ID, SetEffort: true, PlanningClaudeEffort: text("low")}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SetUserPlanningClaude(ctx, store.SetUserPlanningClaudeParams{ID: user.ID}); err != nil {
		t.Fatal(err)
	}
	got, err = q.GetUserPlanningClaude(ctx, user.ID)
	if err != nil || got.PlanningClaudeModel.String != "sonnet" || got.PlanningClaudeEffort.String != "low" {
		t.Fatalf("no-flag write changed the pair: %+v, %v", got, err)
	}
	// The planning pair is independent of the ordinary default_effort lane.
	if _, err := q.SetUserDefaultEffort(ctx, store.SetUserDefaultEffortParams{ID: user.ID, DefaultEffort: text("runtime")}); err != nil {
		t.Fatal(err)
	}
	got, err = q.GetUserPlanningClaude(ctx, user.ID)
	if err != nil || got.PlanningClaudeModel.String != "sonnet" {
		t.Fatalf("default_effort write changed the planning pair: %+v, %v", got, err)
	}
}
