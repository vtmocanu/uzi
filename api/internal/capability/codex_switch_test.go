package capability

import (
	"slices"
	"testing"
)

func TestFilterProtocolCodexCredentialSwitch(t *testing.T) {
	// This exercises only the leaf vocabulary filter, not worker negotiation.
	const protocolName = "codex_credential_switch_v1"
	for _, tt := range []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "explicit_registered_token",
			in:   []string{protocolName},
			want: []string{protocolName},
		},
		{
			name: "alongside_existing_switch_and_harness",
			in:   []string{protocolName, CredentialSwitchInPlaceV1, CredentialSwitchV1, CodexHarnessV1},
			want: []string{CodexHarnessV1, CredentialSwitchV1, CredentialSwitchInPlaceV1, protocolName},
		},
		{
			name: "duplicates_unknowns_stable_order",
			in: []string{
				"unknown_protocol", CredentialSwitchInPlaceV1, protocolName, Docker,
				CodexHarnessV1, CredentialSwitchV1, protocolName, JVM,
				CredentialSwitchInPlaceV1, "unknown_protocol", CodexHarnessV1, CredentialSwitchV1,
			},
			want: []string{CodexHarnessV1, CredentialSwitchV1, CredentialSwitchInPlaceV1, protocolName},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterProtocol(tt.in)
			if !slices.Contains(got, protocolName) {
				t.Fatalf("FilterProtocol lost explicitly registered token %q: got %v", protocolName, got)
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("FilterProtocol = %v, want %v", got, tt.want)
			}
		})
	}

	t.Run("excluded_from_scheduler_filter", func(t *testing.T) {
		got := Filter([]string{protocolName, JVM, Docker, protocolName})
		if !slices.Equal(got, []string{Docker, JVM}) {
			t.Fatalf("Filter = %v, want only scheduler capabilities %v", got, []string{Docker, JVM})
		}
	})
	t.Run("excluded_from_self_reportable", func(t *testing.T) {
		got := SelfReportable([]string{protocolName, Docker, protocolName})
		if !slices.Equal(got, []string{Docker}) {
			t.Fatalf("SelfReportable = %v, want only %v", got, []string{Docker})
		}
	})
}
