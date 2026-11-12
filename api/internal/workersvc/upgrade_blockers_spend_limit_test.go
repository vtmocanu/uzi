package workersvc

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3767: the spend-limit hold is explained as an owner-held hold with its ways out,
// never as an automatic retry. set-token is offered only on switchable lanes.
func TestUpgradeBlockerAnthropicSpendLimit(t *testing.T) {
	row := func(kind string) store.ListWorkerUpgradeBlockersRow {
		return store.ListWorkerUpgradeBlockersRow{
			Kind: blockerText(kind), Status: blockerText("recovery_wait"), RecoveryWaitCause: blockerText("anthropic_spend_limit"),
		}
	}
	got := blockerAction(row("issue"))
	for _, want := range []string{"anthropic_spend_limit", "Anthropic API spend limit reached", "raise the limit and Resume, set-token, or Cancel", "no timer"} {
		if !strings.Contains(got, want) {
			t.Fatalf("blocker %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "automatic retry") {
		t.Fatalf("blocker promises an automatic retry: %q", got)
	}
	for _, kind := range []string{"task", "prompt", "ci_fix", "mr_rework"} {
		if g := blockerAction(row(kind)); !strings.Contains(g, "set-token") {
			t.Fatalf("switchable kind %s lacks set-token: %q", kind, g)
		}
	}
	for _, kind := range []string{"chat", "judge", "self_improve", "job"} {
		g := blockerAction(row(kind))
		if strings.Contains(g, "set-token") {
			t.Fatalf("non-switchable kind %s offers set-token: %q", kind, g)
		}
		if !strings.Contains(g, "raise the limit and Resume, or Cancel; no timer") {
			t.Fatalf("non-switchable kind %s lacks the reduced action: %q", kind, g)
		}
	}
}
