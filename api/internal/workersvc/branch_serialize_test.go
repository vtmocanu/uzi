package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestIssueIIDFromAgentBranch pins the strict inverse of agentIssueBranch (issue #1626): only a
// byte-exact agent/issue-<n> with n > 0 maps, so the run-branch lock key and the issue-run
// check key are the same bytes the issue run for n locks.
func TestIssueIIDFromAgentBranch(t *testing.T) {
	cases := []struct {
		ref    string
		want   int64
		wantOK bool
	}{
		{"agent/issue-7", 7, true},
		{"agent/issue-1626", 1626, true},
		{"agent/issue-9223372036854775807", 9223372036854775807, true},
		{"agent/issue-007", 0, false},                  // non-canonical: a different git branch from agent/issue-7
		{"agent/issue-7x", 0, false},                   // trailing garbage
		{"agent/issue-+7", 0, false},                   // sign is not part of agentIssueBranch's output
		{"agent/issue-0", 0, false},                    // n must be > 0
		{"agent/issue--7", 0, false},                   // negative
		{"agent/issue-", 0, false},                     // empty suffix
		{"agent/issue- 7", 0, false},                   // whitespace
		{"agent/issue-7/x", 0, false},                  // nested path
		{"Agent/issue-7", 0, false},                    // case-sensitive prefix
		{"feature/issue-7", 0, false},                  // wrong prefix
		{"agent/issue-99999999999999999999", 0, false}, // overflows int64
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := issueIIDFromAgentBranch(c.ref)
		if got != c.want || ok != c.wantOK {
			t.Errorf("issueIIDFromAgentBranch(%q) = (%d, %v), want (%d, %v)", c.ref, got, ok, c.want, c.wantOK)
		}
		if ok && agentIssueBranch(got) != c.ref {
			t.Errorf("round trip: agentIssueBranch(%d) = %q, want %q", got, agentIssueBranch(got), c.ref)
		}
	}
}

// An active issue run for N refuses a ci_fix on agent/issue-N (issue #1626), even with no
// active run holding runs.branch (an issue run's branch stays NULL while it is active).
func TestCreateCIFixRunRefusesActiveIssueRun(t *testing.T) {
	fs := &fakeStore{activeIssueRunForIID: true}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-9", "t", "d", sampleSnapshot(), nil); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("err = %v, want ErrBranchInUse", err)
	}
	if fs.ciFixRunParams != nil {
		t.Fatal("the ci_fix insert ran despite the active issue run")
	}
}

// A non-agent ref and a non-canonical agent ref never consult the issue-run check.
func TestCreateCIFixRunIssueCheckOnlyOnCanonicalAgentRef(t *testing.T) {
	for _, ref := range []string{"feature/x", "agent/issue-007"} {
		fs := &fakeStore{activeIssueRunForIID: true, ciFixRunResult: store.Run{ID: uuid.New(), Kind: runkind.CIFix}}
		svc := New(fs, newBox(t), testParams())
		if _, err := svc.CreateCIFixRun(context.Background(), uuid.New(), uuid.New(), ref, "t", "d", sampleSnapshot(), nil); err != nil {
			t.Fatalf("ref %q: err = %v, want success (the issue-run check must not apply)", ref, err)
		}
	}
}

// An active issue run for N refuses both mr_rework create paths on agent/issue-N, before the
// insert runs.
func TestCreateMRReworkRunRefusesActiveIssueRun(t *testing.T) {
	fs := &fakeStore{repoRow: aValidRepoRow(), runByIDPlain: store.Run{Harness: harnessClaude}, activeIssueRunForIID: true, mrReworkRunResult: store.Run{ID: uuid.New(), Kind: runkind.MRRework}}
	svc := New(fs, newBox(t), testParams())
	if _, err := svc.CreateAutoMRReworkRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-9", 9, uuid.New(), "t", "d", nil); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("auto: err = %v, want ErrBranchInUse", err)
	}
	if fs.mrReworkRunParams != nil {
		t.Fatal("auto: the mr_rework insert ran despite the active issue run")
	}
	if _, err := svc.CreateManualMRReworkRun(context.Background(), uuid.New(), uuid.New(), "agent/issue-9", 9, uuid.New(), "t", "d", nil, 5); !errors.Is(err, ErrBranchInUse) {
		t.Fatalf("manual: err = %v, want ErrBranchInUse", err)
	}
	if fs.mrReworkAndAdvanceParams != nil {
		t.Fatal("manual: the mr_rework insert ran despite the active issue run")
	}
}
