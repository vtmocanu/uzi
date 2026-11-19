package handler

import (
	"errors"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// writeTerminalConsumerGateRefusal maps the two terminal-consumer-gate refusals a gated run
// create can return (issue #4488, adr/4007-terminal-preservation.md "Caller semantics") and
// reports whether it wrote a response:
//
//   - workersvc.ErrTerminalConsumerGateBusy: a terminal-ref retirement briefly holds the
//     repository's consumer gate (or the wait / request context ended). Nothing was created, so
//     the caller may retry: 503 with Retry-After.
//   - workersvc.ErrTerminalConsumerGateIdentity: a target run vanished or is in another
//     repository. Retrying cannot help: 409.
//
// Every gated create site calls it before its own sentinel switch so contention can never fall
// through to a generic 500 or be mistaken for a permanent refusal.
func writeTerminalConsumerGateRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, workersvc.ErrTerminalConsumerGateBusy):
		w.Header().Set("Retry-After", "1")
		httpx.Error(w, http.StatusServiceUnavailable, "repository briefly busy (terminal ref retirement); retry")
		return true
	case errors.Is(err, workersvc.ErrTerminalConsumerGateIdentity):
		httpx.Error(w, http.StatusConflict, "the target run changed or no longer exists; reload and try again")
		return true
	}
	return false
}
