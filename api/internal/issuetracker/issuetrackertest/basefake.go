// Package issuetrackertest provides BaseFake, an embeddable implementation of
// every issuetracker.Driver method that fails loudly, so a test fake embeds it
// and overrides only what it exercises. An unstubbed method returns an error
// wrapping ErrNotStubbed that names the method, never a silent zero success.
package issuetrackertest

import (
	"context"
	"errors"
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
)

// ErrNotStubbed is the sentinel every unstubbed BaseFake method wraps.
var ErrNotStubbed = errors.New("issuetrackertest: method not stubbed")

func notStubbed(method string) error {
	return fmt.Errorf("issuetrackertest: method %s not stubbed: %w", method, ErrNotStubbed)
}

// BaseFake implements issuetracker.Driver with not-stubbed defaults.
type BaseFake struct{}

var _ issuetracker.Driver = (*BaseFake)(nil)

// ListLabels implements issuetracker.Driver.
func (*BaseFake) ListLabels(context.Context, int64) ([]forge.Label, error) {
	return nil, notStubbed("ListLabels")
}

// EnsureLabels implements issuetracker.Driver.
func (*BaseFake) EnsureLabels(context.Context, int64, []forge.Label) error {
	return notStubbed("EnsureLabels")
}

// ListIssues implements issuetracker.Driver.
func (*BaseFake) ListIssues(context.Context, int64, forge.ListIssuesOptions) ([]forge.Issue, error) {
	return nil, notStubbed("ListIssues")
}

// GetIssue implements issuetracker.Driver.
func (*BaseFake) GetIssue(context.Context, int64, int64) (forge.Issue, error) {
	return forge.Issue{}, notStubbed("GetIssue")
}

// CreateIssue implements issuetracker.Driver.
func (*BaseFake) CreateIssue(context.Context, int64, string, string, []string) (forge.Issue, error) {
	return forge.Issue{}, notStubbed("CreateIssue")
}

// UpdateIssueLabels implements issuetracker.Driver.
func (*BaseFake) UpdateIssueLabels(context.Context, int64, int64, []string, []string) error {
	return notStubbed("UpdateIssueLabels")
}

// UpdateIssueDescription implements issuetracker.Driver.
func (*BaseFake) UpdateIssueDescription(context.Context, int64, int64, string) error {
	return notStubbed("UpdateIssueDescription")
}

// SetIssueState implements issuetracker.Driver.
func (*BaseFake) SetIssueState(context.Context, int64, int64, forge.IssueState) error {
	return notStubbed("SetIssueState")
}

// ListIssueLabelEvents implements issuetracker.Driver.
func (*BaseFake) ListIssueLabelEvents(context.Context, int64, int64) ([]forge.LabelEvent, error) {
	return nil, notStubbed("ListIssueLabelEvents")
}

// ListIssueComments implements issuetracker.Driver.
func (*BaseFake) ListIssueComments(context.Context, int64, int64) ([]forge.IssueComment, error) {
	return nil, notStubbed("ListIssueComments")
}

// CreateIssueNote implements issuetracker.Driver.
func (*BaseFake) CreateIssueNote(context.Context, int64, int64, string) (forge.IssueNote, error) {
	return forge.IssueNote{}, notStubbed("CreateIssueNote")
}

// VerifyToken implements issuetracker.Driver.
func (*BaseFake) VerifyToken(context.Context) (issuetracker.Account, error) {
	return issuetracker.Account{}, notStubbed("VerifyToken")
}

// ListProjects implements issuetracker.Driver.
func (*BaseFake) ListProjects(context.Context) ([]issuetracker.Project, error) {
	return nil, notStubbed("ListProjects")
}

// ListTags implements issuetracker.Driver.
func (*BaseFake) ListTags(context.Context) ([]issuetracker.Tag, error) {
	return nil, notStubbed("ListTags")
}

// ListStates implements issuetracker.Driver.
func (*BaseFake) ListStates(context.Context, string) ([]issuetracker.State, error) {
	return nil, notStubbed("ListStates")
}
