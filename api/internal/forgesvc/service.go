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

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PromoteLabelColor is the color EnsureLabels pins when Promote (PRD #102 Decision
// 15) applies the uzi run-eligibility label (PRD #764) to a repo that somehow lacks
// it. Green, distinct from the DefaultColumns palette (blues/purple/grey): the uzi
// label marks work uzi owns, which is not a column.
//
// It exists at all because Promote reuses SetIssueLabel, whose apply path
// auto-creates the label; without a pinned color EnsureLabels would fail (GitLab's
// label-create API requires one).
const PromoteLabelColor = "#2da160"

// DefaultColumns are the kanban columns seeded on the forge (as labels) the
// first time a repo's board is opened, in board order. Colors are required by
// GitLab's label create API. In Progress / Later come from a common board
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

// DefaultColumnColor is the grey a board column label falls back to when its
// name is not one of the DefaultColumns (a user-added custom column). It is the
// same color ConfigureColumns creates custom columns with, so ensuring a column
// label at drag-time never changes a column's color out from under an operator.
const DefaultColumnColor = "#8c8c8c"

// ColumnColor resolves the forge label color for a board column by name: the
// pinned DefaultColumns color for a known default column, else DefaultColumnColor.
// This keeps the column palette a single source of truth (the constants), so
// AutoMove can recreate a drifted/missing column label with its correct color.
func ColumnColor(name string) string {
	for _, c := range DefaultColumns {
		if c.Name == name {
			return c.Color
		}
	}
	return DefaultColumnColor
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
	// CI-autofix loop guard (PRD #71 M4): reset the attempt ledger on a green
	// pipeline, and evict ledger rows for refs that left the watch set on reconcile.
	DeleteCIAutofixAttempt(ctx context.Context, arg store.DeleteCIAutofixAttemptParams) (int64, error)
	DeleteCIAutofixAttemptsNotIn(ctx context.Context, arg store.DeleteCIAutofixAttemptsNotInParams) (int64, error)
	// Filed→Done sync (PRD #98 M6): the open→closed edge over the freshly-synced issue
	// cache, the DO-NOTHING disposition insert, and the edge stamp (judge_issue_close.go).
	ListFiledIssueCloseEdges(ctx context.Context, arg store.ListFiledIssueCloseEdgesParams) ([]store.ListFiledIssueCloseEdgesRow, error)
	ApplyFiledIssueCloseEdge(ctx context.Context, arg store.ApplyFiledIssueCloseEdgeParams) (store.ApplyFiledIssueCloseEdgeRow, error)
	// PRD-link patch (PRD #72 M5): the merged-MR edge over completed issue runs that
	// declared a moved PRD path, and its settle (prd_link_patch.go).
	ListPRDLinkPatchCandidates(ctx context.Context, arg store.ListPRDLinkPatchCandidatesParams) ([]store.ListPRDLinkPatchCandidatesRow, error)
	SettlePRDLinkPatch(ctx context.Context, id uuid.UUID) (int64, error)
}

// LabelConfig resolves the configured uzi run-eligibility label the sync filters
// query by (PRD #764 D7: the any-state sync fetch keys on uzi_label now that the
// PRD label has lost its special meaning). *settings.Cache satisfies it; the sync
// depends on the behavior, not the concrete cache, so its tests can supply a fixed
// label. Resolution is best-effort: a nil resolver or an empty/errored read falls
// back to settings.DefaultUziLabel, so a transient settings-store blip degrades a
// sync to the default label rather than filtering on an empty one.
type LabelConfig interface {
	UziLabel(ctx context.Context) (string, error)
}

// Service bundles the dependencies for building forge clients and syncing.
type Service struct {
	q       IssueStore
	box     *secretbox.Box
	timeout time.Duration
	labels  LabelConfig

	// notifier lands the "your auto-fix landed" inbox row on the reset-on-green path
	// (PRD #71 M6). Optional (nil-safe): set via SetNotifier, unset means the landed
	// notification is skipped — every other pipeline-sync behaviour is unaffected.
	notifier LandedNotifier
}

// LandedNotifier lands an inbox notification when a ci_fix run's fix pipeline goes
// green (PRD #71 M6). *notifysvc.Service satisfies it. Kept as an interface so the
// sync's core has no hard dependency on notifysvc and the pipeline-sync tests need
// no notifier.
type LandedNotifier interface {
	Notify(ctx context.Context, n notifysvc.Notification) (store.Notification, error)
}

// New constructs a Service. box encrypts/decrypts stored PATs; timeout bounds
// every forge HTTP call; labels resolves the configured uzi label the sync
// filters on (nil is tolerated and falls back to the compiled-in default).
func New(q IssueStore, box *secretbox.Box, timeout time.Duration, labels LabelConfig) *Service {
	return &Service{q: q, box: box, timeout: timeout, labels: labels}
}

// SetNotifier wires the landed-notification collaborator (PRD #71 M6). Call once at
// startup, before the poller runs. A nil notifier (the default) disables the landed
// inbox row; the rest of the sync is unchanged.
func (s *Service) SetNotifier(n LandedNotifier) { s.notifier = n }

// uziLabel resolves the configured uzi run-eligibility label for the sync filters,
// falling back to the compiled-in default when unconfigured or on a settings read
// error (the accessor already returns the default alongside a cold error, so this
// is best-effort by design).
func (s *Service) uziLabel(ctx context.Context) string {
	if s.labels != nil {
		if l, _ := s.labels.UziLabel(ctx); l != "" {
			return l
		}
	}
	return settings.DefaultUziLabel
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

	// Ensure the label we are about to add exists on the forge before writing it
	// (mirrors SetIssueLabel). AutoMove is the manual-drag / run-automation path and
	// must not assume the forge already carries the target column's label — it can
	// be missing or drifted (e.g. the Upcoming→Planned rename, #176). add is the
	// target column and has at most one element; an empty add (already in the target
	// column, or a move to Open) ensures nothing and keeps the cheap no-forge-call path.
	if len(add) > 0 {
		labels := make([]forge.Label, 0, len(add))
		for _, name := range add {
			labels = append(labels, forge.Label{Name: name, Color: ColumnColor(name)})
		}
		if err := f.EnsureLabels(ctx, forgeProjectID, labels); err != nil {
			return store.Issue{}, err
		}
	}

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
		AssigneeIds:    issue.AssigneeIds, // preserved verbatim; this path never re-derives it
		WebUrl:         issue.WebUrl,
		Author:         issue.Author,
		HasPrdLink:     issue.HasPrdLink,
		ForgeUpdatedAt: issue.ForgeUpdatedAt,
	})
}

