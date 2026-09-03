package forgetest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// The forge.Forge interface has exactly 26 methods. This test invokes EVERY one
// on a *BaseFake and asserts its default. It is both the contract proof (each
// method returns what the package promises) AND the deadcode shield: the
// compile-time `var _ forge.Forge = (*BaseFake)(nil)` assertion in basefake.go
// creates NO reachability, so a BaseFake method that every fake overrides and
// nothing else invokes could be flagged by `deadcode -test`. Actually calling
// each method here is what keeps them all reachable — so all 26 must appear
// below (24 action methods + 2 pipeline reads). A missing method defeats the
// shield.

// actionMethods are the 24 methods that default to notStubbed(<name>). Each
// closure calls exactly one method and returns only its error return, so every
// method is invoked and every arity collapses to a single comparable error.
func actionMethods() []struct {
	name string
	call func(*BaseFake) error
} {
	ctx := context.Background()
	return []struct {
		name string
		call func(*BaseFake) error
	}{
		{"VerifyToken", func(b *BaseFake) error { _, err := b.VerifyToken(ctx); return err }},
		{"ListProjects", func(b *BaseFake) error { _, err := b.ListProjects(ctx); return err }},
		{"ListLabels", func(b *BaseFake) error { _, err := b.ListLabels(ctx, 1); return err }},
		{"EnsureLabels", func(b *BaseFake) error { return b.EnsureLabels(ctx, 1, nil) }},
		{"ListIssues", func(b *BaseFake) error {
			_, err := b.ListIssues(ctx, 1, forge.ListIssuesOptions{})
			return err
		}},
		{"GetIssue", func(b *BaseFake) error { _, err := b.GetIssue(ctx, 1, 2); return err }},
		{"CreateIssue", func(b *BaseFake) error {
			_, err := b.CreateIssue(ctx, 1, "t", "d", nil)
			return err
		}},
		{"UpdateIssueLabels", func(b *BaseFake) error { return b.UpdateIssueLabels(ctx, 1, 2, nil, nil) }},
		{"UpdateIssueDescription", func(b *BaseFake) error { return b.UpdateIssueDescription(ctx, 1, 2, "d") }},
		{"SetIssueState", func(b *BaseFake) error { return b.SetIssueState(ctx, 1, 2, forge.StateClosed) }},
		{"UserExists", func(b *BaseFake) error { _, err := b.UserExists(ctx, "u"); return err }},
		{"ListIssueLabelEvents", func(b *BaseFake) error {
			_, err := b.ListIssueLabelEvents(ctx, 1, 2)
			return err
		}},
		{"ListIssueComments", func(b *BaseFake) error {
			_, err := b.ListIssueComments(ctx, 1, 2)
			return err
		}},
		{"CreateIssueNote", func(b *BaseFake) error {
			_, err := b.CreateIssueNote(ctx, 1, 2, "body")
			return err
		}},
		{"GetMergeRequest", func(b *BaseFake) error {
			_, err := b.GetMergeRequest(ctx, 1, 2)
			return err
		}},
		{"ListMergeRequestComments", func(b *BaseFake) error {
			_, err := b.ListMergeRequestComments(ctx, 1, 2)
			return err
		}},
		{"ReplyMergeRequestComment", func(b *BaseFake) error {
			return b.ReplyMergeRequestComment(ctx, 1, 2, "r", "body")
		}},
		{"ResolveMergeRequestThread", func(b *BaseFake) error {
			return b.ResolveMergeRequestThread(ctx, 1, 2, "res")
		}},
		{"TokenInfo", func(b *BaseFake) error { _, err := b.TokenInfo(ctx); return err }},
		{"ProjectRole", func(b *BaseFake) error {
			role, member, err := b.ProjectRole(ctx, 1, 2)
			// Verify the non-error defaults too: zero Role and non-member.
			if role != forge.RoleNone || member {
				return errors.New("ProjectRole non-error defaults wrong")
			}
			return err
		}},
		{"DefaultBranchProtection", func(b *BaseFake) error {
			_, err := b.DefaultBranchProtection(ctx, 1, "main", 2)
			return err
		}},
		{"ListPipelineJobs", func(b *BaseFake) error {
			_, err := b.ListPipelineJobs(ctx, 1, 2)
			return err
		}},
		{"JobLogTail", func(b *BaseFake) error {
			_, err := b.JobLogTail(ctx, 1, 2, 100)
			return err
		}},
		{"ProjectCIConfigPath", func(b *BaseFake) error {
			_, err := b.ProjectCIConfigPath(ctx, 1)
			return err
		}},
	}
}

func TestBaseFakeActionMethodsNotStubbed(t *testing.T) {
	methods := actionMethods()
	if len(methods) != 24 {
		t.Fatalf("expected 24 action methods, got %d", len(methods))
	}
	b := &BaseFake{}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			err := m.call(b)
			if err == nil {
				t.Fatalf("%s: expected an error, got nil", m.name)
			}
			if !errors.Is(err, ErrNotStubbed) {
				t.Fatalf("%s: error does not wrap ErrNotStubbed: %v", m.name, err)
			}
			if !strings.Contains(err.Error(), m.name) {
				t.Fatalf("%s: error string %q does not name the method", m.name, err.Error())
			}
		})
	}
}

// The two pipeline reads are the deliberate absence-sentinel exception: they
// return forge.ErrNoPipeline (not ErrNotStubbed) and the zero Pipeline, because
// consumers branch on "no pipeline" as a non-error.
func TestBaseFakePipelineReadsReturnNoPipeline(t *testing.T) {
	ctx := context.Background()
	b := &BaseFake{}

	t.Run("LatestPipeline", func(t *testing.T) {
		pl, err := b.LatestPipeline(ctx, 1, "main")
		if !errors.Is(err, forge.ErrNoPipeline) {
			t.Fatalf("LatestPipeline: want ErrNoPipeline, got %v", err)
		}
		if pl != (forge.Pipeline{}) {
			t.Fatalf("LatestPipeline: want zero Pipeline, got %+v", pl)
		}
	})

	t.Run("LatestMRPipeline", func(t *testing.T) {
		pl, err := b.LatestMRPipeline(ctx, 1, 2)
		if !errors.Is(err, forge.ErrNoPipeline) {
			t.Fatalf("LatestMRPipeline: want ErrNoPipeline, got %v", err)
		}
		if pl != (forge.Pipeline{}) {
			t.Fatalf("LatestMRPipeline: want zero Pipeline, got %+v", pl)
		}
	})
}
