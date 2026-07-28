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

// PrdLabelColor is the color EnsureLabels pins when Promote (PRD #102 Decision 15)
// applies the PRD label to a repo that somehow lacks it. Green, distinct from both
// the DefaultColumns palette (blues/purple/grey) and the PRDLESS amber: the PRD
// label marks work uzi owns, which is neither a column nor an exception.
//
// It exists at all because Promote reuses SetIssueLabel, whose apply path
// auto-creates the label. Passing PrdlessLabelColor there would create a missing
// PRD label in the escape hatch's amber, which is the naive-reuse bug Decision 15
// names.
const PrdLabelColor = "#2da160"

// DefaultColumns are the kanban columns seeded on the forge (as labels) the
// first time a repo's board is opened, in board order. Colors are required by
// GitLab's label create API. In Progress / Later come from the common-board
// reference set; Human Review (PRD #12) is where the automation parks a card when
// its run COMPLETES, and sits directly after In Progress.
//
// "once its MR is open" is what this sentence said until 2026-07-27, and it was
// false — pre-existing, and carried through the PRD #102 M2 rewrite of this comment
// without being checked. runlifecycle maps `completed` to ColumnHumanReview in BOTH
// notifierDecision and reconcilerDecision with no MR condition anywhere, so a run
// that completes without opening one still parks here; docs/board.md had it right all
// along ("with or without a merge request"). MR state drives only the later rework
// flip back to In Progress on an unmerged close (forgesvc/mr_watch.go).
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
// label (auto-creating it on the project the first time, pinned to color), then
// issues one UpdateIssueLabels with a single-element add or remove set.
//
// color is the label color to pin when apply auto-creates the label, and is
// ignored on remove. It is a PARAMETER rather than the PRDLESS constant it used to
// be because Promote (PRD #102 Decision 15) reuses this path: hardcoding the
// escape hatch's amber would create a missing PRD label in it. Only on forge success does it upsert the incrementally-updated
// label set — the one label added to / removed from the current cached set, never
// a wholesale recompute from stale data — carrying HasPrdLink through verbatim
// (this path never re-derives it). Returns the re-cached row; on a forge error the
// cache is untouched, so a failed toggle never desyncs the cache from the forge.
func (s *Service) SetIssueLabel(ctx context.Context, f forge.Forge, forgeProjectID int64, issue store.Issue, label, color string, apply bool) (store.Issue, error) {
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
		if err := f.EnsureLabels(ctx, forgeProjectID, []forge.Label{{Name: label, Color: color}}); err != nil {
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

// FullSync fetches the complete PRD-labeled set (state=all, no lower bound) and,
// since PRD #102 M6, every OPEN issue regardless of label — an ADDITIVE second
// fetch, not a widening of the first (Decision 9). It upserts both, then evicts
// cache rows absent from the UNION of the two. This is the only path that
// observes de-labeling and deletion, so it doubles as the reconcile pass and the
// manual Refresh. Returns the high-water-mark a caller may advance to; see
// advanceHWM for why that is not simply the later of the two fetches.
//
// BOTH fetches must succeed before anything is deleted (Decision 11). A union
// missing one half is not authoritative, and treating it as one wipes whatever
// the failed half owns — the entire non-PRD backlog, every poll, on a transient
// forge error. There is deliberately no "the extra fetch is best-effort, log and
// continue" path: a soft-fail would also advance the shared high-water-mark past
// a window nobody read (Decision 11a).
func (s *Service) FullSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge) (time.Time, error) {
	prdLabel := s.prdLabel(ctx)
	issues, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{prdLabel}})
	if err != nil {
		// Abort BEFORE any eviction: a failed/partial fetch must never be
		// treated as an authoritative empty set, or a transient forge error
		// would wipe the cache. Eviction only runs below, after BOTH fetches
		// have come back clean.
		return time.Time{}, err
	}
	openIssues, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{State: forge.StateOpened})
	if err != nil {
		return time.Time{}, err
	}
	extra := withoutLabel(openIssues, prdLabel)

	maxUpdated, err := s.upsertIssues(ctx, repoID, issues)
	if err != nil {
		return time.Time{}, err
	}
	if _, err := s.upsertIssues(ctx, repoID, extra); err != nil {
		return time.Time{}, err
	}
	// A clean PAIR of fetches that legitimately returns zero issues DOES evict
	// everything — the forge is the source of truth (empty means empty).
	keep := make([]int64, 0, len(issues)+len(extra))
	for _, is := range issues {
		keep = append(keep, is.IID)
	}
	for _, is := range extra {
		keep = append(keep, is.IID)
	}
	if _, err := s.q.DeleteIssuesNotIn(ctx, store.DeleteIssuesNotInParams{RepoID: repoID, KeepIids: keep}); err != nil {
		return time.Time{}, err
	}
	return advanceHWM(maxUpdated, maxUpdatedAt(openIssues)), nil
}

