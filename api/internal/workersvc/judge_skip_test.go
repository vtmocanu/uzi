package workersvc

import (
	"errors"
	"fmt"
	"testing"
)

// TestJudgeSkipReasonFor pins the refusal-to-reason mapping and its precedence: the typed
// refusals wrap one another, so the most specific wins (disabled, then pool, then the plain
// no-credential), and anything else records nothing.
func TestJudgeSkipReasonFor(t *testing.T) {
	both := fmt.Errorf("%w and %w", ErrCodexPoolUnavailable, ErrHarnessCredentialDisabled)
	cases := []struct {
		name   string
		err    error
		want   string
		wantOK bool
	}{
		{"disabled", ErrHarnessCredentialDisabled, JudgeSkipCredentialDisabled, true},
		{"pool unavailable", ErrCodexPoolUnavailable, JudgeSkipCodexPoolUnavailable, true},
		{"no credential", ErrNoCredentialForHarness, JudgeSkipNoUsableCredential, true},
		{"wrapped no credential", fmt.Errorf("create: %w", ErrNoCredentialForHarness), JudgeSkipNoUsableCredential, true},
		{"disabled beats pool", both, JudgeSkipCredentialDisabled, true},
		{"pool beats no credential", fmt.Errorf("%w: %w", ErrNoCredentialForHarness, ErrCodexPoolUnavailable), JudgeSkipCodexPoolUnavailable, true},
		{"isolation guard", errJudgeCreateIsolation, "", false},
		{"other", errors.New("boom"), "", false},
		{"nil", nil, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := judgeSkipReasonFor(tc.err)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("judgeSkipReasonFor(%v) = (%q, %v), want (%q, %v)", tc.err, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
