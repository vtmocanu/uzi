package workersvc

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCodexApplyResultRejectsPartialBundle(t *testing.T) {
	generation := int64(0)
	for _, auth := range []ClaimCodexSecrets{
		{AuthMode: codexAuthModeAPIKey, Capability: "cap"},
		{AuthMode: codexAuthModeAPIKey, AccessToken: "private-token"},
		{AuthMode: codexAuthModeSubscription, AccessToken: "private-token", Capability: "cap"},
		{AuthMode: codexAuthModeSubscription, AccessToken: "private-token", Capability: "cap", Generation: &generation},
		{AuthMode: codexAuthModeAPIKey, AccessToken: "private-token", Capability: "cap", Generation: &generation},
	} {
		res := CredentialSwitchApplyResult{Outcome: ApplyOutcomeApplied, Codex: &auth, CapabilityEpoch: 42, Ordinal: 1, MessageSeq: 6}
		if _, err := json.Marshal(res); err == nil {
			t.Fatal("partial Codex bundle serialized as applied")
		}
		for _, value := range []string{fmt.Sprint(res), fmt.Sprintf("%+v", res), fmt.Sprintf("%#v", res)} {
			if strings.Contains(value, "private-token") || strings.Contains(value, "cap") {
				t.Fatal("partial auth leaked in formatting")
			}
		}
		res.Outcome = ApplyOutcomeNotApplied
		wire, err := json.Marshal(res)
		if err != nil || strings.Contains(string(wire), "private-token") || strings.Contains(string(wire), "cap") {
			t.Fatal("refusal disclosed auth")
		}
	}
}
