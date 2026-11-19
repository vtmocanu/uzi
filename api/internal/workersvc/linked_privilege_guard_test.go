package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
)

// guardForge is a forge whose branch protection reads clean, or fails when verifyErr is set.
type guardForge struct {
	forgetest.BaseFake
	verifyErr error
}

func (g *guardForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return forge.BotIdentity{ForgeUserID: 42}, g.verifyErr
}
func (*guardForge) ProjectRole(context.Context, int64, int64) (forge.Role, bool, error) {
	return forge.RoleWrite, true, nil
}
func (*guardForge) DefaultBranchProtection(context.Context, int64, string, int64) (forge.BranchProtection, error) {
	return forge.BranchProtection{Protected: true}, nil
}

// PRD #4173 M4 acceptance: on a repo linked to an issue tracker the default-branch
// guardrail (the same privcheck.GuardRepo the claim backstop calls) still runs against
// the forge only. It passes on a healthy forge without one issue tracker request, even
// with the issue tracker unreachable, and fails closed when the forge cannot be read.
func TestLinkedRepoGuardrailRunsAgainstTheForgeOnlyAndFailsClosed(t *testing.T) {
	for name, tc := range map[string]struct {
		verifyErr error
		wantBlock bool
	}{
		"healthy forge passes": {},
		"failing forge blocks": {verifyErr: errors.New("401 token revoked"), wantBlock: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := youtracktest.NewServer(t)
			yt := youTrackDriver(t, srv)
			srv.Close() // the issue tracker is unreachable; any request to it would fail or be recorded
			srv.ResetRequests()

			g := &guardForge{verifyErr: tc.verifyErr}
			fs := &fakeStore{}
			svc := New(fs, newBox(t), testParams())
			svc.SetRepoGuard(privcheck.NewService(nil, m2Builder{g}))
			svc.SetIssueSources(&fakeSources{tr: yt})

			row := linkedRepoRow()
			row.DefaultBranch.String, row.DefaultBranch.Valid = "main", true
			err := svc.guardDefaultBranch(context.Background(), row)
			var blocked *GuardrailBlockedError
			if got := errors.As(err, &blocked); got != tc.wantBlock {
				t.Fatalf("guardDefaultBranch = %v, want blocked=%v", err, tc.wantBlock)
			}
			if n := len(srv.Requests()); n != 0 {
				t.Fatalf("the guardrail made %d issue tracker requests", n)
			}
		})
	}
}
