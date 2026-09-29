package forge

import (
	"errors"
	"net/http"
)

// createIssueRejection marks a response that definitively rejected issue creation.
// Its cause is already redacted; the raw SDK error is never retained.
type createIssueRejection struct{ err error }

func (e *createIssueRejection) Error() string { return e.err.Error() }
func (e *createIssueRejection) Unwrap() error { return e.err }

// IsCreateIssueDefinitiveRejection reports whether the forge conclusively rejected
// a CreateIssue request. A false result leaves the outcome unknown (including
// timeouts, transport errors, rate limits and failures before the POST).
func IsCreateIssueDefinitiveRejection(err error) bool {
	var rejection *createIssueRejection
	return errors.As(err, &rejection)
}

func definitiveCreateIssueStatus(status int) bool {
	// A 403 may be an abuse or secondary rate limit even without a usable
	// retry header. Treat it conservatively across all forge providers.
	return status >= http.StatusBadRequest && status < http.StatusInternalServerError &&
		status != http.StatusForbidden && status != http.StatusRequestTimeout &&
		status != http.StatusConflict && status != http.StatusTooManyRequests
}

func createIssueError(status int, err error) error {
	if definitiveCreateIssueStatus(status) {
		return &createIssueRejection{err: err}
	}
	return err
}
