// Package forgesvc is the shared forge-sync service used by both the HTTP
// handlers and the background poller. It owns three things the two callers must
// agree on: building a forge driver from a stored (encrypted) connection, the
// PRD-link sanity check, and the full/incremental sync-into-cache logic.
package forgesvc

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// PRDLabel is the forge label that marks an issue as factory work. The board
// and every sync only ever fetch issues carrying it.
const PRDLabel = "PRD"

// DefaultColumns are the kanban columns seeded on the forge (as labels) the
// first time a repo's board is opened. Colors are required by GitLab's label
// create API; these are the common-board reference set.
var DefaultColumns = []forge.Label{
	{Name: "In Progress", Color: "#1f75cb"},
	{Name: "Upcoming", Color: "#6699cc"},
	{Name: "Later", Color: "#999999"},
}

// prdLinkRe matches a PRD reference in an issue description: a bare or
// blob-URL-prefixed path to a prds/<file>.md. Computed at fetch time; the
// description itself is never stored.
var prdLinkRe = regexp.MustCompile(`(?i)(?:https?://\S+/-/blob/[^\s)]+/)?prds/[\w.-]+\.md(?:[#?][^\s)]*)?`)

// HasPRDLink reports whether an issue description contains a PRD-file link.
func HasPRDLink(description string) bool {
	return prdLinkRe.MatchString(description)
}

// Service bundles the dependencies for building forge clients and syncing.
type Service struct {
	q       *store.Queries
	box     *secretbox.Box
	timeout time.Duration
}

// New constructs a Service. box encrypts/decrypts stored PATs; timeout bounds
// every forge HTTP call.
func New(q *store.Queries, box *secretbox.Box, timeout time.Duration) *Service {
	return &Service{q: q, box: box, timeout: timeout}
}

// EncryptToken seals a plaintext PAT for storage.
func (s *Service) EncryptToken(pat string) ([]byte, error) {
	return s.box.Seal([]byte(pat))
}

// ForgeForToken builds a driver from a plaintext token (the connect/verify path,
// before the token is stored).
func (s *Service) ForgeForToken(forgeType forge.Type, baseURL, token string) (forge.Forge, error) {
	return forge.New(forgeType, baseURL, token, s.timeout)
}

// ForgeForConnection builds a driver from a stored connection by decrypting its
// token ciphertext.
func (s *Service) ForgeForConnection(forgeType, baseURL string, tokenCiphertext []byte) (forge.Forge, error) {
	plain, err := s.box.Open(tokenCiphertext)
	if err != nil {
		return nil, err
	}
	return forge.New(forge.Type(forgeType), baseURL, string(plain), s.timeout)
}

// FullSync fetches the complete PRD-labeled set (state=all, no lower bound),
// upserts every issue into the cache, then evicts cache rows absent from the
// fresh set. This is the only path that observes de-labeling and deletion, so
// it doubles as the reconcile pass and the manual Refresh. Returns the max
// forge updated_at seen, so a caller tracking a high-water-mark can advance it.
func (s *Service) FullSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge) (time.Time, error) {
	issues, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{PRDLabel}})
	if err != nil {
		return time.Time{}, err
	}
	maxUpdated, err := s.upsertIssues(ctx, repoID, issues)
	if err != nil {
		return time.Time{}, err
	}
	keep := make([]int64, len(issues))
	for i, is := range issues {
		keep[i] = is.IID
	}
	if _, err := s.q.DeleteIssuesNotIn(ctx, store.DeleteIssuesNotInParams{RepoID: repoID, KeepIids: keep}); err != nil {
		return time.Time{}, err
	}
	return maxUpdated, nil
}

// IncrementalSync fetches only issues updated at/after hwm and upserts them. It
// cannot see de-labeling or deletion (the filter structurally excludes them) —
// that is FullSync's job. Returns the advanced high-water-mark (the larger of
// hwm and the max updated_at returned by the forge). The bound is inclusive at
// second granularity, so the boundary row is re-fetched and deduped by upsert.
func (s *Service) IncrementalSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge, hwm time.Time) (time.Time, error) {
	opts := forge.ListIssuesOptions{Labels: []string{PRDLabel}}
	if !hwm.IsZero() {
		opts.UpdatedAfter = &hwm
	}
	issues, err := f.ListIssues(ctx, forgeProjectID, opts)
	if err != nil {
		return hwm, err
	}
	maxUpdated, err := s.upsertIssues(ctx, repoID, issues)
	if err != nil {
		return hwm, err
	}
	if maxUpdated.After(hwm) {
		return maxUpdated, nil
	}
	return hwm, nil
}

// upsertIssues writes each forge issue into the cache, computing has_prd_link at
// write time. Returns the max forge updated_at across the batch.
func (s *Service) upsertIssues(ctx context.Context, repoID uuid.UUID, issues []forge.Issue) (time.Time, error) {
	var maxUpdated time.Time
	for _, is := range issues {
		labelsJSON, err := json.Marshal(is.Labels)
		if err != nil {
			return maxUpdated, err
		}
		author := pgtype.Text{}
		if is.Author != "" {
			author = pgtype.Text{String: is.Author, Valid: true}
		}
		updated := is.UpdatedAt
		if updated.IsZero() {
			updated = time.Now()
		}
		if updated.After(maxUpdated) {
			maxUpdated = updated
		}
		if _, err := s.q.UpsertIssue(ctx, store.UpsertIssueParams{
			RepoID:         repoID,
			ForgeIssueIid:  is.IID,
			Title:          is.Title,
			State:          is.State,
			Labels:         labelsJSON,
			WebUrl:         is.WebURL,
			Author:         author,
			HasPrdLink:     HasPRDLink(is.Description),
			ForgeUpdatedAt: pgtype.Timestamptz{Time: updated, Valid: true},
		}); err != nil {
			return maxUpdated, err
		}
	}
	return maxUpdated, nil
}
