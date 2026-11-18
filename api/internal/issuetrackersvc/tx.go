package issuetrackersvc

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Postgres SQLSTATEs the service maps. UZ413 is the issue-number guard's own
// code (migration 00474).
const (
	sqlStateForeignKeyViolation = "23503"
	sqlStateIssueNumberGuard    = "UZ413"
)

// TxBeginner opens a transaction; *pgxpool.Pool satisfies it.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// UseTransactions makes the link and reconnect rules atomic: begin opens a
// transaction and bind returns the Querier whose statements run in it
// (store.Queries.WithTx in production). A Service without it runs each unit of
// work directly on its Querier, which is atomic only if that Querier is, so
// production wiring (cmd/server) always sets it. It returns s for chaining and
// must be called before the Service is shared.
func (s *Service) UseTransactions(begin TxBeginner, bind func(pgx.Tx) Querier) *Service {
	s.begin, s.bind = begin, bind
	return s
}

// inTx runs fn in one transaction (READ COMMITTED, the pool default), rolling
// back when fn fails and committing otherwise.
func (s *Service) inTx(ctx context.Context, fn func(Querier) error) error {
	if s.begin == nil || s.bind == nil {
		return fn(s.q)
	}
	tx, err := s.begin.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after a successful Commit
	if err := fn(s.bind(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// txError turns an inTx failure into the typed error a caller returns: a typed
// *Error passes through, the issue-number guard's UZ413 and a foreign key
// violation are conflicts, and anything else is logged and becomes Internal.
func (s *Service) txError(what string, err error) *Error {
	if te := (*Error)(nil); errors.As(err, &te) {
		return te
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == sqlStateIssueNumberGuard || pe.Code == sqlStateForeignKeyViolation) {
		return conflict("the repo or connection changed while this was being done; retry")
	}
	s.log.Error(what, "error", err)
	return internal()
}

func isForeignKeyViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == sqlStateForeignKeyViolation
}

var errConnectionLinked = conflict("this connection is linked to a repo and cannot be deleted while it is linked; a link cannot be removed")
