package pushbroker

import (
	"context"
	"errors"
	"net/http"

	"github.com/vtmocanu/uzi/api/internal/redirectguard"
)

type withoutRedirectsKey struct{}

// WithoutRedirects binds prerequisite Git observations to the validated URL.
// REST validation and Git requests remain separate observations, not an atomic
// guarantee against a concurrent forge rename.
func WithoutRedirects(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutRedirectsKey{}, true)
}

// CheckRedirect retains the ordinary broker policy unless a prerequisite caller
// explicitly forbids redirects, including redirects to another same-origin path.
func CheckRedirect(req *http.Request, via []*http.Request) error {
	return checkRedirect(req.Context(), req, via)
}

func checkRedirect(ctx context.Context, req *http.Request, via []*http.Request) error {
	if marked, _ := ctx.Value(withoutRedirectsKey{}).(bool); marked {
		return errors.New("pushbroker: prerequisite redirect refused")
	}
	return redirectguard.SameOrigin(req, via)
}
