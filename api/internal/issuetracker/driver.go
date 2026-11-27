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
	// Archived is true for a value the bundle keeps but no longer offers.
	Archived bool
	// LocalizedName is the value's name in the server's language, empty when it
	// has none or the driver refused it (see stateNameProblem).
	LocalizedName string
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
	// CheckStateField reports whether uzi can write the project's State field.
	// It returns ErrStateMachineField when a sample of the project's issues
	// shows the field run by a state machine, ErrStateFieldUnreadable (never
	// nil and never ErrStateMachineField for it) when a sampled issue of the
	// project does not list the State field as a plain StateIssueCustomField
	// only, nil when the sample is empty or its issues of the project are
	// plain, and a plain error (never nil, never
	// ErrStateMachineField) when the sample is not empty but holds no issue of
	// the project, as with a stale short name. A (redacted) read error is
	// returned when the sample cannot be read, so a failed read never counts as
	// a pass. ErrStateMachineField wins when both kinds of issue appear. It
	// never writes. A sample is one page of issues, so a project that mixes
	// kinds is judged by that page only.
	CheckStateField(ctx context.Context, project ProjectRef) error
}
