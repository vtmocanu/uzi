package forge

import (
	"context"
	"errors"
	"net"
	"net/http"

	gh "github.com/google/go-github/v92/github"
	gitlab "gitlab.com/gitlab-org/api/client-go/v3"
)

// ErrorClass is a forge-neutral outcome. Class normalizes values to this closed set.
type ErrorClass string

const (
	// ErrorClassTimeout identifies a deadline or network timeout.
	ErrorClassTimeout ErrorClass = "timeout"
	// ErrorClassAuth identifies an authentication or permission rejection.
	ErrorClassAuth ErrorClass = "auth"
	// ErrorClassServerError identifies an HTTP 5xx response.
	ErrorClassServerError ErrorClass = "server_error"
	// ErrorClassRateLimited identifies a typed rate limit or HTTP 429 response.
	ErrorClassRateLimited ErrorClass = "rate_limited"
	// ErrorClassOther identifies an outcome without a recognized class.
	ErrorClassOther ErrorClass = "other"
)

// Class reads an outcome through safe error wraps. Missing or unknown classes,
// including nil errors, return other; arbitrary class strings never escape.
func Class(err error) ErrorClass {
	var classified interface{ Class() ErrorClass }
	if errors.As(err, &classified) {
		switch c := classified.Class(); c {
		case ErrorClassTimeout, ErrorClassAuth, ErrorClassServerError, ErrorClassRateLimited:
			return c
		}
	}
	return ErrorClassOther
}

// classifiedError retains only a scrubbed message and a normalized outcome.
type classifiedError struct {
	message string
	class   ErrorClass
}

func (e *classifiedError) Error() string     { return e.message }
func (e *classifiedError) Class() ErrorClass { return e.class }

// classifyError inspects raw typed errors before the redactor severs their chain.
// status is supplied where an SDK returns HTTP metadata separately.
func classifyError(err error, status int) ErrorClass {
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrorClassTimeout
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return ErrorClassTimeout
	}
	var primary *gh.RateLimitError
	var abuse *gh.AbuseRateLimitError
	var neutral *RateLimitError
	if errors.As(err, &primary) || errors.As(err, &abuse) || errors.As(err, &neutral) {
		return ErrorClassRateLimited
	}
	if status == 0 {
		var githubError *gh.ErrorResponse
		var gitlabError *gitlab.ErrorResponse
		switch {
		case errors.As(err, &githubError) && githubError.Response != nil:
			status = githubError.Response.StatusCode
		case errors.As(err, &gitlabError):
			status = gitlabError.StatusCode
		}
	}
	switch {
	case status == http.StatusTooManyRequests:
		return ErrorClassRateLimited
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ErrorClassAuth
	case status >= 500 && status < 600:
		return ErrorClassServerError
	}
	return Class(err)
}
