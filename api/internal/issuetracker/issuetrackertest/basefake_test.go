package issuetrackertest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// Calling every method keeps each one reachable for `deadcode -test`, since the
// compile-time assertion in basefake.go creates no reachability.
func TestBaseFakeMethodsNotStubbed(t *testing.T) {
	ctx := context.Background()
	methods := []struct {
		name string
		call func(*BaseFake) error
	}{
		{"ListLabels", func(b *BaseFake) error { _, err := b.ListLabels(ctx, 1); return err }},
		{"EnsureLabels", func(b *BaseFake) error { return b.EnsureLabels(ctx, 1, nil) }},
		{"ListIssues", func(b *BaseFake) error {
			_, err := b.ListIssues(ctx, 1, forge.ListIssuesOptions{})
			return err
		}},
		{"GetIssue", func(b *BaseFake) error { _, err := b.GetIssue(ctx, 1, 2); return err }},
		{"CreateIssue", func(b *BaseFake) error { _, err := b.CreateIssue(ctx, 1, "t", "d", nil); return err }},
		{"UpdateIssueLabels", func(b *BaseFake) error { return b.UpdateIssueLabels(ctx, 1, 2, nil, nil) }},
		{"UpdateIssueDescription", func(b *BaseFake) error { return b.UpdateIssueDescription(ctx, 1, 2, "d") }},
		{"SetIssueState", func(b *BaseFake) error { return b.SetIssueState(ctx, 1, 2, forge.StateClosed) }},
		{"ListIssueLabelEvents", func(b *BaseFake) error { _, err := b.ListIssueLabelEvents(ctx, 1, 2); return err }},
		{"ListIssueComments", func(b *BaseFake) error { _, err := b.ListIssueComments(ctx, 1, 2); return err }},
		{"CreateIssueNote", func(b *BaseFake) error { _, err := b.CreateIssueNote(ctx, 1, 2, "b"); return err }},
		{"VerifyToken", func(b *BaseFake) error { _, err := b.VerifyToken(ctx); return err }},
		{"ListProjects", func(b *BaseFake) error { _, err := b.ListProjects(ctx); return err }},
		{"ListTags", func(b *BaseFake) error { _, err := b.ListTags(ctx); return err }},
		{"ListStates", func(b *BaseFake) error { _, err := b.ListStates(ctx, "p"); return err }},
	}
	if len(methods) != 15 {
		t.Fatalf("expected 15 methods, got %d", len(methods))
	}
	b := &BaseFake{}
	for _, m := range methods {
		t.Run(m.name, func(t *testing.T) {
			err := m.call(b)
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !errors.Is(err, ErrNotStubbed) {
				t.Fatalf("error does not wrap ErrNotStubbed: %v", err)
			}
			if !strings.Contains(err.Error(), m.name) {
				t.Fatalf("error %q does not name the method", err.Error())
			}
		})
	}
}
