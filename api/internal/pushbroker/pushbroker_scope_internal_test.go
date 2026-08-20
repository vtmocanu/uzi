package pushbroker

import (
	"errors"
	"testing"
)

// TestIsWorkflowScopeRejection pins the text predicate PRD #456 M4 adds: GitHub refuses
// a checkpoint push whose .github/workflows/ tree differs from the default branch when
// the bot PAT lacks the `workflow` scope, and go-git surfaces that remote rejection as
// formatted error text (no sentinel), so — as with isNonFastForward — the message is the
// only discriminator. This lives in an internal test file (mirroring
// refspec_internal_test.go / budget_internal_test.go) because the predicate is
// unexported.
func TestIsWorkflowScopeRejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "representative github rejection",
			err: errors.New("command error on refs/uzi-checkpoints/agent/issue-456: " +
				"! [remote rejected] agent/issue-456 -> agent/issue-456 (refusing to allow a " +
				"Personal Access Token to create or update workflow .github/workflows/brew.yml " +
				"without workflow scope)"),
			want: true,
		},
		{
			name: "mixed-case rejection still matches (case-insensitive)",
			err:  errors.New("Refusing To Allow A Personal Access Token To Create Or Update Workflow .github/workflows/ci.yml Without Workflow Scope"),
			want: true,
		},
		{
			name: "short-form workflow scope substring matches (tolerant)",
			err:  errors.New("push rejected: missing workflow scope for this token"),
			want: true,
		},
		{
			name: "non-fast-forward is NOT a workflow-scope rejection",
			err:  errors.New("non-fast-forward update: refs/uzi-checkpoints/agent/issue-1"),
			want: false,
		},
		{
			name: "generic transport error is not a workflow-scope rejection",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "nil error is not a rejection",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isWorkflowScopeRejection(tc.err); got != tc.want {
				t.Fatalf("isWorkflowScopeRejection(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}

	// Discrimination: the non-fast-forward predicate and the workflow-scope predicate must
	// not both fire on the same message, so the push switch routes each rejection to its own
	// sentinel (ErrNotDescendant vs ErrWorkflowScopeRejected).
	nff := errors.New("non-fast-forward update: refs/uzi-checkpoints/agent/issue-1")
	if !isNonFastForward(nff) || isWorkflowScopeRejection(nff) {
		t.Fatalf("a non-fast-forward error must classify as NFF only, not workflow-scope")
	}
}
