package issuetracker

import "time"

// ProjectRef identifies the project a connection is bound to.
type ProjectRef struct {
	ID        string
	ShortName string
}

// TagRef identifies a tag by its server-side id and its name.
type TagRef struct {
	ID   string
	Name string
}

// Binding is what ties a driver to one repository's project: the issue
// tracker project, the repository's forge project id the driver answers to, and
// the eligibility tag. Either all three are set or none is; a connection-level
// driver has none (see ValidBinding).
type Binding struct {
	Project        ProjectRef
	ProjectID      int64
	EligibilityTag TagRef
}

// Options configures a driver. There is deliberately no TLS field: a driver
// trusts the system roots and nothing else (PRD #4173 S3).
type Options struct {
	// BaseURL is the issue tracker's root URL; the caller has already validated it.
	BaseURL string
	// Token is the permanent token. It never appears in a returned error.
	Token string
	// Project is the project the connection is bound to.
	Project ProjectRef
	// ProjectID is the repository's forge project id. Every forge.IssueTracker
	// method refuses a different id with ErrProjectMismatch (PRD #4173 D10).
	ProjectID int64
	// EligibilityTag is the tag that marks an issue as eligible. Its label name
	// always resolves to this id, whoever owns the tag (PRD #4173 D14).
	EligibilityTag TagRef
	// StateField, OpenState and ClosedState map neutral open/closed onto the
	// issue tracker's own state field and values.
	StateField  string
	OpenState   string
	ClosedState string
	// Timeout bounds one HTTP request; zero selects defaultTimeout.
	Timeout time.Duration
}
