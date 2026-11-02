package workersvc

import (
	"testing"
	"time"
)

// Destination comparison values are admissible only for the Codex selector's
// allowed destination outcomes. Raw measured headroom remains separate.
func TestHarnessOverflowDestinationComparisonUnknownLiveDB(t *testing.T) {
	for _, scenario := range []string{"pool empty", "cooldown", "reserve held"} {
		t.Run(scenario, func(t *testing.T) {
			fx := newOverflowFix(t)
			switch scenario {
			case "pool empty":
				fx.env.exec("UPDATE user_secrets SET auto_eligible=false WHERE id=$1", fx.codex.alias)
			case "cooldown":
				fx.mode("default")
				fx.cooldown(fx.codex, time.Hour)
			case "reserve held":
				fx.reserve(20)
				fx.reading(fx.codex, 50)
			}
			run, err := fx.issue()
			if err != nil {
				t.Fatal(err)
			}
			if run.Harness != "claude" {
				t.Fatalf("ineligible Codex destination received overflow: %s", run.Harness)
			}
			if run.HarnessOtherScore.Valid {
				t.Fatalf("ineligible destination comparison must be unknown: scenario=%s comparison=%v measured=%v",
					scenario, run.HarnessOtherScore, run.HarnessOtherMeasured)
			}
			if !run.HarnessOtherMeasured.Valid {
				t.Fatal("measured headroom was discarded")
			}
		})
	}
}
