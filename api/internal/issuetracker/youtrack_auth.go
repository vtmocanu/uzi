package issuetracker

import (
	"context"
	"net/http"
	"net/url"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// VerifyToken implements Driver. It asks YouTrack who the token belongs to.
func (y *youTrack) VerifyToken(ctx context.Context) (Account, error) {
	return y.fetchAccount(ctx)
}

// connAccount returns the account the token belongs to, asking YouTrack once
// and keeping the answer for the driver's lifetime. Tag ownership is judged
// against this account, never against a stored id. A failed lookup is not kept.
func (y *youTrack) connAccount(ctx context.Context) (Account, error) {
	y.accountMu.Lock()
	defer y.accountMu.Unlock()
	if y.account != nil {
		return *y.account, nil
	}
	a, err := y.fetchAccount(ctx)
	if err != nil {
		return Account{}, err
	}
	if !entityIDRE.MatchString(a.ID) {
		return Account{}, y.fail("youtrack: account with a malformed id in the response")
	}
	y.account = &a
	return a, nil
}

func (y *youTrack) fetchAccount(ctx context.Context) (Account, error) {
	var u ytUser
	_, _, err := y.do(ctx, ytCall{
		method: http.MethodGet,
		path:   []string{"users", "me"},
		query:  url.Values{"fields": {"id,login"}},
		limit:  objectBytes,
		prov:   forge.ResponseSuccess,
	}, &u)
	if err != nil {
		return Account{}, err
	}
	return Account(u), nil
}
