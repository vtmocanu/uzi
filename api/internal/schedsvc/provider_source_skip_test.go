package schedsvc

import (
	"context"
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestTickOnceProviderSourceUnusableHoldsAndRecurringAdvances: a one-time row whose owner's provider
// source is unusable is held un-advanced (the owner can repair the provider), recording
// provider_source_unusable, and fires once it is repaired; a recurring row advances. The other
// provider refusals stay benign and advancing for a once row, as before.
func TestTickOnceProviderSourceUnusableHoldsAndRecurringAdvances(t *testing.T) {
	unusable := &workersvc.ProviderSourceUnusableError{Reason: workersvc.SourceUnusableKeyDisabled}
	t.Run("once is held then fires after the repair", func(t *testing.T) {
		h := newHarness()
		h.st.due = []store.RunSchedule{onceOf(h.issueSchedule())}
		h.runs.err = unusable
		for tick := 0; tick < 2; tick++ {
			h.sched.Boot(context.Background())
		}
		if len(h.st.advanceCalls) != 0 || len(h.st.statusCalls) != 0 || len(h.runs.autopilot) != 0 {
			t.Fatalf("a held once row must not advance, park or start: advance=%d status=%d runs=%d", len(h.st.advanceCalls), len(h.st.statusCalls), len(h.runs.autopilot))
		}
		if len(h.st.heldFireCalls) != 1 {
			t.Fatalf("held last_fire writes = %d, want 1", len(h.st.heldFireCalls))
		}
		if started, reasons := heldLastFire(t, h.st.due[0].LastFire); started != 0 || len(reasons) != 1 || reasons[0] != "provider_source_unusable" {
			t.Fatalf("held last_fire started=%d reasons=%v, want [provider_source_unusable]", started, reasons)
		}
		h.runs.err = nil
		h.sched.Boot(context.Background())
		if len(h.runs.autopilot) != 1 {
			t.Fatalf("once retry after the repair: runs = %d, want 1", len(h.runs.autopilot))
		}
	})
	t.Run("recurring advances", func(t *testing.T) {
		h := newHarness()
		h.st.due = []store.RunSchedule{h.issueSchedule()}
		h.runs.err = unusable
		h.sched.Boot(context.Background())
		if len(h.st.advanceCalls) != 1 || len(h.st.heldFireCalls) != 0 {
			t.Fatalf("recurring: advance=%d held=%d, want one advance and no hold", len(h.st.advanceCalls), len(h.st.heldFireCalls))
		}
	})
	t.Run("other provider refusals do not hold a once row", func(t *testing.T) {
		for _, err := range []error{workersvc.ErrProviderModelUnmapped, workersvc.ErrProviderLaneUnsupported, workersvc.ErrProviderBindingStale} {
			if holdsOnceCredentialDisabled(onceOf(store.RunSchedule{}), err) {
				t.Errorf("a once row is held on %v, want it consumed like any benign skip", err)
			}
		}
	})
}

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
