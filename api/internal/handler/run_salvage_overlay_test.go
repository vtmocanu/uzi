package handler

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1867 M4: the salvage overlay maps a run_salvage row onto the run-detail DTO; no row
// and a lookup error both leave every salvage field null, and landing_state is untouched.
func TestApplyRunSalvage(t *testing.T) {
	run := uuid.New()
	exp := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	tip := "89abcdef0123456789abcdef0123456789abcdef"
	row := func(state string, created bool, lastErr string) store.RunSalvage {
		r := store.RunSalvage{RunID: run, State: state, Tip: tip, Branch: "agent/issue-7"}
		if created {
			r.ExpiresAt = pgtype.Timestamptz{Time: exp, Valid: true}
		}
		if lastErr != "" {
			r.LastError = pgtype.Text{String: lastErr, Valid: true}
		}
		return r
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	t.Run("promoted row", func(t *testing.T) {
		dto := apitypes.RunDTO{LandingState: "none"}
		applyRunSalvage(&dto, row("promoted", true, ""), nil)
		if str(dto.SalvageState) != "promoted" || str(dto.SalvageRef) != "refs/uzi-salvage/"+run.String() ||
			str(dto.SalvageTip) != tip || dto.SalvageExpiresAt == nil || !dto.SalvageExpiresAt.Equal(exp) ||
			dto.SalvageLastError != nil || dto.LandingState != "none" {
			t.Fatalf("promoted overlay = state %s ref %s tip %s exp %v err %s landing %q",
				str(dto.SalvageState), str(dto.SalvageRef), str(dto.SalvageTip), dto.SalvageExpiresAt, str(dto.SalvageLastError), dto.LandingState)
		}
	})

	// Every non-promoted state carries no salvage_ref (no copy exists, or it has expired),
	// and never the branch checkpoint ref.
	for _, state := range []string{"pending", "unavailable", "refused", "failed", "skipped_secret", "expired", "disabled"} {
		t.Run(state+" row", func(t *testing.T) {
			var dto apitypes.RunDTO
			applyRunSalvage(&dto, row(state, state == "expired", "forge said no"), nil)
			if str(dto.SalvageState) != state || dto.SalvageRef != nil || str(dto.SalvageTip) != tip ||
				str(dto.SalvageLastError) != "forge said no" || (dto.SalvageExpiresAt != nil) != (state == "expired") {
				t.Fatalf("%s overlay = state %s ref %s tip %s exp %v err %s",
					state, str(dto.SalvageState), str(dto.SalvageRef), str(dto.SalvageTip), dto.SalvageExpiresAt, str(dto.SalvageLastError))
			}
		})
	}

	for name, err := range map[string]error{"no row": pgx.ErrNoRows, "lookup error": errors.New("db down")} {
		t.Run(name, func(t *testing.T) {
			dto := apitypes.RunDTO{LandingState: "needs_landing"}
			applyRunSalvage(&dto, row("promoted", true, "x"), err)
			if dto.SalvageState != nil || dto.SalvageRef != nil || dto.SalvageTip != nil || dto.SalvageExpiresAt != nil ||
				dto.SalvageLastError != nil || dto.LandingState != "needs_landing" {
				t.Fatalf("%s must leave every salvage field null and landing_state unchanged: %+v", name, dto)
			}
		})
	}
}
