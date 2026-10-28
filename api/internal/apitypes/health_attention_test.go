package apitypes

import (
	"encoding/json"
	"testing"
)

func TestHealthAttentionBannerJSON(t *testing.T) {
	for _, attention := range []bool{false, true} {
		c := HealthCheckDTO{ID: "recovery.storage", Scope: "owner", Severity: "warn", AttentionBanner: attention}
		body, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		flag, present := fields["attention_banner"]
		if present != attention || (present && string(flag) != "true") {
			t.Fatalf("attention=%v: %s", attention, body)
		}
		var got HealthCheckDTO
		if err := json.Unmarshal(body, &got); err != nil || got.AttentionBanner != attention {
			t.Fatalf("round trip: %+v, %v", got, err)
		}
	}
}
