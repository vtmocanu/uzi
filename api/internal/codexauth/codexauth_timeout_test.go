package codexauth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// PRD #1171 M1: WithPerRequestTimeout is the production knob that bounds each provider call
// under the app-server callback deadline. These tests prove the option actually installs a
// per-request context deadline (and that omitting it leaves the context untouched, the
// pre-#1171 behaviour) — the fake doer inspects the request context directly.

// doerFunc adapts a func to the (unexported) httpDoer seam. Legal here because the test is
// in package codexauth.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestWithPerRequestTimeoutSetsDeadlineOnRefresh(t *testing.T) {
	var sawReq, hadDeadline bool
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		sawReq = true
		_, hadDeadline = req.Context().Deadline()
		return jsonResponse(http.StatusOK, `{"access_token":"`+assembleToken("access")+`"}`), nil
	})
	c := NewClient(WithHTTPDoer(doer), WithPerRequestTimeout(2*time.Second))

	if _, err := c.Refresh(context.Background(), assembleToken("refresh")); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !sawReq {
		t.Fatal("the doer was never called")
	}
	if !hadDeadline {
		t.Fatal("WithPerRequestTimeout must impose a context deadline on the provider request")
	}
}

func TestWithPerRequestTimeoutSetsDeadlineOnDiscover(t *testing.T) {
	var hadDeadline bool
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		_, hadDeadline = req.Context().Deadline()
		return jsonResponse(http.StatusOK, `{"user_id":"u","account_id":"a"}`), nil
	})
	c := NewClient(WithHTTPDoer(doer), WithPerRequestTimeout(2*time.Second))

	if _, err := c.DiscoverIdentity(context.Background(), assembleToken("access")); err != nil {
		t.Fatalf("DiscoverIdentity: %v", err)
	}
	if !hadDeadline {
		t.Fatal("WithPerRequestTimeout must impose a context deadline on the identity request")
	}
}

func TestNoPerRequestTimeoutLeavesContextUntouched(t *testing.T) {
	var hadDeadline bool
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		_, hadDeadline = req.Context().Deadline()
		return jsonResponse(http.StatusOK, `{"user_id":"u","account_id":"a"}`), nil
	})
	// No WithPerRequestTimeout: a background context has no deadline, and the client must not
	// invent one (preserves the pre-#1171 dark-M1 behaviour).
	c := NewClient(WithHTTPDoer(doer))

	if _, err := c.DiscoverIdentity(context.Background(), assembleToken("access")); err != nil {
		t.Fatalf("DiscoverIdentity: %v", err)
	}
	if hadDeadline {
		t.Fatal("without the option, the client must not impose a per-request deadline")
	}
}

func TestPerRequestTimeoutIgnoresNonPositive(t *testing.T) {
	var hadDeadline bool
	doer := doerFunc(func(req *http.Request) (*http.Response, error) {
		_, hadDeadline = req.Context().Deadline()
		return jsonResponse(http.StatusOK, `{"user_id":"u","account_id":"a"}`), nil
	})
	// A zero/negative value is ignored, so an accidental zero never disables the deadline
	// path in a surprising way — it just behaves like "no per-request timeout configured".
	c := NewClient(WithHTTPDoer(doer), WithPerRequestTimeout(0))

	if _, err := c.DiscoverIdentity(context.Background(), assembleToken("access")); err != nil {
		t.Fatalf("DiscoverIdentity: %v", err)
	}
	if hadDeadline {
		t.Fatal("a non-positive per-request timeout must be ignored (no deadline imposed)")
	}
}
