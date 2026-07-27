// Package forgesvc is the shared forge-sync service used by both the HTTP
// handlers and the background poller. It owns three things the two callers must
// agree on: building a forge driver from a stored (encrypted) connection, the
// PRD-link sanity check, and the full/incremental sync-into-cache logic.
package forgesvc

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/board"
	"gitlab.example.com/vtmocanu/uzi/api/internal/forge"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// PrdlessLabelColor is the GitLab label color EnsureLabels pins when the PRDLESS
// label is auto-created on first apply from the uzi UI (PRD #22 Decision 8:
// EnsureLabels requires a color, the way board columns pin theirs). Amber, chosen
// distinct from the DefaultColumns palette (blues/purple/grey) so the escape-hatch
// label reads as an exception, not a column.
const PrdlessLabelColor = "#ec9a29"

// DefaultColumns are the kanban columns seeded on the forge (as labels) the
// first time a repo's board is opened, in board order. Colors are required by
// GitLab's label create API. In Progress / Later come from the common-board
// reference set; Human Review (PRD #12) is where the automation parks a card
// once its MR is open, and sits directly after In Progress.
//
// The order is READING ORDER = FLOW ORDER (PRD #102 Decision 2): the implicit
// Backlog lane is intake, then Planned ("somebody picked this"), In Progress
// ("an agent has it"), Human Review ("its MR is open"), and Later for work
// deliberately deferred. That deliberately replaces the convention this comment
// used to state — "the two workflow columns lead, the backlog buckets follow" —
// which seeded Planned's predecessor (Upcoming) AFTER Human Review, so a column
// meaning "selected, not yet started" rendered past the review lane. Planned
// keeps Upcoming's color so an operator's palette does not shift under them.
//
// The Human Review retrofit is unaffected by the reorder: humanReviewPlacement
// (handler/board.go) anchors the column at In Progress's position + 1, never at
// an absolute index, so a board seeded before Human Review existed still gets it
// in the right place. Existing boards are NOT renamed or reordered (Decision 3);
// the manual procedure is in docs/configuration.md.
var DefaultColumns = []forge.Label{
	{Name: "Planned", Color: "#6699cc"},
	{Name: board.ColumnInProgress, Color: "#1f75cb"},
	{Name: board.ColumnHumanReview, Color: "#6e49cb"},
	{Name: "Later", Color: "#999999"},
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

// IssueStore is the subset of store methods forgesvc needs: the issue-cache sync
// paths plus the MR-close watcher (PRD #24). Narrowing to an interface lets both
// be unit-tested against a fake store (and a mocked Forge) without a live
// database. *store.Queries satisfies it.
type IssueStore interface {
	UpsertIssue(ctx context.Context, arg store.UpsertIssueParams) (store.Issue, error)
	DeleteIssuesNotIn(ctx context.Context, arg store.DeleteIssuesNotInParams) (int64, error)
	// Used by the MR-close watcher (mr_watch.go).
	ListMRWatchCandidates(ctx context.Context, repoID uuid.UUID) ([]store.ListMRWatchCandidatesRow, error)
	GetIssueByIID(ctx context.Context, arg store.GetIssueByIIDParams) (store.Issue, error)
	ListBoardColumns(ctx context.Context, repoID uuid.UUID) ([]store.BoardColumn, error)
	SetRunMRState(ctx context.Context, arg store.SetRunMRStateParams) (int64, error)
	// Pipeline-status cache (PRD #6): the poller-tick pipeline sync.
	ListWatchedRunRefsForRepo(ctx context.Context, arg store.ListWatchedRunRefsForRepoParams) ([]store.ListWatchedRunRefsForRepoRow, error)
	UpsertPipelineStatus(ctx context.Context, arg store.UpsertPipelineStatusParams) (store.PipelineStatus, error)
	DeletePipelineStatusesNotIn(ctx context.Context, arg store.DeletePipelineStatusesNotInParams) (int64, error)
	// CI-fix verification (PRD #6): stamp a fix run's verdict from its post-fix pipeline.
	FindCIFixStampTarget(ctx context.Context, arg store.FindCIFixStampTargetParams) (store.Run, error)
	StampFixVerdict(ctx context.Context, arg store.StampFixVerdictParams) (int64, error)
	// Filed→Done sync (PRD #98 M6): the open→closed edge over the freshly-synced issue
	// cache, the DO-NOTHING disposition insert, and the edge stamp (judge_issue_close.go).
	ListFiledIssueCloseEdges(ctx context.Context, arg store.ListFiledIssueCloseEdgesParams) ([]store.ListFiledIssueCloseEdgesRow, error)
	ApplyFiledIssueCloseEdge(ctx context.Context, arg store.ApplyFiledIssueCloseEdgeParams) (store.ApplyFiledIssueCloseEdgeRow, error)
	// PRD-link patch (PRD #72 M5): the merged-MR edge over completed issue runs that
	// declared a moved PRD path, and its settle (prd_link_patch.go).
	ListPRDLinkPatchCandidates(ctx context.Context, arg store.ListPRDLinkPatchCandidatesParams) ([]store.ListPRDLinkPatchCandidatesRow, error)
	SettlePRDLinkPatch(ctx context.Context, id uuid.UUID) (int64, error)
}

// LabelConfig resolves the configured PRD label the sync filters query by
// (PRD #19). *settings.Cache satisfies it; the sync depends on the behavior, not
// the concrete cache, so its tests can supply a fixed label. Resolution is
// best-effort: a nil resolver or an empty/errored read falls back to
// settings.DefaultPRDLabel, so a transient settings-store blip degrades a sync to
// the default label rather than filtering on an empty one.
type LabelConfig interface {
	PRDLabel(ctx context.Context) (string, error)
}

// Service bundles the dependencies for building forge clients and syncing.
type Service struct {
	q       IssueStore
	box     *secretbox.Box
	timeout time.Duration
	labels  LabelConfig
}

// New constructs a Service. box encrypts/decrypts stored PATs; timeout bounds
// every forge HTTP call; labels resolves the configured PRD label the sync
// filters on (nil is tolerated and falls back to the compiled-in default).
func New(q IssueStore, box *secretbox.Box, timeout time.Duration, labels LabelConfig) *Service {
	return &Service{q: q, box: box, timeout: timeout, labels: labels}
}

// prdLabel resolves the configured PRD label for the sync filters, falling back
// to the compiled-in default when unconfigured or on a settings read error (the
// accessor already returns the default alongside a cold error, so this is
// best-effort by design).
func (s *Service) prdLabel(ctx context.Context) string {
	if s.labels != nil {
		if l, _ := s.labels.PRDLabel(ctx); l != "" {
			return l
		}
	}
	return settings.DefaultPRDLabel
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

// SetIssueLabel adds or removes ONE named label on an issue forge-first, then
// updates the cache incrementally — the mechanic behind the PRDLESS UI toggle
// (PRD #22 Decision 10). Unlike AutoMove (which strips every other column label to
// enforce single-column membership) it touches only the one label and preserves
// everything else, so it must never be used for column moves.
//
// Idempotent by a cached-labels diff: when the desired state already holds (apply
// and the label is already present, or remove and already absent) it is a local
// no-op success with NO forge call. Otherwise, on apply it first EnsureLabels the
// label (auto-creating it on the project the first time, pinned to
// PrdlessLabelColor), then issues one UpdateIssueLabels with a single-element add
// or remove set. Only on forge success does it upsert the incrementally-updated
// label set — the one label added to / removed from the current cached set, never
// a wholesale recompute from stale data — carrying HasPrdLink through verbatim
// (this path never re-derives it). Returns the re-cached row; on a forge error the
// cache is untouched, so a failed toggle never desyncs the cache from the forge.
func (s *Service) SetIssueLabel(ctx context.Context, f forge.Forge, forgeProjectID int64, issue store.Issue, label string, apply bool) (store.Issue, error) {
	var current []string
	if err := json.Unmarshal(issue.Labels, &current); err != nil {
		current = []string{}
	}

	// Diff-first: if the cache already reflects the desired state, skip the forge
	// entirely and return the row unchanged (idempotent apply/remove).
	if slices.Contains(current, label) == apply {
		return issue, nil
	}

	var add, remove []string
	if apply {
		// Auto-create the label on the project the first time it is applied from
		// uzi (board columns do the same); GitLab label creation needs a color.
		if err := f.EnsureLabels(ctx, forgeProjectID, []forge.Label{{Name: label, Color: PrdlessLabelColor}}); err != nil {
			return store.Issue{}, err
		}
		add = []string{label}
	} else {
		remove = []string{label}
	}
	if err := f.UpdateIssueLabels(ctx, forgeProjectID, issue.ForgeIssueIid, add, remove); err != nil {
		return store.Issue{}, err
	}

	// Incremental cache update on success only: add/remove the one label on the
	// current set (order preserved, every other label kept), never an overwrite
	// computed from a possibly-stale snapshot.
	next := make([]string, 0, len(current)+1)
	if apply {
		next = append(next, current...)
		next = append(next, label)
	} else {
		for _, l := range current {
			if l != label {
				next = append(next, l)
			}
		}
	}
	labelsJSON, err := json.Marshal(next)
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
		HasPrdLink:     issue.HasPrdLink, // preserved verbatim; this path never re-derives it
		ForgeUpdatedAt: issue.ForgeUpdatedAt,
	})
}

// FullSync fetches the complete PRD-labeled set (state=all, no lower bound),
// upserts every issue into the cache, then evicts cache rows absent from the
// fresh set. This is the only path that observes de-labeling and deletion, so
// it doubles as the reconcile pass and the manual Refresh. Returns the max
// forge updated_at seen, so a caller tracking a high-water-mark can advance it.
func (s *Service) FullSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge) (time.Time, error) {
	issues, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{s.prdLabel(ctx)}})
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
	opts := forge.ListIssuesOptions{Labels: []string{s.prdLabel(ctx)}}
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