// SetIssueLabel adds or removes ONE named label on an issue forge-first, then
// updates the cache incrementally — the mechanic behind the Promote button (PRD
// #102 Decision 15, PRD #764). Unlike AutoMove (which strips every other column
// label to enforce single-column membership) it touches only the one label and
// preserves everything else, so it must never be used for column moves.
//
// Idempotent by a cached-labels diff: when the desired state already holds (apply
// and the label is already present, or remove and already absent) it is a local
// no-op success with NO forge call. Otherwise, on apply it first EnsureLabels the
// label (auto-creating it on the project the first time, pinned to color), then
// issues one UpdateIssueLabels with a single-element add or remove set.
//
// color is the label color to pin when apply auto-creates the label, and is
// ignored on remove. It is a PARAMETER so the one caller (Promote) supplies the
// color for the label it applies. Only on forge success does it upsert the incrementally-updated
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
		AssigneeIds:    issue.AssigneeIds, // preserved verbatim; this path never re-derives it
		WebUrl:         issue.WebUrl,
		Author:         issue.Author,
		HasPrdLink:     issue.HasPrdLink, // preserved verbatim; this path never re-derives it
		ForgeUpdatedAt: issue.ForgeUpdatedAt,
	})
}

// Marks is the pair of high-water marks the two sync fetches carry — ONE PER
// FETCH (issue #177), not one shared between them. PRD bounds the PRD-labelled
// fetch (state=all); Open bounds the additive open, no-label fetch. Each mark
// advances only on its OWN fetch's evidence: to the max forge updated_at over
// that fetch's RAW result, and never backwards. An empty result carries no
// evidence and leaves its mark exactly where it was.
//
// The invariant is "never past the INSTANT ITS FETCH OBSERVED", which is not the
// same as "never past that fetch's max". A mark routinely sits ABOVE the max of
// the batch in front of it — Advance keeps 9000 over a fetch whose newest row is
// 100, which is what monotonicity means and what
// TestIncrementalSyncKeepsHWMWhenBatchOlder asserts. What must never happen is a
// mark exceeding the read instant, because that is what skips a window.
//
// It replaces a single shared mark, and the reason that one could not work is
// worth keeping, because folding these two fields back into one reads like a
// simplification. The fetches are separate round trips: the PRD fetch observes
// the forge at some instant tA, the open fetch at a later tB. A SHARED mark
// therefore had to take the MINIMUM of the two maxima, never the maximum — a
// PRD-labelled issue updated in (tA, tB) appears in NEITHER result (too late for
// the first fetch, dropped by withoutLabel from the second), so a shared mark
// taken from the later fetch steps straight over an update nobody observed, and
// the next IncrementalSync asks only for what came after it. The row then sits
// stale until the next full reconcile (Decision 11a).
//
// Separate marks DISSOLVE that race rather than working around it. prdMax <= tA
// and openMax <= tB, since no issue can report an updated_at later than the
// moment it was read. So the PRD mark stays bounded by tA and the next PRD fetch,
// asking for updated_after = prdMax, still returns the issue updated in (tA, tB);
// and the Open mark advancing to openMax skips nothing IT owns, because any open
// issue updated before tB was in that fetch. min() existed ONLY because the open
// fetch's later evidence could push the SHARED mark past tA. With a mark per
// fetch it has nothing left to protect.
//
// What the shared mark cost — this is a fix, not a refactor (issue #177). The old
// combine returned prdMax whenever EITHER input was zero, so a repo with open
// issues but no PRD-labelled ones had prdMax zero on every pass: its mark never
// advanced at all and every IncrementalSync re-read the whole open set from the
// beginning, forever. PRD #102 M6 is precisely what makes a board useful for such
// a repo, and it is also what doubled the traffic that stall costs.
//
// Label transitions still work, per mark. A transition bumps the issue's
// updated_at to t1, and each mark is bounded by the instant its own last fetch
// observed, so a mark below t1 means that fetch has not yet covered the change
// and will return it.
//
//   - GAINS the PRD label at t1: PRD < t1 (the mark predates the change), so the
//     PRD fetch returns it.
//   - LOSES the label at t1: it cannot appear in the PRD fetch at all, but
//     withoutLabel now KEEPS it, so the open fetch is what carries it — and if
//     Open < t1 the very next open fetch does. Open >= t1 is possible when the
//     loss predates the last open fetch, but then that fetch already returned the
//     issue and it is already synced, so there is nothing left owing either way.
//   - Loses the label AND closes: it appears in neither fetch; FullSync's
//     eviction handles it, unchanged.
//
// NARROWING, seen and priced (auditor advisory, Low). A forge-supplied
// FUTURE-dated updated_at now pins its own mark forward. The old shared min()
// incidentally clamped such a value on one path against the other path's max;
// per-fetch marks have nothing to clamp against, so one bad timestamp bounds one
// fetch until it is repaired. This is not a regression in kind — the soundness
// argument above always assumed updated_at <= the read instant, and a forge
// violating that broke the old mark too, just less visibly. It is bounded:
// FullSync takes no marks, so the next reconcile re-seeds both from its own
// result, within FORGE_RECONCILE_EVERY × FORGE_POLL_INTERVAL (roughly ten
// minutes at shipped defaults). Eviction is unaffected, since the keep-set is
// built from what the fetches returned and never from a mark.
type Marks struct {
	PRD  time.Time // bounds the PRD-labelled fetch (state=all)
	Open time.Time // bounds the additive open, no-label fetch
}

