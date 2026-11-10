package forgesvc

import (
	"context"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// IssueTrackerForRepo returns the issue side of a repo (D8 of the IssueTracker /
// CodeHost split). Phase 1: the repo's own forge driver, built exactly as
// ForgeForConnection builds it. Phase 2 will map repoID to an external tracker.
// The caller has already resolved the connection through its own owner-scoped
// lookup, so forgeType, baseURL and tokenCiphertext come from that row.
func (s *Service) IssueTrackerForRepo(ctx context.Context, repoID uuid.UUID, forgeType, baseURL string, tokenCiphertext []byte) (forge.IssueTracker, error) {
	f, err := s.ForgeForConnection(forgeType, baseURL, tokenCiphertext)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// CodeHostForRepo returns the code-host side of a repo: always the repo's own
// forge driver, built as ForgeForConnection builds it. See IssueTrackerForRepo
// for the connection-resolution contract.
func (s *Service) CodeHostForRepo(ctx context.Context, repoID uuid.UUID, forgeType, baseURL string, tokenCiphertext []byte) (forge.CodeHost, error) {
	f, err := s.ForgeForConnection(forgeType, baseURL, tokenCiphertext)
	if err != nil {
		return nil, err
	}
	return f, nil
}
