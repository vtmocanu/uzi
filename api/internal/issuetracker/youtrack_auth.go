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

// MapsAuthor reports whether an issue tracker driver can say that an author
// login is the connection's own account, which is how D11 of the external issue
// trackers PRD decides that an issue or comment counts as uzi's. The consumer
// declares the interface it needs (issueinput.AuthorMapper, which this package
// does not import); this assertion only guards the method.
var _ interface {
	MapsAuthor(ctx context.Context, author string) (bool, error)
} = (*youTrack)(nil)

// MapsAuthor reports whether author, compared exactly, is the login of the
// account the token belongs to; an empty author never maps. A failed account
// lookup is an error and is not kept (see connAccount).
func (y *youTrack) MapsAuthor(ctx context.Context, author string) (bool, error) {
	if author == "" {
		return false, nil
	}
	a, err := y.connAccount(ctx)
	if err != nil {
		return false, err
	}
	return a.Login != "" && a.Login == author, nil
}