// Advance folds an observed pair into m FIELD-WISE, keeping the later of each.
// Neither mark regresses, and neither is constrained by the other's value — the
// zero time an empty fetch reports simply loses the comparison. Combining ACROSS
// the fields is exactly what this type exists to stop; see Marks for why a shared
// mark had to, and why a per-fetch one must not.
func (m Marks) Advance(next Marks) Marks {
	if next.PRD.After(m.PRD) {
		m.PRD = next.PRD
	}
	if next.Open.After(m.Open) {
		m.Open = next.Open
	}
	return m
}

// FullSync fetches the complete uzi-labeled set (state=all, no lower bound) and,
// since PRD #102 M6, every OPEN issue regardless of label — an ADDITIVE second
// fetch, not a widening of the first (Decision 9). It upserts both, then evicts
// cache rows absent from the UNION of the two. This is the only path that
// observes de-labeling and deletion, so it doubles as the reconcile pass and the
// manual Refresh. (PRD #764 D7: the any-state fetch keys on the uzi label now that
// the PRD label has lost its special meaning.)
//
// It takes NO marks (it is unbounded by design) and returns the pair it OBSERVED,
// one mark per fetch, for the caller to fold into its own with Marks.Advance. On
// any error it returns the ZERO pair, which folds in as a no-op, so a failed
// reconcile leaves the caller's marks exactly as they were.
//
// BOTH fetches must succeed before anything is deleted (Decision 11). A union
// missing one half is not authoritative, and treating it as one wipes whatever
// the failed half owns — the entire non-uzi backlog, every poll, on a transient
// forge error. There is deliberately no "the extra fetch is best-effort, log and
// continue" path: a soft-fail would also report a mark for a window nobody read
// (Decision 11a).
func (s *Service) FullSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge) (Marks, error) {
	uziLabel := s.uziLabel(ctx)
	issues, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{uziLabel}})
	if err != nil {
		// Abort BEFORE any eviction: a failed/partial fetch must never be
		// treated as an authoritative empty set, or a transient forge error
		// would wipe the cache. Eviction only runs below, after BOTH fetches
		// have come back clean.
		return Marks{}, err
	}
	openIssues, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{State: forge.StateOpened})
	if err != nil {
		return Marks{}, err
	}
	extra := withoutLabel(openIssues, uziLabel)

	if err := s.upsertIssues(ctx, repoID, issues); err != nil {
		return Marks{}, err
	}
	if err := s.upsertIssues(ctx, repoID, extra); err != nil {
		return Marks{}, err
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
		return Marks{}, err
	}
	return Marks{PRD: maxUpdatedAt(issues), Open: maxUpdatedAt(openIssues)}, nil
}

