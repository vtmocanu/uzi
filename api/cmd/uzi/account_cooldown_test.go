package main

import (
	"encoding/json"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"strings"
	"testing"
	"time"
)

func TestAccountCooldownCLIConsumers(t *testing.T) {
	now := time.Now()
	at := now.Add(time.Hour)
	class := apitypes.AccountCooldownClass("overload")
	r := apitypes.RunDTO{ID: "cooldown", Status: "running", Health: "waiting_account_cooldown", HealthReason: sp(apitypes.AccountCooldownReason), AccountCooldownClass: &class, AccountCooldownEligibleAt: &at}
	want := apitypes.AccountCooldownLine(class, at)
	if got := runHealthReasonAt(r, now); got != want {
		t.Fatal(got)
	}
	if got := runStatusCellAt(apitypes.RunListItemDTO{RunDTO: r}, now); !strings.Contains(got, want) {
		t.Fatal(got)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"account_cooldown_class":"overload"`) {
		t.Fatal(string(data))
	}
	for _, status := range []string{"queued", "completed", "paused"} {
		copy := r
		copy.Status = status
		if got := accountCooldownLineAt(copy, now); got != "" {
			t.Fatalf("%s=%s", status, got)
		}
	}
	if got := accountCooldownLineAt(r, at); got != "" {
		t.Fatalf("expiry=%s", got)
	}
}
