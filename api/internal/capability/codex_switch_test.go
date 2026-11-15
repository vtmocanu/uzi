package capability

import (
	"slices"
	"testing"
)

func TestFilterProtocolCodexCredentialSwitch(t *testing.T) {
	// This exercises only the leaf vocabulary filter, not worker negotiation.
	const token = "codex_credential_switch_v1"
	for _, tt := range []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "explicit_registered_token",
			in:   []string{token},
			want: []string{token},
		},
		{
			name: "alongside_existing_switch_and_harness",
			in:   []string{token, CredentialSwitchInPlaceV1, CredentialSwitchV1, CodexHarnessV1},
			want: []string{CodexHarnessV1, CredentialSwitchV1, CredentialSwitchInPlaceV1, token},
		},
		{
			name: "duplicates_unknowns_stable_order",
			in: []string{
				"unknown_protocol", CredentialSwitchInPlaceV1, token, Docker,
				CodexHarnessV1, CredentialSwitchV1, token, JVM,
				CredentialSwitchInPlaceV1, "unknown_protocol", CodexHarnessV1, CredentialSwitchV1,
			},
			want: []string{CodexHarnessV1, CredentialSwitchV1, CredentialSwitchInPlaceV1, token},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterProtocol(tt.in)
			if !slices.Contains(got, token) {
				t.Fatalf("FilterProtocol lost explicitly registered token %q: got %v", token, got)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("FilterProtocol = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("excluded_from_scheduler_filter", func(t *testing.T) {
		got := Filter([]string{token, JVM, Docker, token})
		if !slices.Equal(got, []string{Docker, JVM}) {
			t.Fatalf("Filter = %v, want only scheduler capabilities %v", got, []string{Docker, JVM})
		}
	})
	t.Run("excluded_from_self_reportable", func(t *testing.T) {
		got := SelfReportable([]string{token, Docker, token})
		if !slices.Equal(got, []string{Docker}) {
			t.Fatalf("SelfReportable = %v, want only %v", got, []string{Docker})
		}
	})
}
