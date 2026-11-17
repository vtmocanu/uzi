package schedsvc

import (
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestSkipReasonForErr_ProviderSource pins PRD #4320 M4: each provider-source creation refusal
// records its own literal skip reason instead of the transient default, and the credential
// override refusal (which wraps the Codex harness-unsupported sentinel) is not mislabelled as a
// Codex override conflict.
func TestSkipReasonForErr_ProviderSource(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&workersvc.ProviderSourceUnusableError{Reason: workersvc.SourceUnusableRetired}, "provider_source_unusable"},
		{workersvc.ErrProviderLaneUnsupported, "provider_lane_unsupported"},
		{&workersvc.ProviderModelUnmappedError{Model: "m", Source: "run"}, "provider_model_unmapped"},
		{fmt.Errorf("wrapped: %w", workersvc.ErrProviderBindingStale), "provider_binding_stale"},
		{workersvc.ErrProviderDestinationNotAllowed, "provider_destination_not_allowed"},
		{workersvc.ErrCredentialOverrideProviderSource, "credential_override_provider_source"},
	}
	for _, tc := range cases {
		got, ok := skipReasonForErr(tc.err)
		if !ok || string(got) != tc.want {
			t.Errorf("skipReasonForErr(%v) = (%q, %t), want (%q, true)", tc.err, got, ok, tc.want)
		}
	}
}