// IncrementalSync fetches only the issues updated at/after each fetch's OWN mark
// and upserts them — the uzi-labeled set (state=all) plus the additive open,
// no-label set, the same pair FullSync takes. It cannot see de-labeling or
// deletion (the filters structurally exclude them) — that is FullSync's job.
// Returns m advanced field-wise, never lower than what it was given. The bound is
// inclusive at second granularity, so the boundary row is re-fetched and deduped
// by upsert.
//
// The two lower bounds are guarded INDEPENDENTLY: a zero mark sends no
// updated_after on its own fetch and leaves the other's bound alone. A single
// shared guard would drop both bounds the moment either mark was zero, which is
// the unbounded re-read issue #177 is about.
//
// Either fetch failing returns the CALLER'S pair unchanged, along with the error
// (Decision 11a) — BOTH marks held, not just the failed one's. Both fetches
// complete before any upsert runs, so a half-failure means neither path's rows
// were written; advancing the successful path's mark would skip a window whose
// rows never reached the cache.
func (s *Service) IncrementalSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge, m Marks) (Marks, error) {
	uziLabel := s.uziLabel(ctx)
	opts := forge.ListIssuesOptions{Labels: []string{uziLabel}}
	if !m.PRD.IsZero() {
		opts.UpdatedAfter = &m.PRD
	}
	openOpts := forge.ListIssuesOptions{State: forge.StateOpened}
	if !m.Open.IsZero() {
		openOpts.UpdatedAfter = &m.Open
	}
	issues, err := f.ListIssues(ctx, forgeProjectID, opts)
	if err != nil {
		return m, err
	}
	openIssues, err := f.ListIssues(ctx, forgeProjectID, openOpts)
	if err != nil {
		return m, err
	}
	if err := s.upsertIssues(ctx, repoID, issues); err != nil {
		return m, err
	}
	if err := s.upsertIssues(ctx, repoID, withoutLabel(openIssues, uziLabel)); err != nil {
		return m, err
	}
	return m.Advance(Marks{PRD: maxUpdatedAt(issues), Open: maxUpdatedAt(openIssues)}), nil
}

