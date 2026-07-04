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

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// PRDLabel is the forge label that marks an issue as factory work. The board
// and every sync only ever fetch issues carrying it.
const PRDLabel = "PRD"

// DefaultColumns are the kanban columns seeded on the forge (as labels) the
// first time a repo's board is opened. Colors are required by GitLab's label
// create API. In Progress / Upcoming / Later are the common-board reference set;
// Human Review (PRD #12) is where the automation parks a card once its MR is
// open. GetBoard also ensures Human Review on boards seeded before it existed.
var DefaultColumns = []forge.Label{
	{Name: board.ColumnInProgress, Color: "#1f75cb"},
	{Name: "Upcoming", Color: "#6699cc"},
	{Name: "Later", Color: "#999999"},
	{Name: board.ColumnHumanReview, Color: "#6e49cb"},
}

// prdLinkRe matches a PRD reference in an issue description: a bare or
// blob-URL-prefixed path to a prds/.../<file>.md, allowing subdirectories
// (e.g. prds/done/1-foo.md). Computed at fetch time; the description itself is
// never stored.
var prdLinkRe = regexp.MustCompile(`(?i)(?:https?://\S+/-/blob/[^\s)]+/)?prds/(?:[\w.-]+/)*[\w.-]+\.md(?:[#?][^\s)]*)?`)

// HasPRDLink reports whether an issue description contains a PRD-file link.
func HasPRDLink(description string) bool {
	return prdLinkRe.MatchString(description)
}

// IssueStore is the subset of store methods the sync paths need. Narrowing to an
// interface lets the sync logic be unit-tested against a fake store (and a
// mocked Forge) without a live database. *store.Queries satisfies it.
type IssueStore interface {
	UpsertIssue(ctx context.Context, arg store.UpsertIssueParams) (store.Issue, error)
	DeleteIssuesNotIn(ctx context.Context, arg store.DeleteIssuesNotInParams) (int64, error)
}

// Service bundles the dependencies for building forge clients and syncing.
type Service struct {
	q       IssueStore
	box     *secretbox.Box
	timeout time.Duration
}

// New constructs a Service. box encrypts/decrypts stored PATs; timeout bounds
// every forge HTTP call.
func New(q IssueStore, box *secretbox.Box, timeout time.Duration) *Service {
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

// AutoMove applies a single-column move forge-first, then updates the issue
// cache — the mechanic the board drag and the run-lifecycle automation share
// (both need the same "one atomic label swap, then snapshot" behavior). It plans
// the add/remove sets against the repo's column set, calls UpdateIssueLabels once
// (GitLab makes that atomic, which is what enforces single-column membership),
// and only on forge success upserts the new label set. A forge error is returned
// with the cache untouched, so a failed move never desyncs the cache from the
// forge; the caller decides whether that fails a request (manual drag) or leaves
// a pending marker for reconciliation (automation). The returned issue is the
// re-cached row. Guards (closed issue, manual-drag) live in the caller, not here:
// a manual drag is itself the human's intent and must not be second-guessed.
func (s *Service) AutoMove(ctx context.Context, f forge.Forge, forgeProjectID int64, issue store.Issue, columns []store.BoardColumn, target string) (store.Issue, error) {
	var current []string
	if err := json.Unmarshal(issue.Labels, &current); err != nil {
		current = []string{}
	}
	columnSet := make(map[string]struct{}, len(columns))
	for _, c := range columns {
		columnSet[c.LabelName] = struct{}{}
	}
	add, remove, newLabels := board.PlanLabelMove(current, columnSet, target)

	// Forge-first: apply the label change remotely before touching the cache. On
	// failure the cache is untouched (UpdateIssueLabels no-ops on empty sets, so a
	// move to a column the card already sits in costs no forge call).
	if err := f.UpdateIssueLabels(ctx, forgeProjectID, issue.ForgeIssueIid, add, remove); err != nil {
		return store.Issue{}, err
	}

	labelsJSON, err := json.Marshal(newLabels)
	if err != nil {
		return store.Issue{}, err
	}
	return s.q.UpsertIssue(ctx, store.UpsertIssueParams{
		RepoID:         issue.RepoID,
		ForgeIssueIid:  issue.ForgeIssueIid,
		Title:          issue.Title,
		State:          issue.State,
		Labels:         labelsJSON,
		WebUrl:         issue.WebUrl,
		Author:         issue.Author,
		HasPrdLink:     issue.HasPrdLink,
		ForgeUpdatedAt: issue.ForgeUpdatedAt,
	})
}

// FullSync fetches the complete PRD-labeled set (state=all, no lower bound),
// upserts every issue into the cache, then evicts cache rows absent from the
// fresh set. This is the only path that observes de-labeling and deletion, so
// it doubles as the reconcile pass and the manual Refresh. Returns the max
// forge updated_at seen, so a caller tracking a high-water-mark can advance it.
func (s *Service) FullSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge) (time.Time, error) {
	issues, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{PRDLabel}})
	if err != nil {
		// Abort BEFORE any eviction: a failed/partial fetch must never be
		// treated as an authoritative empty set, or a transient forge error
		// would wipe the cache. Eviction only runs below, after a clean fetch.
		return time.Time{}, err
	}
	maxUpdated, err := s.upsertIssues(ctx, repoID, issues)
	if err != nil {
		return time.Time{}, err
	}
	// A clean fetch that legitimately returns zero PRD issues DOES evict
	// everything — the forge is the source of truth (empty means empty).
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