// IncrementalSync fetches only issues updated at/after hwm and upserts them —
// the PRD-labeled set (state=all) plus the additive open, no-label set, the same
// pair FullSync takes. It cannot see de-labeling or deletion (the filters
// structurally exclude them) — that is FullSync's job. Returns the advanced
// high-water-mark, never lower than the one it was given. The bound is inclusive
// at second granularity, so the boundary row is re-fetched and deduped by upsert.
//
// Either fetch failing returns the CALLER'S hwm unchanged, along with the error
// (Decision 11a). Returning nil with an advanced mark after a half-failure is
// what would make the next pass skip the failed path's window until the next
// full reconcile, roughly ten minutes at shipped defaults.
func (s *Service) IncrementalSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge, hwm time.Time) (time.Time, error) {
	prdLabel := s.prdLabel(ctx)
	opts := forge.ListIssuesOptions{Labels: []string{prdLabel}}
	openOpts := forge.ListIssuesOptions{State: forge.StateOpened}
	if !hwm.IsZero() {
		opts.UpdatedAfter = &hwm
		openOpts.UpdatedAfter = &hwm
	}
	issues, err := f.ListIssues(ctx, forgeProjectID, opts)
	if err != nil {
		return hwm, err
	}
	openIssues, err := f.ListIssues(ctx, forgeProjectID, openOpts)
	if err != nil {
		return hwm, err
	}
	maxUpdated, err := s.upsertIssues(ctx, repoID, issues)
	if err != nil {
		return hwm, err
	}
	if _, err := s.upsertIssues(ctx, repoID, withoutLabel(openIssues, prdLabel)); err != nil {
		return hwm, err
	}
	if advanced := advanceHWM(maxUpdated, maxUpdatedAt(openIssues)); advanced.After(hwm) {
		return advanced, nil
	}
	return hwm, nil
}

// withoutLabel drops the issues carrying label from a fetch result. It is what
// makes the second fetch ADDITIVE rather than overlapping (Decision 9): an
// unfiltered open fetch necessarily re-returns the open issues the PRD fetch
// already owns, and those belong to the PRD path, whose semantics (state=all,
// eviction on de-labeling) are defined against ITS snapshot.
//
// Matching is exact, like every other label comparison in this package — board
// column labels, the PRDLESS toggle — and, more to the point, like the filter the
// forge itself applied to the first fetch. A looser match here would classify an
// issue differently from the way the forge classified it, and those two are the
// one pair that must not disagree: an issue in both halves is written twice, an
// issue in neither is evicted.
func withoutLabel(issues []forge.Issue, label string) []forge.Issue {
	out := make([]forge.Issue, 0, len(issues))
	for _, is := range issues {
		if slices.Contains(is.Labels, label) {
			continue
		}
		out = append(out, is)
	}
	return out
}

// maxUpdatedAt returns the largest forge updated_at in a fetch result, or the
// zero time for an empty one. Unlike upsertIssues' return it is computed over the
// RAW result, before withoutLabel drops the rows the PRD path owns: the value is
// used only as a lower bound on WHEN the forge was read, and a row that was read
// and then discarded witnesses that reading just as well as one that was kept.
func maxUpdatedAt(issues []forge.Issue) time.Time {
	var max time.Time
	for _, is := range issues {
		if is.UpdatedAt.After(max) {
			max = is.UpdatedAt
		}
	}
	return max
}

// advanceHWM combines the two fetches' max forge updated_at into the single
// high-water-mark the poller carries per repo (Decision 11a). It takes the
// MINIMUM of the two, never the maximum, and the reason is the gap between them.
//
// The fetches are separate round trips: the PRD fetch observes the forge at some
// instant tA, the open fetch at a later tB. Everything the PAIR is known to have
// seen is bounded by tA, the EARLIER of the two. An issue updated in (tA, tB)
// that carries the PRD label appears in neither result — too late for the first
// fetch, dropped by withoutLabel from the second — so a mark taken from the later
// fetch steps straight over an update nobody observed, and the next
// IncrementalSync asks only for what came after it. The row then sits stale until
// the next full reconcile.
//
// prdMax and openMax are lower bounds on tA and tB respectively, since no issue
// can report an updated_at later than the moment it was read. The smaller of the
// two is therefore a lower bound on tA and safe to keep.
//
// The zero cases are NOT symmetric, and the asymmetry is the fetch order:
//   - openMax zero constrains nothing. It says the window held no open issues,
//     not that the window went unread, and the open fetch ran AFTER the PRD one,
//     so prdMax <= tA < tB stands on its own.
//   - prdMax zero is the pre-M6 "nothing to advance to" case and is preserved:
//     the caller keeps the mark it already had. Advancing to openMax instead
//     would be unsound for exactly the reason above, openMax being free to exceed
//     tA.
//
// THE SECOND CASE HAS A STANDING COST, and it is worth knowing rather than fixing.
// A repo with open issues but NO PRD issues has prdMax zero on every pass, so its
// mark never advances at all and every IncrementalSync re-reads from the same
// bound — now paying TWO unbounded fetches per poll rather than one. Correctness is
// unaffected (the upserts are idempotent and the window only ever re-covers ground
// already covered), and the pre-M6 code had the identical no-advance behaviour on
// the PRD fetch; M6 doubles the traffic it costs.
//
// Not fixed here because every fix is worse: advancing on openMax alone is the
// unsound case above, and a per-path mark is a wider change to the poller's state
// than this milestone should carry. It is the same unboundedness the PRD's "M6
// scale" open question names, and it wants the same answer — a cap or a per-repo
// opt-in — not a change to this function.
func advanceHWM(prdMax, openMax time.Time) time.Time {
	if prdMax.IsZero() || openMax.IsZero() {
		return prdMax
	}
	if openMax.Before(prdMax) {
		return openMax
	}
	return prdMax
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
