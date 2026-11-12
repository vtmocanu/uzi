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
	// ErrInvalidLabelName is returned for a label name the issue tracker cannot carry.
	ErrInvalidLabelName = errors.New("issuetracker: invalid label name")
	// ErrRedirectRefused is returned when the server answers with a redirect.
	// The client never follows one, so the token cannot be forwarded.
	ErrRedirectRefused = errors.New("issuetracker: redirect refused")
)

// packageSentinels lists every sentinel above. redactor.sentinel keeps only
// these in an error chain, so a new sentinel must be added here too.
var packageSentinels = []error{ErrUnsupportedKind, ErrProjectNotBound, ErrProjectMismatch, ErrStateMappingUnset, ErrInvalidLabelName, ErrRedirectRefused}
