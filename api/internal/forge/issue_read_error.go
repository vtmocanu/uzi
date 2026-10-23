package forge

import "net/http"

// IssueNotFoundError marks a GetIssue HTTP 404 and retains only an already-redacted error.
type IssueNotFoundError struct {
	Err error
}

func (e *IssueNotFoundError) Error() string { return e.Err.Error() }
func (e *IssueNotFoundError) Unwrap() error { return e.Err }

// issueReadError must receive the driver's already-redacted error, never an SDK error.
func issueReadError(status int, redacted error) error {
	if status == http.StatusNotFound && redacted != nil {
		return &IssueNotFoundError{Err: redacted}
	}
	return redacted
}