// withoutLabel drops the issues carrying label from a fetch result. It is what
// makes the second fetch ADDITIVE rather than overlapping (Decision 9): an
// unfiltered open fetch necessarily re-returns the open issues the PRD fetch
// already owns, and those belong to the PRD path, whose semantics (state=all,
// eviction on de-labeling) are defined against ITS snapshot.
//
// Matching is exact, like every other label comparison in this package — board
// column labels, the uzi label — and, more to the point, like the filter the
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
// zero time for an empty one. It is computed over the RAW result, before
// withoutLabel drops the rows the PRD path owns: the value is used only as a
// lower bound on WHEN the forge was read, and a row that was read and then
// discarded witnesses that reading just as well as one that was kept.
//
// BOTH marks derive from it, and the symmetry is load-bearing rather than tidy.
// upsertIssues substitutes time.Now() for a zero UpdatedAt because
// issues.forge_updated_at is NOT NULL, so a mark read off what was WRITTEN would
// jump to WALL-CLOCK NOW for any issue the forge reported without an updated_at —
// later than the instant the fetch observed, which is the one thing a mark must
// never be. (The single shared mark used to mask that: min() with the other
// fetch's maximum usually clamped it back. Per-fetch marks have nothing to clamp
// against, so the exposure would be complete.) Reading the raw result errs the
// safe way instead: a mark that stays too small re-reads, it never skips. Do not
// re-derive either mark from upsertIssues.
func maxUpdatedAt(issues []forge.Issue) time.Time {
	var max time.Time
	for _, is := range issues {
		if is.UpdatedAt.After(max) {
			max = is.UpdatedAt
		}
	}
	return max
}

// upsertIssues writes each forge issue into the cache, computing has_prd_link at
// write time. It reports only an error: no high-water mark is read off this path,
// deliberately, because the time.Now() substitution below would put one past the
// instant its fetch observed (see maxUpdatedAt).
func (s *Service) upsertIssues(ctx context.Context, repoID uuid.UUID, issues []forge.Issue) error {
	for _, is := range issues {
		labelsJSON, err := json.Marshal(is.Labels)
		if err != nil {
			return err
		}
		assigneeIDsJSON, err := json.Marshal(is.Assignees)
		if err != nil {
			return err
		}
		author := pgtype.Text{}
		if is.Author != "" {
			author = pgtype.Text{String: is.Author, Valid: true}
		}
		// forge_updated_at is NOT NULL (00002_forge.sql), so an issue the forge
		// reported without an updated_at still needs a value in the COLUMN. This
		// substitution is for the column and nothing else.
		updated := is.UpdatedAt
		if updated.IsZero() {
			updated = time.Now()
		}
		if _, err := s.q.UpsertIssue(ctx, store.UpsertIssueParams{
			RepoID:         repoID,
			ForgeIssueIid:  is.IID,
			Title:          is.Title,
			State:          is.State,
			Labels:         labelsJSON,
			AssigneeIds:    assigneeIDsJSON,
			WebUrl:         is.WebURL,
			Author:         author,
			HasPrdLink:     HasPRDLink(is.Description),
			ForgeUpdatedAt: pgtype.Timestamptz{Time: updated, Valid: true},
		}); err != nil {
			return err
		}
	}
	return nil
}
