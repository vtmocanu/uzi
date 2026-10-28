package apitypes

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAccountCooldownContract(t *testing.T) {
	class := AccountCooldownClass("rate_limit")
	at := time.Date(2026, 10, 28, 10, 11, 12, 0, time.UTC)
	item := RunListItemDTO{RunDTO: RunDTO{Status: "running", Health: "waiting_account_cooldown", AccountCooldownClass: &class, AccountCooldownEligibleAt: &at}}
	summary := RunSummaryOf(item)
	// The CLI restores the same fields from its summary endpoint.
	restored := summary.ListItem()
	data, err := json.Marshal(restored)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"account_cooldown_class":"rate_limit"`, `"account_cooldown_eligible_at":"2026-10-28T10:11:12Z"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing %s", want)
		}
	}
	if got := AccountCooldownLine(class, at); got != "waiting for account cooldown (rate_limit); earliest eligible at 2026-10-28T10:11:12Z" {
		t.Fatal(got)
	}
	if AccountCooldownLine("unknown", at) != "" {
		t.Fatal("unknown class labeled")
	}
}
