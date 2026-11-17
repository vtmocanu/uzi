package issuetrackersvc

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

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
//
// Reconnecting never changes the account of a connection that a repo links to
// (PRD #4173 D11, D14): the new token must belong to the stored account, else the
// result is a Conflict. The check takes the row lock FOR UPDATE and then counts the
// linked repos in a separate statement; the upsert's own WHERE is only a last defence.
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
	if err := validateAccount(account, token); err != nil {
		return Connection{}, &Error{Kind: KindBadGateway, Message: err.Error()}
	}

	sealed, err := sealToken(s.box, userID, parsedKind, norm, token)
	if err != nil {
		s.log.Error("seal issue tracker token", "error", err)
		return Connection{}, internal()
	}
	params := store.UpsertIssueTrackerConnectionParams{
		UserID:          userID,
		Kind:            string(parsedKind),
		BaseUrl:         norm,
		AccountID:       account.ID,
		AccountLogin:    account.Login,
		TokenCiphertext: sealed,
	}
	var row store.IssueTrackerConnection
	err = s.inTx(ctx, func(q Querier) error {
		// The existing row is locked FOR UPDATE first, so a link (which holds the row
		// FOR SHARE) and this reconnect cannot interleave: the link either commits
		// before the count below and is seen by it, or the lock waits for it.
		//
		// When no row exists yet the row is inserted with ON CONFLICT DO NOTHING. If
		// someone else inserted it meanwhile that returns no row, and the locked path
		// is entered once more: the row exists now, may already carry a link, and only
		// the lock and a fresh count statement can see that. An upsert's own WHERE
		// cannot: its subquery reads the statement's snapshot, taken before the wait
		// on a link's lock, so it is kept only as a last defence (see the query).
		for attempt := 0; ; attempt++ {
			existing, err := q.LockIssueTrackerConnectionByIdentityForUpdate(ctx, store.LockIssueTrackerConnectionByIdentityForUpdateParams{
				UserID: userID, Kind: string(parsedKind), BaseUrl: norm,
			})
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				if attempt == 0 {
					row, err = q.InsertIssueTrackerConnectionIfAbsent(ctx, store.InsertIssueTrackerConnectionIfAbsentParams(params))
					if err == nil {
						return nil // a first connect: nothing can link to a row that did not exist
					}
					if errors.Is(err, pgx.ErrNoRows) {
						continue // inserted by another connect meanwhile: lock it and count
					}
					return err
				}
				// Still no row to lock after the re-entry: the row the insert lost to was
				// deleted meanwhile. Fall through to the upsert, which inserts a row with a
				// new id that no link can reference yet. Accepted boundary: if a third
				// connect re-inserts the row and a link commits on it while this upsert
				// waits, only the upsert's weaker WHERE stands in the way (see the query).
			case err != nil:
				return err
			case existing.AccountID != account.ID:
				// A separate statement after the lock, so READ COMMITTED sees every link
				// committed before the lock was granted.
				n, err := q.CountReposLinkedToIssueTrackerConnection(ctx, existing.ID)
				if err != nil {
					return err
				}
				if n > 0 {
					return errReconnectChangesAccount()
				}
			}
			row, err = q.UpsertIssueTrackerConnection(ctx, params)
			if errors.Is(err, pgx.ErrNoRows) {
				// The upsert's WHERE refused (defence in depth; see the query).
				return errReconnectChangesAccount()
			}
			return err
		}
	})
	if err != nil {
		return Connection{}, s.txError("connect issue tracker", err)
	}
	return toConnection(row), nil
}

func errReconnectChangesAccount() *Error {
	return conflict("this connection is linked to a repo and the new token belongs to a different account; " +
		"use a token of the account the connection already stands for, or connect to a different base URL")
}
