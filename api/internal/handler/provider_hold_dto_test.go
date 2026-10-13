package handler

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1733: runToDTO surfaces the open provider-park streak and derives can_stop_at_provider_hold
// (a paused provider hold on a non-interactive issue run with a completed milestone).
func TestRunToDTOProviderHold(t *testing.T) {
	started := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	hold := func(mutate func(*store.Run)) store.Run {
		r := store.Run{
			Kind: "issue", Status: "paused",
			HoldReason:                  pgtype.Text{String: "provider_unavailable", Valid: true},
			MilestonesCompleted:         []byte(`["m1"]`),
			ProviderParkCount:           4,
			ProviderParkStreakStartedAt: pgtype.Timestamptz{Time: started, Valid: true},
		}
		if mutate != nil {
			mutate(&r)
		}
		return r
	}

	dto := runToDTO(hold(nil), "normal", 0, 0, 0, dtoTestNow, 0)
	if dto.ProviderParkCount != 4 || dto.ProviderParkStreakStartedAt == nil || !dto.ProviderParkStreakStartedAt.Equal(started) {
		t.Fatalf("streak: count=%d start=%v, want 4/%v", dto.ProviderParkCount, dto.ProviderParkStreakStartedAt, started)
	}
	if !dto.CanStopAtProviderHold {
		t.Fatal("can_stop_at_provider_hold = false, want true for a hold with a completed milestone")
	}
	if dto.CanStopAtWall {
		t.Fatal("can_stop_at_wall = true on a provider hold, want false")
	}

	for name, mutate := range map[string]func(*store.Run){
		"no completed milestone": func(r *store.Run) { r.MilestonesCompleted = []byte(`[]`) },
		"interactive run":        func(r *store.Run) { r.Interactive = true },
		"task run":               func(r *store.Run) { r.Kind = "task" },
		"other hold reason":      func(r *store.Run) { r.HoldReason = pgtype.Text{String: "budget_exhausted", Valid: true} },
		"not paused":             func(r *store.Run) { r.Status = "queued" },
	} {
		if runToDTO(hold(mutate), "normal", 0, 0, 0, dtoTestNow, 0).CanStopAtProviderHold {
			t.Errorf("%s: can_stop_at_provider_hold = true, want false", name)
		}
	}

	none := runToDTO(store.Run{Kind: "issue", Status: "running"}, "normal", 0, 0, 0, dtoTestNow, 0)
	if none.ProviderParkCount != 0 || none.ProviderParkStreakStartedAt != nil {
		t.Fatalf("no streak: count=%d start=%v, want 0/nil", none.ProviderParkCount, none.ProviderParkStreakStartedAt)
	}
}
