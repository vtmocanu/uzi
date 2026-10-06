package schedsvc

import (
	"context"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestSkipReasonEnumIsHonest pins the Go side of the SkipReason contract internally
// consistent (PRD #308 M3; PRD #590 M1 added vault_locked; PRD #764 retired the link-less skip;
// issue #856 added open_mr_exists; PRD #1093 M1 added schedules_paused; PRD #1429 M2 added
// codex_override_conflict; a PRD #1429 review fix added no_usable_credential; PRD #1732 added
// credential_disabled; PRD #2343 added config_not_supported): the closed set has the expected
// members with no duplicates, every benign seam sentinel maps into that
// set, an unrelated error maps to no reason, and the reasons the seam does not map (fetch_failed
// is recorded at the sweep site, already_running also at the prompt site, vault_locked at the
// self_improve site) are still enumerated. The cross-language guard that the TS reason union has
// not drifted lives in web/src/lib/scheduleSkipReasons.test.ts; this keeps the Go enum honest so
// that guard has a trustworthy source to compare against.
func TestSkipReasonEnumIsHonest(t *testing.T) {
	want := map[SkipReason]bool{
		SkipConfigNotSupported:      true,
		SkipNotEligible:             true,
		SkipAlreadyRunning:          true,
		SkipDescriptionTooLarge:     true,
		SkipFetchFailed:             true,
		SkipVaultLocked:             true,
		SkipSelfImproveMRCapReached: true,
		SkipOpenMRExists:            true,
		SkipCodexOverrideConflict:   true,
		SkipSchedulesPaused:         true,
		SkipNoUsableCredential:      true,
		SkipCredentialDisabled:      true,
	}

	if len(AllSkipReasons) != len(want) {
		t.Fatalf("AllSkipReasons has %d members, want %d", len(AllSkipReasons), len(want))
	}

	seen := make(map[SkipReason]bool, len(AllSkipReasons))
	for _, r := range AllSkipReasons {
		if seen[r] {
			t.Fatalf("AllSkipReasons contains a duplicate: %q", r)
		}
		seen[r] = true
		if !want[r] {
			t.Fatalf("AllSkipReasons contains an unexpected reason: %q", r)
		}
	}
	for r := range want {
		if !seen[r] {
			t.Fatalf("AllSkipReasons is missing the expected reason %q", r)
		}
	}

	// SkipFetchFailed and SkipAlreadyRunning are recorded at their own sites (the sweep
	// fan-out and the prompt path), not returned by skipReasonForErr, but they must still
	// be enumerated in the closed set.
	if !seen[SkipFetchFailed] {
		t.Fatal("AllSkipReasons must include SkipFetchFailed")
	}
	if !seen[SkipAlreadyRunning] {
		t.Fatal("AllSkipReasons must include SkipAlreadyRunning")
	}

	// Every benign seam sentinel maps to a member of the closed set; an unrelated error
	// maps to no reason.
	mapped := []struct {
		name string
		err  error
	}{
		{"ErrNotPRDIssue", workersvc.ErrNotPRDIssue},
		{"ErrActiveRunExists", workersvc.ErrActiveRunExists},
		{"ErrDescriptionTooLarge", workersvc.ErrDescriptionTooLarge},
		{"ErrOpenMRExists", workersvc.ErrOpenMRExists},
		{"ErrCredentialOverrideHarnessUnsupported", workersvc.ErrCredentialOverrideHarnessUnsupported},
		{"ErrNoUsableCredential", workersvc.ErrNoUsableCredential},
		{"ErrBranchInUse", workersvc.ErrBranchInUse},
		{"ErrCredentialDisabled", workersvc.ErrCredentialDisabled},
		{"ErrHarnessCredentialDisabled", workersvc.ErrHarnessCredentialDisabled},
	}
	for _, c := range mapped {
		got, ok := skipReasonForErr(c.err)
		if !ok {
			t.Fatalf("skipReasonForErr(%s) = (_, false), want a mapped reason", c.name)
		}
		if !seen[got] {
			t.Fatalf("skipReasonForErr(%s) returned %q, which is not in AllSkipReasons", c.name, got)
		}
	}
	if r, ok := skipReasonForErr(context.DeadlineExceeded); ok {
		t.Fatalf("skipReasonForErr(unrelated error) = (%q, true), want (_, false)", r)
	}
}

// TestSkipReasonCredentialDisabled (PRD #1732 D2/D15): a disabled stored pin and a pinned
// harness with no enabled credential both record credential_disabled, while a genuinely absent
// credential for an explicit harness stays unmapped (the hard, non-advancing refusal).
//
// MUTATION: drop the credential_disabled arm; the disabled-harness error then falls to no
// reason and the pin error to no reason, and this test fails.
func TestSkipReasonCredentialDisabled(t *testing.T) {
	for name, err := range map[string]error{
		"disabled pin":            workersvc.ErrCredentialDisabled,
		"disabled pinned harness": workersvc.ErrHarnessCredentialDisabled,
	} {
		if got, ok := skipReasonForErr(err); !ok || got != SkipCredentialDisabled {
			t.Fatalf("%s: skipReasonForErr = (%q, %t), want credential_disabled", name, got, ok)
		}
	}
	if got, ok := skipReasonForErr(workersvc.ErrNoCredentialForHarness); ok {
		t.Fatalf("absent explicit-harness credential mapped to %q, want the hard refusal", got)
	}
}
