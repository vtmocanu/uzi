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

// TestIsCASDeleteRefusal pins the predicate that is the SOLE arbiter, on a real forge, of
// a benign CAS-delete refusal (origin's checkpoint ref moved out from under the Old we
// bound in the list→delete window → nil, no retry) versus a genuine transport/auth fault
// (→ the wrapped error is surfaced). That wire branch is unreachable in the file:// test
// harness (the go-git internal server does not enforce the wire CAS — casDelete's local
// list-and-compare guard short-circuits first), so this unit test is the only coverage of
// the classification production depends on. It lives in an internal test file (mirroring
// TestIsWorkflowScopeRejection above) because the predicate is unexported.
func TestIsCASDeleteRefusal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error is not a refusal",
			err:  nil,
			want: false,
		},
		// Benign wire-CAS refusals: origin's ref moved past the Old we bound, surfacing as a
		// lock-failure / non-fast-forward form (reused from isNonFastForward).
		{
			name: "cannot lock ref (mismatched Old) is benign",
			err:  errors.New("cannot lock ref refs/uzi-checkpoints/agent/issue-7: is at abc123 but expected def456"),
			want: true,
		},
		{
			name: "non-fast-forward is benign",
			err:  errors.New("non-fast-forward"),
			want: true,
		},
		{
			name: "failed to update ref is benign",
			err:  errors.New("failed to update ref refs/uzi-checkpoints/agent/issue-7"),
			want: true,
		},
		{
			name: "fetch first is benign",
			err:  errors.New("! [rejected] agent/issue-7 -> agent/issue-7 (fetch first)"),
			want: true,
		},
		// Delete-of-missing forms: the ref is already absent, so the delete has nothing to do.
		{
			name: "does not exist is benign",
			err:  errors.New("error: unable to delete 'refs/uzi-checkpoints/agent/issue-7': remote ref does not exist"),
			want: true,
		},
		{
			name: "no such ref is benign",
			err:  errors.New("remote: no such ref refs/uzi-checkpoints/agent/issue-7"),
			want: true,
		},
		// Genuine faults that MUST NOT be swallowed as benign — a real transport/auth failure
		// has to return the wrapped error rather than silently classify as a benign refusal.
		{
			name: "authentication required is a genuine fault",
			err:  errors.New("authentication required"),
			want: false,
		},
		{
			name: "403 forbidden is a genuine fault",
			err:  errors.New("unexpected client error: unexpected requesting \"...\" status code: 403 forbidden"),
			want: false,
		},
		{
			name: "authorization failed is a genuine fault",
			err:  errors.New("authorization failed"),
			want: false,
		},
		{
			name: "connection refused is a genuine fault",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "context deadline exceeded is a genuine fault",
			err:  errors.New("context deadline exceeded"),
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCASDeleteRefusal(tc.err); got != tc.want {
				t.Fatalf("isCASDeleteRefusal(%v) = %t, want %t", tc.err, got, tc.want)
			}
		})
	}
}
