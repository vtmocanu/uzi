package codexauth

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Timeout tests inspect the actual outbound request context through an injected doer;
// they never wait for a timer to expire.

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

func TestRefreshTimeoutIsSeparateFromReadTimeouts(t *testing.T) {
	for _, operation := range []string{"refresh", "identity", "usage"} {
		t.Run(operation, func(t *testing.T) {
			want := 2500 * time.Millisecond
			if operation == "refresh" {
				want = 5 * time.Second
			}
			var sawRequest bool
			doer := doerFunc(func(req *http.Request) (*http.Response, error) {
				sawRequest = true
				deadline, ok := req.Context().Deadline()
				if !ok {
					t.Fatal("outbound request has no deadline")
				}
				if remaining := time.Until(deadline); remaining < want-250*time.Millisecond || remaining > want {
					t.Fatalf("outbound deadline remaining = %s, want approximately %s", remaining, want)
				}
				if operation == "refresh" {
					return jsonResponse(http.StatusOK, `{"access_token":"`+assembleToken("access")+`"}`), nil
				}
				return jsonResponse(http.StatusOK, `{"user_id":"u","account_id":"a","rate_limit":{"allowed":true}}`), nil
			})
			c := NewClient(WithHTTPDoer(doer),
				WithRefreshTimeout(5*time.Second), WithPerRequestTimeout(2500*time.Millisecond))
			var err error
			switch operation {
			case "refresh":
				_, err = c.Refresh(context.Background(), assembleToken("refresh"))
			case "identity":
				_, err = c.DiscoverIdentity(context.Background(), assembleToken("access"))
			case "usage":
				_, err = c.ReadUsage(context.Background(), assembleToken("access"), "a")
			}
			if err != nil {
				t.Fatal(err)
			}
			if !sawRequest {
				t.Fatal("doer was not called")
			}
		})
	}
}

func TestRefreshTimeoutFallbackAndParentDeadline(t *testing.T) {
	for _, tc := range []struct {
		name       string
		options    []Option
		want       time.Duration
		withParent bool
	}{
		{name: "default"},
		{name: "zero", options: []Option{WithRefreshTimeout(0)}},
		{name: "negative", options: []Option{WithRefreshTimeout(-time.Second)}},
		{name: "default falls back", options: []Option{WithPerRequestTimeout(2500 * time.Millisecond)}, want: 2500 * time.Millisecond},
		{name: "zero falls back", options: []Option{WithPerRequestTimeout(2500 * time.Millisecond), WithRefreshTimeout(0)}, want: 2500 * time.Millisecond},
		{name: "negative falls back", options: []Option{WithPerRequestTimeout(2500 * time.Millisecond), WithRefreshTimeout(-time.Second)}, want: 2500 * time.Millisecond},
		{name: "nonpositive preserves override", options: []Option{WithRefreshTimeout(5 * time.Second), WithRefreshTimeout(0), WithRefreshTimeout(-time.Second)}, want: 5 * time.Second},
		{name: "earlier parent wins", options: []Option{WithPerRequestTimeout(2500 * time.Millisecond), WithRefreshTimeout(5 * time.Second)}, withParent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			parentDeadline := time.Now().Add(time.Second)
			if tc.withParent {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, parentDeadline)
				defer cancel()
			}
			var sawRequest bool
			doer := doerFunc(func(req *http.Request) (*http.Response, error) {
				sawRequest = true
				deadline, ok := req.Context().Deadline()
				switch {
				case tc.withParent:
					if !ok || !deadline.Equal(parentDeadline) {
						t.Fatalf("deadline = %v (%t), want parent %v", deadline, ok, parentDeadline)
					}
				case tc.want == 0:
					if ok || req.Context() != ctx {
						t.Fatal("default/nonpositive timeout must leave context untouched")
					}
				default:
					remaining := time.Until(deadline)
					if !ok || remaining < tc.want-250*time.Millisecond || remaining > tc.want {
						t.Fatalf("deadline remaining = %s (%t), want approximately %s", remaining, ok, tc.want)
					}
				}
				return jsonResponse(http.StatusOK, `{"access_token":"`+assembleToken("access")+`"}`), nil
			})
			options := append([]Option{WithHTTPDoer(doer)}, tc.options...)
			if _, err := NewClient(options...).Refresh(ctx, assembleToken("refresh")); err != nil {
				t.Fatal(err)
			}
			if !sawRequest {
				t.Fatal("doer was not called")
			}
		})
	}
}
