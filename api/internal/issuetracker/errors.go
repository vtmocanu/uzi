package issuetracker

import "errors"

var (
	// ErrUnsupportedKind is returned by New for a Kind with no driver.
	ErrUnsupportedKind = errors.New("issuetracker: unsupported kind")
	// ErrProjectNotBound is returned by an IssueTracker method on a driver built
	// with no bound project (Options.Project empty).
	ErrProjectNotBound = errors.New("issuetracker: project not bound to this connection")
	// ErrProjectMismatch is returned by an IssueTracker method called with a
	// project id other than the one the driver is bound to.
	ErrProjectMismatch = errors.New("issuetracker: project id does not match the one this connection is bound to")
	// ErrStateMappingUnset is returned when open/closed state mapping is needed
	// but the connection has not configured it.
	ErrStateMappingUnset = errors.New("issuetracker: state mapping not configured")
	// ErrColumnNotMapped is returned by MoveToColumn for a board column that has
	// no issue tracker State mapped to it.
	ErrColumnNotMapped = errors.New("issuetracker: board column has no state mapped")
	// ErrStateNotAllowed is returned before a State write whose target the
	// project's State bundle does not allow: the value is missing, archived, or
	// has the wrong resolved flag or meaning for the role it is written in.
	ErrStateNotAllowed = errors.New("issuetracker: state value is not allowed for this write")
	// ErrInvalidLabelName is returned for a label name the issue tracker cannot carry.
	ErrInvalidLabelName = errors.New("issuetracker: invalid label name")
	// ErrRedirectRefused is returned when the server answers with a redirect.
	// The client never follows one, so the token cannot be forwarded.
	ErrRedirectRefused = errors.New("issuetracker: redirect refused")
	// ErrIssueResolved is returned by MoveToColumn for an issue that is resolved
	// in the issue tracker when the move is not an explicit reopen. Nothing is
	// written.
	ErrIssueResolved = errors.New("issuetracker: issue is resolved in the issue tracker")
	// ErrStateMachineField is returned when a project's State field is run by a
	// state machine (YouTrack's StateMachineIssueCustomField): the issue tracker
	// then owns which transitions are allowed, so uzi cannot write the State
	// directly. CheckStateField returns it for a project and a State write
	// returns it for an issue; nothing is written.
	ErrStateMachineField = errors.New("issuetracker: the State field is run by a state machine")
	// ErrStateFieldUnreadable is returned when an issue's response does not
	// list the State field as a plain StateIssueCustomField only: no entry, an
	// entry with no or another $type, or such an entry next to a plain one, as
	// when the token cannot see the field. CheckStateField returns it
	// for a project and a State write returns it for an issue; nothing is
	// written.
	ErrStateFieldUnreadable = errors.New("issuetracker: the State field could not be read as a plain State field")
)

// packageSentinels lists every sentinel above. redactor.sentinel keeps only
// these in an error chain, so a new sentinel must be added here too.
var packageSentinels = []error{ErrUnsupportedKind, ErrProjectNotBound, ErrProjectMismatch, ErrStateMappingUnset, ErrColumnNotMapped, ErrStateNotAllowed, ErrInvalidLabelName, ErrRedirectRefused, ErrIssueResolved, ErrStateMachineField, ErrStateFieldUnreadable}
