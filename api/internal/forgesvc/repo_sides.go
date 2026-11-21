package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// IssueTrackerDrivers builds the driver of a linked external issue tracker for
// one repo binding. *issuetrackersvc.Service satisfies it. Optional on a Service:
// without it a linked repo fails closed (PRD #4173 DL-5).
type IssueTrackerDrivers interface {
	DriverFor(ctx context.Context, userID, connID uuid.UUID, binding issuetracker.Binding, states issuetracker.StateMapping) (issuetracker.Driver, error)
}

// SetIssueTrackerDrivers wires the linked-issue-tracker driver source.
func (s *Service) SetIssueTrackerDrivers(d IssueTrackerDrivers) { s.itDrivers = d }

// errNoRepoStore is returned by the resolvers on a Service built without queries: a
// resolver must fail, not dereference a nil store.
var errNoRepoStore = errors.New("forgesvc: no repo store wired")

// errRepoSourceRead marks a failed database read of the repo's issue source (not a missing
// row, which is permanent). It is the one UnavailableIssueTracker cause the board-free MR
// watch treats as transient, see SyncBoardFreeMRStatesWithIssueSide.
var errRepoSourceRead = errors.New("forgesvc: repo issue source read failed")

// repoSource reads the one owner-scoped row both resolvers use.
func (s *Service) repoSource(ctx context.Context, userID, repoID uuid.UUID) (store.GetRepoIssueSourceForUserRow, error) {
	if s.q == nil {
		return store.GetRepoIssueSourceForUserRow{}, errNoRepoStore
	}
	return s.q.GetRepoIssueSourceForUser(ctx, store.GetRepoIssueSourceForUserParams{ID: repoID, UserID: userID})
}

// ErrLinkedRepoUnresolvable is returned for a linked repo whose issue side
// cannot be built: no driver source wired, or a link whose connection is missing
// for the repo's owner. There is deliberately no fallback to the forge.
var ErrLinkedRepoUnresolvable = errors.New("repo is linked to an issue tracker that cannot be resolved")

// IssueTrackerForRepo returns the issue side of a repo (D5, D10 of PRD #4173),
// bound by repo row and owner, never by forge project number. An unlinked repo
// gets its forge driver, built as ForgeForConnection builds it. A linked repo
// gets the issue tracker driver for its stored binding, wrapped in the write
// guard; a link that cannot be resolved is an error.
func (s *Service) IssueTrackerForRepo(ctx context.Context, userID, repoID uuid.UUID) (forge.IssueTracker, error) {
	src, err := s.repoSource(ctx, userID, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errNoRepoStore) {
			return nil, fmt.Errorf("read repo issue source: %w", err)
		}
		return nil, fmt.Errorf("read repo issue source: %w: %w", errRepoSourceRead, err)
	}
	if !src.IssueTrackerConnectionID.Valid {
		f, err := s.ForgeForConnection(src.ForgeType, src.BaseUrl, src.TokenCiphertext)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	if !src.LinkedConnectionID.Valid || s.itDrivers == nil ||
		!src.IssueTrackerProjectID.Valid || !src.IssueTrackerProjectShort.Valid ||
		!src.EligibilityTagID.Valid || !src.EligibilityTagName.Valid {
		return nil, ErrLinkedRepoUnresolvable
	}
	var columns map[string]string
	if len(src.ColumnStates) > 0 {
		if err := json.Unmarshal(src.ColumnStates, &columns); err != nil {
			return nil, fmt.Errorf("decode stored column states: %w", err)
		}
	}
	d, err := s.itDrivers.DriverFor(ctx, userID, uuid.UUID(src.LinkedConnectionID.Bytes), issuetracker.Binding{
		Project:        issuetracker.ProjectRef{ID: src.IssueTrackerProjectID.String, ShortName: src.IssueTrackerProjectShort.String},
		ProjectID:      src.ForgeProjectID,
		EligibilityTag: issuetracker.TagRef{ID: src.EligibilityTagID.String, Name: src.EligibilityTagName.String},
	}, issuetracker.StateMapping{Open: src.OpenState.String, Closed: src.ClosedState.String, Columns: columns})
	if err != nil {
		return nil, fmt.Errorf("build linked issue tracker: %w", err)
	}
	return &issueWriteGuard{inner: d, eligibility: src.EligibilityTagName.String}, nil
}

// CodeHostForRepo returns the code-host side of a repo: always the repo's own
// forge driver, built as ForgeForConnection builds it, never reading the link (S2).
func (s *Service) CodeHostForRepo(ctx context.Context, userID, repoID uuid.UUID) (forge.CodeHost, error) {
	src, err := s.repoSource(ctx, userID, repoID)
	if err != nil {
		return nil, fmt.Errorf("read repo connection: %w", err)
	}
	f, err := s.ForgeForConnection(src.ForgeType, src.BaseUrl, src.TokenCiphertext)
	if err != nil {
		return nil, err
	}
	return f, nil
}
