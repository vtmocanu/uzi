package issuetrackersvc

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Connect verifies a token against the issue tracker and stores it sealed for
// the user, creating the connection or rotating the existing one for the same
// (user, kind, base URL). The order is fixed: kind, token presence, token shape
// (issuetracker.ValidateToken), strict base URL, allowlist, driver build, token
// verification, account check, seal, upsert. A refused base URL never reaches the driver factory, so no outbound
// request or token use happens for it. The token is trimmed of surrounding
// whitespace, and it appears in no returned error or log line.
func (s *Service) Connect(ctx context.Context, userID uuid.UUID, kind, baseURL, token string) (Connection, error) {
	parsedKind, ok := issuetracker.ParseKind(strings.TrimSpace(kind))
	if !ok {
		return Connection{}, &Error{Kind: KindBadRequest, Message: "unsupported issue tracker kind; supported values: " + issuetracker.SupportedKindsList()}
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return Connection{}, &Error{Kind: KindBadRequest, Message: "a token is required"}
	}
	if err := issuetracker.ValidateToken(token); err != nil {
		// ValidateToken's message names the rule that failed and never
		// includes the token, so it is passed through.
		return Connection{}, &Error{Kind: KindBadRequest, Message: "invalid token: " + err.Error()}
	}
	norm, err := strictBaseURL(baseURL)
	if err != nil {
		return Connection{}, &Error{Kind: KindBadRequest, Message: err.Error()}
	}
	if !s.allowed(norm) {
		return Connection{}, &Error{Kind: KindBadRequest, Message: "base URL is not in the allowed issue tracker list"}
	}

	d, err := s.newDriver(parsedKind, issuetracker.Options{BaseURL: norm, Token: token})
	if err != nil {
		s.log.Error("build issue tracker driver", "kind", string(parsedKind), "error", err)
		// The driver's build errors never carry the token.
		return Connection{}, &Error{Kind: KindBadRequest, Message: "invalid token or connection settings: " + err.Error()}
	}
	account, err := d.VerifyToken(ctx)
	if err != nil {
		// err is already token-redacted by the driver.
		return Connection{}, &Error{Kind: KindBadGateway, Message: "token verification failed: " + err.Error()}
	}
	if err := validateAccount(account); err != nil {
		return Connection{}, &Error{Kind: KindBadGateway, Message: err.Error()}
	}

	sealed, err := sealToken(s.box, userID, parsedKind, norm, token)
	if err != nil {
		s.log.Error("seal issue tracker token", "error", err)
		return Connection{}, internal()
	}
	row, err := s.q.UpsertIssueTrackerConnection(ctx, store.UpsertIssueTrackerConnectionParams{
		UserID:          userID,
		Kind:            string(parsedKind),
		BaseUrl:         norm,
		AccountID:       account.ID,
		AccountLogin:    account.Login,
		TokenCiphertext: sealed,
	})
	if err != nil {
		s.log.Error("upsert issue tracker connection", "error", err)
		return Connection{}, internal()
	}
	return toConnection(row), nil
}
