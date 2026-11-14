package forgesvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4007: the terminal-preservation lifetime read reaches the issue side through
// IssueTrackerForRepo and the pull request side through CodeHostForRepo. workersvc cannot import
// forgesvc, so it type-asserts its ForgeBuilder to workersvc.TerminalLifetimeForges; this pins that
// *Service satisfies it (and still satisfies the ForgeBuilder it is wired as).
var (
	_ workersvc.TerminalLifetimeForges = (*Service)(nil)
	_ workersvc.ForgeBuilder           = (*Service)(nil)
)

func TestServiceSatisfiesTerminalLifetimeForges(t *testing.T) {
	var s any = (*Service)(nil)
	if _, ok := s.(workersvc.TerminalLifetimeForges); !ok {
		t.Fatal("*forgesvc.Service must implement workersvc.TerminalLifetimeForges")
	}
}
