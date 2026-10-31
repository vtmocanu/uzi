package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3708: the run snapshot rides the DTO as plan_cross_check_gated, omitted when false.
func TestRunToDTOPlanCrossCheckGated(t *testing.T) {
	for _, gated := range []bool{false, true} {
		dto := runToDTO(store.Run{ID: uuid.New(), Status: "awaiting_approval", PlanCrossCheckGated: gated}, "normal", 0, 0, 0, dtoTestNow, 0)
		if dto.PlanCrossCheckGated != gated {
			t.Fatalf("gated=%v mapped to %v", gated, dto.PlanCrossCheckGated)
		}
		wire, err := json.Marshal(dto)
		if err != nil {
			t.Fatal(err)
		}
		if has := strings.Contains(string(wire), `"plan_cross_check_gated":true`); has != gated {
			t.Fatalf("gated=%v wire has key=%v", gated, has)
		}
		if !gated && strings.Contains(string(wire), "plan_cross_check_gated") {
			t.Fatal("false must omit the key so an older client sees nothing new")
		}
		if dto.PlanCrossCheckRequired {
			t.Fatal("a gated run is not a required run")
		}
	}
}
