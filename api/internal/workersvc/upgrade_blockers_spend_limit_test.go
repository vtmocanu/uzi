package workersvc

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3767: the spend-limit hold is explained as an owner-held hold with its three ways out,
// never as an automatic retry.
func TestUpgradeBlockerAnthropicSpendLimit(t *testing.T) {
	got := blockerAction(store.ListWorkerUpgradeBlockersRow{
		Status: blockerText("recovery_wait"), RecoveryWaitCause: blockerText("anthropic_spend_limit"),
	})
	for _, want := range []string{"anthropic_spend_limit", "Anthropic API spend limit reached", "raise the limit and Resume, set-token, or Cancel", "no timer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("blocker %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "automatic retry") {
		t.Fatalf("blocker promises an automatic retry: %q", got)
	}
}
