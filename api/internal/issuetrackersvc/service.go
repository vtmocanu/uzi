package issuetrackersvc

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Querier is the slice of *store.Queries the service uses.
type Querier interface {
	UpsertIssueTrackerConnection(ctx context.Context, arg store.UpsertIssueTrackerConnectionParams) (store.IssueTrackerConnection, error)
	InsertIssueTrackerConnectionIfAbsent(ctx context.Context, arg store.InsertIssueTrackerConnectionIfAbsentParams) (store.IssueTrackerConnection, error)
	ListIssueTrackerConnectionsByUser(ctx context.Context, userID uuid.UUID) ([]store.IssueTrackerConnection, error)
	GetIssueTrackerConnectionForUser(ctx context.Context, arg store.GetIssueTrackerConnectionForUserParams) (store.IssueTrackerConnection, error)
	DeleteIssueTrackerConnectionForUser(ctx context.Context, arg store.DeleteIssueTrackerConnectionForUserParams) (int64, error)
	LockIssueTrackerConnectionByIdentityForUpdate(ctx context.Context, arg store.LockIssueTrackerConnectionByIdentityForUpdateParams) (store.IssueTrackerConnection, error)
	LockIssueTrackerConnectionForShareForUser(ctx context.Context, arg store.LockIssueTrackerConnectionForShareForUserParams) (store.IssueTrackerConnection, error)
	CountReposLinkedToIssueTrackerConnection(ctx context.Context, connectionID uuid.UUID) (int64, error)
	GetRepoIssueSourceForUser(ctx context.Context, arg store.GetRepoIssueSourceForUserParams) (store.GetRepoIssueSourceForUserRow, error)
	LockRepoForLinkForUser(ctx context.Context, arg store.LockRepoForLinkForUserParams) (store.LockRepoForLinkForUserRow, error)
	RepoStoresIssueNumber(ctx context.Context, repoID uuid.UUID) (bool, error)
	SetRepoIssueTrackerLink(ctx context.Context, arg store.SetRepoIssueTrackerLinkParams) (store.Repo, error)
}

var _ Querier = (*store.Queries)(nil)

// NewDriverFunc builds a driver; issuetracker.New is the production value.
type NewDriverFunc func(issuetracker.Kind, issuetracker.Options) (issuetracker.Driver, error)

// Service owns the connection rules. It is safe for concurrent use when its
// Querier is.
type Service struct {
	q         Querier
	box       *secretbox.Box
	allowed   func(string) bool
	newDriver NewDriverFunc
	log       *slog.Logger
	// begin and bind run a unit of work in one transaction (see UseTransactions).
	begin TxBeginner
	bind  func(pgx.Tx) Querier
}

// New builds a Service. allowed reports whether a normalized base URL is on the
// issue tracker allowlist (config.Config.IssueTrackerBaseURLAllowed); a nil
// allowed refuses everything. A nil newDriver selects issuetracker.New.
func New(q Querier, box *secretbox.Box, allowed func(string) bool, newDriver NewDriverFunc) *Service {
	if newDriver == nil {
		newDriver = issuetracker.New
	}
	if allowed == nil {
		allowed = func(string) bool { return false }
	}
	return &Service{q: q, box: box, allowed: allowed, newDriver: newDriver, log: slog.Default()}
}
