package issuetrackersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// List returns the user's connections, oldest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Connection, error) {
	rows, err := s.q.ListIssueTrackerConnectionsByUser(ctx, userID)
	if err != nil {
		s.log.Error("list issue tracker connections", "error", err)
		return nil, internal()
	}
	out := make([]Connection, 0, len(rows))
	for _, r := range rows {
		out = append(out, toConnection(r))
	}
	return out, nil
}

// Get returns one of the user's connections; a missing or foreign id is NotFound.
func (s *Service) Get(ctx context.Context, userID, connID uuid.UUID) (Connection, error) {
	row, err := s.getRow(ctx, userID, connID)
	if err != nil {
		return Connection{}, err
	}
	return toConnection(row), nil
}

// Delete removes one of the user's connections; a missing or foreign id is NotFound.
func (s *Service) Delete(ctx context.Context, userID, connID uuid.UUID) error {
	n, err := s.q.DeleteIssueTrackerConnectionForUser(ctx, store.DeleteIssueTrackerConnectionForUserParams{ID: connID, UserID: userID})
	if err != nil {
		s.log.Error("delete issue tracker connection", "error", err)
		return internal()
	}
	if n == 0 {
		return notFound()
	}
	return nil
}

// getRow loads the stored row, ciphertext included, for in-package use only.
func (s *Service) getRow(ctx context.Context, userID, connID uuid.UUID) (store.IssueTrackerConnection, error) {
	row, err := s.q.GetIssueTrackerConnectionForUser(ctx, store.GetIssueTrackerConnectionForUserParams{ID: connID, UserID: userID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.IssueTrackerConnection{}, notFound()
	}
	if err != nil {
		s.log.Error("get issue tracker connection", "error", err)
		return store.IssueTrackerConnection{}, internal()
	}
	return row, nil
}
