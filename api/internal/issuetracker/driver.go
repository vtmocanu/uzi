package issuetracker

import (
	"context"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// Account is the identity a token authenticates as.
type Account struct {
	ID    string
	Login string
}

// Project is an issue container on the issue tracker. ID is the opaque
// server-side identifier; ShortName is the human-facing key (for example the
// prefix of an issue id).
type Project struct {
	ID        string
	ShortName string
	Name      string
}

// State is one value of the issue state field.
type State struct {
	ID       string
	Name     string
	Resolved bool // true when the state counts as closed
	Ordinal  int
}

// Driver is an external issue tracker connection. It embeds forge.IssueTracker
// only, never forge.CodeHost (PRD #4173 S1). The extra methods serve the
// connection flow: verify the token, then discover projects, tags and states.
type Driver interface {
	forge.IssueTracker
	// VerifyToken confirms the token works and returns the account it belongs to.
	VerifyToken(ctx context.Context) (Account, error)
	// ListProjects returns the projects the token can see.
	ListProjects(ctx context.Context) ([]Project, error)
	// ListTags returns the tags the token can see, with their owners and
	// sharing settings.
	ListTags(ctx context.Context) ([]Tag, error)
	// ListStates returns the issue states available in a project.
	ListStates(ctx context.Context, projectID string) ([]State, error)
}
