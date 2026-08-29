package schedsvc

import (
	"context"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestSkipReasonEnumIsHonest pins the Go side of the SkipReason contract internally
// consistent (PRD #308 M3; PRD #590 M1 added vault_locked): the closed set has exactly the
// six expected members with no duplicates, every benign seam sentinel maps into that set, an
// unrelated error maps to no reason, and the reasons the seam does not map (fetch_failed is
// recorded at the sweep site, already_running also at the prompt site, vault_locked at the
// self_improve site) are still enumerated. The
// cross-language guard that the TS reason union has not drifted lives in
// web/src/lib/scheduleSkipReasons.test.ts; this keeps the Go enum honest so that guard
// has a trustworthy source to compare against.
func TestSkipReasonEnumIsHonest(t *testing.T) {
	want := map[SkipReason]bool{
		SkipNoPRDLink:               true,
		SkipNotEligible:             true,
		SkipAlreadyRunning:          true,
		SkipDescriptionTooLarge:     true,
		SkipFetchFailed:             true,
		SkipVaultLocked:             true,
		SkipSelfImproveMRCapReached: true,
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
		{"ErrNoPRDLink", workersvc.ErrNoPRDLink},
		{"ErrNotPRDIssue", workersvc.ErrNotPRDIssue},
		{"ErrActiveRunExists", workersvc.ErrActiveRunExists},
		{"ErrDescriptionTooLarge", workersvc.ErrDescriptionTooLarge},
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
