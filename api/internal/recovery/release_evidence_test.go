package recovery

import (
	"errors"
	"testing"
)

// TestAllowlistedReleaseEvidence proves the worker Release endpoint's UNTRUSTED release-evidence
// allowlist (PRD #1392 M1, D3): only the two worker-assertable dispositions pass, absent stamps
// nothing, and every other value — a server-derived class or an injection attempt — is
// ErrBadRequest. The allowlist short-circuits before the pool, so this is a plain unit test that
// runs in `go test ./...` / gate:api. The STORAGE and generation-echo proofs for these paths are
// the swept-package *LiveDB tests (store.TestCustodyReleaseEvidenceLiveDB and
// handler.TestRecoveryReleaseAndDiscardEvidenceLiveDB), since internal/recovery is swept by no CI
// LiveDB job.
func TestAllowlistedReleaseEvidence(t *testing.T) {
	str := func(s string) *string { return &s }

	// Absent → no explicit evidence, no error.
	if got, err := allowlistedReleaseEvidence(nil); err != nil || got.Valid {
		t.Fatalf("allowlistedReleaseEvidence(nil) = (%+v, %v), want invalid text / no error", got, err)
	}

	// The two worker-assertable classes pass and carry their value.
	for _, ev := range []string{"publication", "forge_no_output"} {
		got, err := allowlistedReleaseEvidence(str(ev))
		if err != nil {
			t.Fatalf("allowlistedReleaseEvidence(%q) errored: %v", ev, err)
		}
		if !got.Valid || got.String != ev {
			t.Fatalf("allowlistedReleaseEvidence(%q) = %+v, want valid text %q", ev, got, ev)
		}
	}

	// Everything else is ErrBadRequest: server-derived classes a worker may NOT assert, the empty
	// string, a case variant, and an injection attempt.
	for _, ev := range []string{"archive", "owner_discard", "no_adopted_source", "", "PUBLICATION", "publication'; DROP"} {
		if _, err := allowlistedReleaseEvidence(str(ev)); !errors.Is(err, ErrBadRequest) {
			t.Fatalf("allowlistedReleaseEvidence(%q) = %v, want ErrBadRequest", ev, err)
		}
	}
}
