// Package forgesvc is the shared forge-sync service used by both the HTTP
// handlers and the background poller. It owns three things the two callers must
// agree on: building a forge driver from a stored (encrypted) connection, the
// PRD-link sanity check, and the full/incremental sync-into-cache logic.
package forgesvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
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
	RemoveCachedIssueLabel(context.Context, store.RemoveCachedIssueLabelParams) (store.Issue, error)
	UpsertIssue(ctx context.Context, arg store.UpsertIssueParams) (store.Issue, error)
	// UpsertIssueLabels is the label-only cache-write variant used by AutoMove and
	// SetIssueLabel: it omits assignee_ids so a racing label mutation cannot clobber
	// the forge-synced assignees with a stale in-memory snapshot (PRD #767).
	// Labels seeds INSERT only; AddLabels/RemoveLabels apply to current labels on
	// conflict, preserving survivors and appending distinct additions in order.
	UpsertIssueLabels(ctx context.Context, arg store.UpsertIssueLabelsParams) (store.Issue, error)
	// UpdateIssueState / ReopenIssueState are the narrow state-only cache flips behind
	// CloseIssue / ReopenIssue (PRD #1034 M2). UpdateIssueState touches only `state`
	// (a bare close); ReopenIssueState flips state back to 'opened' AND nulls
	// board_position so a reopened card lands at the bottom of its lane.
	UpdateIssueState(ctx context.Context, arg store.UpdateIssueStateParams) (store.Issue, error)
	ReopenIssueState(ctx context.Context, arg store.ReopenIssueStateParams) (store.Issue, error)
	DeleteIssuesNotIn(ctx context.Context, arg store.DeleteIssuesNotInParams) (int64, error)
	// Used by the MR-close watcher (mr_watch.go).
	ListMRWatchCandidates(ctx context.Context, repoID uuid.UUID) ([]store.ListMRWatchCandidatesRow, error)
	// Used by the board-free MR-state recorder (board_free_mr_watch.go, PRD #908).
	ListBoardFreeMRStateWatchCandidates(ctx context.Context, repoID uuid.UUID) ([]store.ListBoardFreeMRStateWatchCandidatesRow, error)
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
	// Findings Filed→Done sync (PRD #1183 Child B, M3): the finding twin of the judge close
	// sync — the open→closed edge over the freshly-synced issue cache and its guarded apply
	// (finding_issue_close.go).
	ListFindingIssueCloseEdges(ctx context.Context, repoID uuid.UUID) ([]store.ListFindingIssueCloseEdgesRow, error)
	ApplyFindingIssueCloseEdge(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
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
	// FindingLabel resolves the incidental-finding marker label (PRD #333 D5) the
	// sync's THIRD fetch keys on (PRD #1183 Child B). A filed finding issue carries
	// ONLY this marker, never the uzi label (settings.ValidateMerged forbids the two
	// being equal), so without a fetch keyed on it a CLOSED finding issue is observed
	// by neither of the first two fetches and SyncFindingIssueCloses never sees the
	// open→closed edge. Best-effort like UziLabel: a nil resolver or an empty/errored
	// read falls back to settings.DefaultFindingLabel. *settings.Cache satisfies it.
	FindingLabel(ctx context.Context) (string, error)
}

// ForgeBuilder constructs a forge client from its type, base URL, token, and timeout.
type ForgeBuilder func(forge.Type, string, string, time.Duration) (forge.Forge, error)

// Service bundles the dependencies for building forge clients and syncing.
type Service struct {
	q             IssueStore
	box           *secretbox.Box
	timeout       time.Duration
	labels        LabelConfig
	groupDB       findingGroupDB
	groupCursorMu sync.Mutex
	groupCursors  map[uuid.UUID]store.FindingGroupCursor
	forgeBuilder  ForgeBuilder

	// findingGroupSince is the optional pending-warning age clock; nil uses time.Since.
	findingGroupSince func(time.Time) time.Duration

	// reworkCanceller aborts an in-flight mr_rework run when its MR leaves the opened
	// state (issue #853). Optional (nil-safe): set via SetReworkCanceller, unset means
	// the mid-flight abort is skipped — every other MR-sync behaviour is unaffected.
	reworkCanceller ReworkCanceller

	// cappedRepos remembers which repos' pipeline watch dropped branches at the ref
	// cap on their last sync, so SyncPipelines logs the transition into and out of
	// the capped state rather than once per tick (issue #1483). Lazily initialised;
	// guarded by cappedMu because the handlers and the poller share one Service.
	cappedMu    sync.Mutex
	cappedRepos map[uuid.UUID]bool
}

// ReworkCanceller aborts an active mr_rework run when its MR leaves the opened
// state (issue #853). *workersvc.Service satisfies it. Optional (nil-safe): unset
// means the mid-flight abort is skipped and every other sync behaviour is unchanged.
type ReworkCanceller interface {
	CancelReworkForMR(ctx context.Context, repoID uuid.UUID, mrIID int64, reason string) error
}

// New constructs a Service. box encrypts/decrypts stored PATs; timeout bounds
// every forge HTTP call; labels resolves the configured uzi label the sync
// filters on (nil is tolerated and falls back to the compiled-in default).
func New(q IssueStore, box *secretbox.Box, timeout time.Duration, labels LabelConfig) *Service {
	return &Service{q: q, box: box, timeout: timeout, labels: labels, forgeBuilder: forge.New}
}

// NewWithForgeBuilder constructs a Service with a supplied forge client builder.
// A nil builder uses the same default as New.
func NewWithForgeBuilder(q IssueStore, box *secretbox.Box, timeout time.Duration, labels LabelConfig, builder ForgeBuilder) *Service {
	s := New(q, box, timeout, labels)
	if builder != nil {
		s.forgeBuilder = builder
	}
	return s
}

// findingGroupDB supports repo-scoped reads and atomic settlement.
type findingGroupDB interface {
	store.DBTX
	store.FindingGroupDB
}

// SetFindingGroupDB enables group filing reconciliation. Call at startup.
func (s *Service) SetFindingGroupDB(db findingGroupDB) { s.groupDB = db }

// pendingFindingGroups settles durable records before any forge observation. The
// returned closure emits one warning after the pass if claims remain.
func (s *Service) pendingFindingGroups(ctx context.Context, repoID uuid.UUID) ([]store.FindingGroupClaimOperation, func(), func(), error) {
	noop := func() {}
	if s.groupDB == nil {
		return nil, noop, noop, nil
	}
	var reconcileErr error
	finish := func() {
		// The sync context may have expired during forge work. Keep diagnostics
		// bounded, but give the aggregate its own chance to report count and age.
		statsCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		count, oldest, err := store.FindingGroupRepoPendingStats(statsCtx, s.groupDB, repoID)
		if err != nil {
			reconcileErr = errors.Join(reconcileErr, err)
		}
		if count == 0 && reconcileErr == nil {
			return
		}
		attrs := []any{"repo_id", repoID, "pending_group_operations", count}
		if oldest != nil {
			since := s.findingGroupSince
			if since == nil {
				since = time.Since
			}
			age := since(*oldest)
			if age < 0 {
				age = 0
			}
			attrs = append(attrs, "oldest_age", age)
		}
		if reconcileErr != nil {
			attrs = append(attrs, "error", reconcileErr)
		}
		slog.Warn("finding group reconciliation pending", attrs...)
	}
	// Select a bounded page. The cursor advances only after recorded settlements succeed.
	s.groupCursorMu.Lock()
	if s.groupCursors == nil {
		s.groupCursors = make(map[uuid.UUID]store.FindingGroupCursor)
	}
	var after *store.FindingGroupCursor
	if cursor, ok := s.groupCursors[repoID]; ok {
		after = &cursor
	}
	ops, err := store.ListPendingFindingGroupsForRepo(ctx, s.groupDB, repoID, after)
	if err == nil && len(ops) == 0 && after != nil {
		ops, err = store.ListPendingFindingGroupsForRepo(ctx, s.groupDB, repoID, nil)
	}
	s.groupCursorMu.Unlock()
	if err != nil {
		reconcileErr = err
		return nil, finish, noop, err
	}
	// Settle durable issue identities before spending work on uncertain claims.
	for _, op := range ops {
		if op.IssueIID == nil || op.IssueURL == "" {
			continue
		}
		_, err := store.SettleFindingGroup(ctx, s.groupDB, op.UserID, op.ID)
		if err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("settle finding group operation %s: %w", op.ID, err))
		}
		// A failed or raced settlement stays claimed for a later pass.
	}
	if reconcileErr != nil {
		return ops, finish, noop, nil
	}
	advance := func() {
		if len(ops) == 0 {
			return
		}
		last := ops[len(ops)-1]
		s.groupCursorMu.Lock()
		defer s.groupCursorMu.Unlock()
		// A concurrent pass may already have advanced farther. Do not rewind it.
		current, exists := s.groupCursors[repoID]
		if (after == nil && !exists) || (after != nil && exists && current == *after) {
			s.groupCursors[repoID] = store.FindingGroupCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		}
	}
	return ops, finish, advance, nil
}

// Marker reconciliation scans the COMPLETE unfiltered issue list (see
// reconcileFindingGroupMarkers), but it indexes only markers naming a matchable
// operation on the current pending page. A marker for any other id (an operation
// settled long ago, one on another page, or text planted in an unrelated issue)
// cannot hide a second carrier of a wanted marker, so ignoring it is safe; and
// counting it would let settled group issues, or anyone who can open an issue,
// grow the scan without bound. The wanted set is at most one pending page, and a
// wanted id saturates at ambiguous on its second carrier, so the scan needs no
// candidate or description cap.
const (
	findingGroupMarkerPrefix = "<!-- uzi-finding-group-operation: "
	findingGroupMarkerSuffix = " -->"
)

// errFindingGroupScanIncomplete marks a marker scan that could not see the
// complete issue set (the unfiltered fetch failed, including on the driver's own
// pagination cap). FullSync keeps every unconfirmed claim and carries on with the
// issue sync; any other reconciliation error, a database write in particular,
// still fails the pass.
var errFindingGroupScanIncomplete = errors.New("finding group marker scan incomplete")

type findingGroupMatch struct {
	issue     forge.Issue
	ambiguous bool
}

// indexFindingGroupIssues maps each wanted operation id to the issue carrying its
// marker. A repeated exact marker, even in one description, is ambiguous.
func indexFindingGroupIssues(issues []forge.Issue, want map[uuid.UUID]struct{}) map[uuid.UUID]findingGroupMatch {
	index := make(map[uuid.UUID]findingGroupMatch)
	for _, issue := range issues {
		description := issue.Description
		for {
			at := strings.Index(description, findingGroupMarkerPrefix)
			if at < 0 {
				break
			}
			description = description[at+len(findingGroupMarkerPrefix):]
			if len(description) < 36+len(findingGroupMarkerSuffix) || description[36:36+len(findingGroupMarkerSuffix)] != findingGroupMarkerSuffix {
				continue
			}
			id, err := uuid.Parse(description[:36])
			if err != nil || id.String() != description[:36] {
				continue
			}
			if _, wanted := want[id]; !wanted {
				continue
			}
			if previous, exists := index[id]; exists {
				previous.ambiguous = true
				index[id] = previous
			} else {
				index[id] = findingGroupMatch{issue: issue}
			}
		}
	}
	return index
}

func (s *Service) recordFindingGroupMatches(ctx context.Context, repoID uuid.UUID, issues []forge.Issue, want map[uuid.UUID]struct{}) error {
	if s.groupDB == nil {
		return nil
	}
	index := indexFindingGroupIssues(issues, want)
	if len(index) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	ops, err := store.ListPendingFindingGroupsByIDs(ctx, s.groupDB, repoID, ids)
	if err != nil {
		return err
	}
	for _, op := range ops {
		match := index[op.ID]
		issue := match.issue
		if match.ambiguous || issue.IID <= 0 || issue.WebURL == "" {
			continue
		}
		recorded, err := store.RecordFindingGroupIssue(ctx, s.groupDB, op.UserID, op.ID, issue.IID, issue.WebURL)
		if err != nil {
			return err
		}
		if !recorded {
			continue
		}
		if _, err := store.SettleFindingGroup(ctx, s.groupDB, op.UserID, op.ID); err != nil {
			return err
		}
	}
	return nil
}

// reconcileFindingGroupMarkers settles unconfirmed group operations whose issue
// was created but whose iid was never recorded, by matching the durable marker
// in issue descriptions. Only FullSync calls it, and only with the COMPLETE
// issue list: one unfiltered, all-states ListIssues. An UpdatedAfter-filtered
// set can return only one of two issues carrying the same marker, and a
// label-filtered set omits an issue that was de-labeled or never labeled; either
// hides a duplicate and turns an ambiguous marker into a unique one. Ambiguity is
// therefore evaluated over the complete set inside recordFindingGroupMatches.
//
// It matches only the page's matchable operations (phase in_flight or
// returned_uncertain with no recorded iid) and makes NO forge request when there
// are none: a recorded op (settled by pendingFindingGroups) or a pre_call op
// never triggers the fetch. Operations on other pages are matched as FullSync
// rotates to them. A failed fetch returns errFindingGroupScanIncomplete with
// nothing recorded, so every claim stays.
func (s *Service) reconcileFindingGroupMarkers(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge, pending []store.FindingGroupClaimOperation) error {
	if s.groupDB == nil {
		return nil
	}
	want := make(map[uuid.UUID]struct{})
	for _, op := range pending {
		if op.IssueIID == nil && (op.Phase == "in_flight" || op.Phase == "returned_uncertain") {
			want[op.ID] = struct{}{}
		}
	}
	if len(want) == 0 {
		return nil
	}
	all, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{})
	if err != nil {
		return errors.Join(errFindingGroupScanIncomplete, err)
	}
	return s.recordFindingGroupMatches(ctx, repoID, all, want)
}

// SetReworkCanceller wires the mid-flight mr_rework abort collaborator (issue #853).
// Call once at startup, before the poller runs. A nil canceller (the default) disables
// the abort; the rest of the sync is unchanged.
func (s *Service) SetReworkCanceller(c ReworkCanceller) { s.reworkCanceller = c }

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

// findingLabel resolves the configured incidental-finding marker label for the
// sync's third fetch (PRD #1183 Child B), falling back to the compiled-in default
// when unconfigured or on a settings read error — the same best-effort resolution
// uziLabel uses, and against the same s.labels dependency the Service already holds.
func (s *Service) findingLabel(ctx context.Context) string {
	if s.labels != nil {
		if l, _ := s.labels.FindingLabel(ctx); l != "" {
			return l
		}
	}
	return settings.DefaultFindingLabel
}

// EncryptToken seals a plaintext PAT for storage.
func (s *Service) EncryptToken(pat string) ([]byte, error) {
	return s.box.Seal([]byte(pat))
}

// ForgeForToken builds a driver from a plaintext token (the connect/verify path,
// before the token is stored).
func (s *Service) ForgeForToken(forgeType forge.Type, baseURL, token string) (forge.Forge, error) {
	return s.forgeBuilder(forgeType, baseURL, token, s.timeout)
}

// ForgeForConnection builds a driver from a stored connection by decrypting its
// token ciphertext.
func (s *Service) ForgeForConnection(forgeType, baseURL string, tokenCiphertext []byte) (forge.Forge, error) {
	plain, err := s.box.Open(tokenCiphertext)
	if err != nil {
		return nil, err
	}
	return s.ForgeForToken(forge.Type(forgeType), baseURL, string(plain))
}

// AutoMove plans a column label delta, writes it forge-first, then applies it to
// the current cached row. Forge errors leave the cache untouched; a driver can
// partially change remote labels before returning an error. No compensation is
// attempted. The returned issue is the re-cached row. Caller guards own closed
// issue and manual-drag policy.
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

	labelsJSON, err := json.Marshal(append([]string{}, newLabels...))
	if err != nil {
		return store.Issue{}, err
	}
	addJSON, _ := json.Marshal(append([]string{}, add...))
	removeJSON, _ := json.Marshal(append([]string{}, remove...))
	// UpsertIssueLabels omits assignee_ids from both its INSERT and its ON CONFLICT
	// SET, so the DB's forge-synced assignees are preserved from the existing row
	// rather than re-written from this caller's possibly-stale snapshot (PRD #767).
	return s.q.UpsertIssueLabels(ctx, store.UpsertIssueLabelsParams{
		RepoID:         issue.RepoID,
		ForgeIssueIid:  issue.ForgeIssueIid,
		Title:          issue.Title,
		State:          issue.State,
		Labels:         labelsJSON,
		AddLabels:      addJSON,
		RemoveLabels:   removeJSON,
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
// Applying an already-cached label is a local no-op with no forge call. Otherwise,
// apply first ensures the label exists on the project, pinned to color. Removal
// always sends the forge delta: cached absence does not prove forge absence.
// Both paths issue one UpdateIssueLabels with a single-element add or remove set.
//
// color pins the project label on apply and is ignored on remove. Only forge
// success updates the cache: apply uses a current-row delta, removal uses
// RemoveCachedIssueLabel and never inserts a missing row. A forge error leaves
// the cache untouched, but remote labels may have partially changed.
func (s *Service) SetIssueLabel(ctx context.Context, f forge.Forge, forgeProjectID int64, issue store.Issue, label, color string, apply bool) (store.Issue, error) {
	var current []string
	if err := json.Unmarshal(issue.Labels, &current); err != nil {
		current = []string{}
	}

	// Applying a cached label preserves the existing idempotent apply behavior.
	if apply && slices.Contains(current, label) {
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

	if !apply {
		return s.q.RemoveCachedIssueLabel(ctx, store.RemoveCachedIssueLabelParams{RepoID: issue.RepoID, ForgeIssueIid: issue.ForgeIssueIid, Label: label})
	}

	// The snapshot plus label seeds INSERT only; conflict applies the delta.
	next := append(append([]string{}, current...), label)
	labelsJSON, err := json.Marshal(next)
	if err != nil {
		return store.Issue{}, err
	}
	addJSON, _ := json.Marshal(append([]string{}, add...))
	removeJSON, _ := json.Marshal(append([]string{}, remove...))
	// UpsertIssueLabels omits assignee_ids from both its INSERT and its ON CONFLICT
	// SET, so the DB's forge-synced assignees are preserved from the existing row and
	// never re-written from this caller's possibly-stale snapshot (PRD #767).
	// has_prd_link is still carried through verbatim (this path never re-derives it).
	return s.q.UpsertIssueLabels(ctx, store.UpsertIssueLabelsParams{
		RepoID:         issue.RepoID,
		ForgeIssueIid:  issue.ForgeIssueIid,
		Title:          issue.Title,
		State:          issue.State,
		Labels:         labelsJSON,
		AddLabels:      addJSON,
		RemoveLabels:   removeJSON,
		WebUrl:         issue.WebUrl,
		Author:         issue.Author,
		HasPrdLink:     issue.HasPrdLink, // preserved verbatim; this path never re-derives it
		ForgeUpdatedAt: issue.ForgeUpdatedAt,
	})
}

// Marks holds the high-water marks the sync fetches carry — ONE PER FETCH
// (issue #177), not one shared between them. PRD bounds the uzi-labelled
// fetch (state=all); Open bounds the additive open, no-label fetch; Finding bounds
// the additive finding-labelled fetch (state=all, PRD #1183 Child B). The
// per-fetch independence argument below is written about the PRD/Open pair — the
// original race issue #177 dissolved — and the Finding mark sits on exactly the same
// footing: its own bound, its own evidence, no constraint from or on the other two.
// Each mark advances only on its OWN fetch's evidence: to the max forge updated_at over
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
	PRD  time.Time // bounds the uzi-labelled fetch (state=all)
	Open time.Time // bounds the additive open, no-label fetch
	// Finding bounds the finding-labelled fetch (state=all), PRD #1183 Child B. It is
	// a THIRD independent per-fetch mark on exactly the same footing as PRD and Open —
	// advanced only on its own fetch's raw-result evidence, never constrained by the
	// other two, and left untouched by an empty result. In-memory only, like the whole
	// struct (the poller's repoState holds it and nothing persists it), so no
	// migration or persistence change rides along with the new field.
	Finding time.Time
}

// Advance folds an observed Marks into m FIELD-WISE, keeping the later of each.
// No mark regresses, and none is constrained by another's value — the
// zero time an empty (or unissued) fetch reports simply loses the comparison. Combining
// ACROSS the fields is exactly what this type exists to stop; see Marks for why a shared
// mark had to, and why a per-fetch one must not.
func (m Marks) Advance(next Marks) Marks {
	if next.PRD.After(m.PRD) {
		m.PRD = next.PRD
	}
	if next.Open.After(m.Open) {
		m.Open = next.Open
	}
	if next.Finding.After(m.Finding) {
		m.Finding = next.Finding
	}
	return m
}

// FullSync fetches the complete uzi-labeled set (state=all, no lower bound); since
// PRD #102 M6, every OPEN issue regardless of label — an ADDITIVE second fetch, not
// a widening of the first (Decision 9); and, since PRD #1183 Child B, every
// finding-labelled issue in ALL states — an ADDITIVE third fetch mirroring the first
// so CLOSED finding issues are observed. It upserts all three, then evicts cache rows
// absent from the UNION of them. This is the only path that observes de-labeling and
// deletion, so it doubles as the reconcile pass and the manual Refresh. (PRD #764 D7:
// the any-state fetch keys on the uzi label now that the PRD label has lost its
// special meaning.)
//
// FullSync is also the only path that settles unconfirmed finding-group
// operations by marker (reconcileFindingGroupMarkers): it makes one extra
// unfiltered all-states fetch, only while a matchable operation is pending. A
// failure of that fetch settles nothing and keeps every claim,
// but the issue sync below still runs and reports its marks: the marks bound
// only the label-filtered fetches, which match no markers, so advancing them
// hides nothing from a later scan. A database error while recording a match
// still fails the pass with the zero Marks and no cache writes.
//
// The third fetch closes a gap the first two structurally left open: a filed finding
// issue carries only the finding marker label, never the uzi label, so once it CLOSES
// the uzi fetch (uzi-labelled) never returns it and the open fetch (state=opened) no
// longer does either — it would be evicted and SyncFindingIssueCloses would never see
// the open→closed edge. Keyed on the finding label (state=all), the third fetch
// observes exactly those closed finding issues and keeps them cached as closed.
//
// It takes NO marks (it is unbounded by design) and returns the TRIPLE it OBSERVED,
// one mark per fetch, for the caller to fold into its own with Marks.Advance. On
// any error it returns the ZERO Marks, which folds in as a no-op, so a failed
// reconcile leaves the caller's marks exactly as they were.
//
// ALL THREE fetches must succeed before anything is deleted (Decision 11). A union
// missing one part is not authoritative, and treating it as one wipes whatever
// the failed part owns — the entire non-uzi backlog, every poll, on a transient
// forge error. There is deliberately no "the extra fetch is best-effort, log and
// continue" path: a soft-fail would also report a mark for a window nobody read
// (Decision 11a).
func (s *Service) FullSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge) (Marks, error) {
	pendingGroups, finishGroups, advanceGroups, pendingErr := s.pendingFindingGroups(ctx, repoID)
	defer finishGroups()
	if pendingErr != nil {
		return Marks{}, pendingErr
	}
	uziLabel := s.uziLabel(ctx)
	issues, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{uziLabel}})
	if err != nil {
		// Abort BEFORE any eviction: a failed/partial fetch must never be
		// treated as an authoritative empty set, or a transient forge error
		// would wipe the cache. Eviction only runs below, after ALL THREE fetches
		// have come back clean.
		return Marks{}, err
	}
	openIssues, err := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{State: forge.StateOpened})
	if err != nil {
		return Marks{}, err
	}
	extra := withoutLabel(openIssues, uziLabel)

	// Third fetch: finding-labelled issues in ALL states (State zero = StateAll),
	// mirroring the uzi fetch's shape. Additive from the first fetch, exactly like the
	// open fetch — withoutLabel drops any issue that ALSO carries the uzi label so it
	// stays owned by the uzi path (its state=all + eviction semantics). findingExtra
	// overlapping the open fetch for OPEN finding issues is harmless: UpsertIssue is
	// idempotent and DeleteIssuesNotIn dedupes the keep-set. Its error, like the first
	// two, returns BEFORE any eviction — extending the "all fetches succeed first"
	// invariant from two to three. When the finding label equals the uzi label (a
	// misconfig ValidateMerged forbids), the third fetch is SKIPPED: those issues are
	// already covered by the first fetch's state=all uzi query, so a third round trip
	// would be pure duplicate work.
	findingLabel := s.findingLabel(ctx)
	var findingExtra []forge.Issue
	var findingMark time.Time
	if findingLabel != uziLabel {
		findingIssues, ferr := f.ListIssues(ctx, forgeProjectID, forge.ListIssuesOptions{Labels: []string{findingLabel}})
		if ferr != nil {
			return Marks{}, ferr
		}
		findingExtra = withoutLabel(findingIssues, uziLabel)
		findingMark = maxUpdatedAt(findingIssues)
	}
	// Marker reconciliation runs over the complete issue list, after every
	// fetch above succeeded and before any cache write. An incomplete scan
	// leaves the page's claims unsettled and the sync continues; the cursor
	// still rotates, so recorded operations on other pages keep settling while
	// the scan cannot complete. Any other error returns the zero Marks, leaves
	// the cache untouched and does not advance the cursor.
	if err := s.reconcileFindingGroupMarkers(ctx, repoID, forgeProjectID, f, pendingGroups); err != nil {
		if !errors.Is(err, errFindingGroupScanIncomplete) {
			return Marks{}, err
		}
		slog.Warn("finding group marker scan incomplete; unconfirmed operations stay claimed",
			"repo_id", repoID, "error", err)
	}
	advanceGroups()

	if err := s.upsertIssues(ctx, repoID, issues); err != nil {
		return Marks{}, err
	}
	if err := s.upsertIssues(ctx, repoID, extra); err != nil {
		return Marks{}, err
	}
	if err := s.upsertIssues(ctx, repoID, findingExtra); err != nil {
		return Marks{}, err
	}
	// A clean SET of fetches that legitimately returns zero issues DOES evict
	// everything — the forge is the source of truth (empty means empty).
	keep := make([]int64, 0, len(issues)+len(extra)+len(findingExtra))
	for _, is := range issues {
		keep = append(keep, is.IID)
	}
	for _, is := range extra {
		keep = append(keep, is.IID)
	}
	for _, is := range findingExtra {
		keep = append(keep, is.IID)
	}
	if _, err := s.q.DeleteIssuesNotIn(ctx, store.DeleteIssuesNotInParams{RepoID: repoID, KeepIids: keep}); err != nil {
		return Marks{}, err
	}
	return Marks{PRD: maxUpdatedAt(issues), Open: maxUpdatedAt(openIssues), Finding: findingMark}, nil
}

// IncrementalSync fetches only the issues updated at/after each fetch's OWN mark
// and upserts them — the uzi-labeled set (state=all), the additive open, no-label
// set, and (PRD #1183 Child B) the additive finding-labelled set (state=all), the
// same three FullSync takes. It cannot see de-labeling or deletion (the filters
// structurally exclude them) — that is FullSync's job. Returns m advanced field-wise,
// never lower than what it was given. The bound is inclusive at second granularity,
// so the boundary row is re-fetched and deduped by upsert.
//
// The three lower bounds are guarded INDEPENDENTLY: a zero mark sends no
// updated_after on its own fetch and leaves the others' bounds alone. A single
// shared guard would drop every bound the moment any mark was zero, which is
// the unbounded re-read issue #177 is about.
//
// Any fetch failing returns the CALLER'S marks unchanged, along with the error
// (Decision 11a) — ALL marks held, not just the failed one's. Every fetch
// completes before any upsert runs, so a partial failure means no path's rows
// were written; advancing a successful path's mark would skip a window whose
// rows never reached the cache.
//
// IncrementalSync makes no finding-group marker match: its fetches are filtered
// by label and by updated_after, so they are not the complete issue set marker
// uniqueness requires. It only settles durably recorded group operations (those
// with a stored issue iid and url) on the current pending page, and it never
// advances the group cursor: only FullSync rotates pages, so every page is
// examined within as many FullSync passes as there are pages, however many
// incremental passes run between them.
//
// The finding fetch is SKIPPED when the finding label equals the uzi label (a
// misconfig ValidateMerged forbids): those issues are already covered by the uzi
// fetch, so a third round trip would be pure duplicate work. Its mark then never
// advances, which is correct — an unissued fetch is no evidence.
func (s *Service) IncrementalSync(ctx context.Context, repoID uuid.UUID, forgeProjectID int64, f forge.Forge, m Marks) (Marks, error) {
	_, finishGroups, _, pendingErr := s.pendingFindingGroups(ctx, repoID)
	defer finishGroups()
	if pendingErr != nil {
		return m, pendingErr
	}
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
	findingLabel := s.findingLabel(ctx)
	var findingIssues []forge.Issue
	if findingLabel != uziLabel {
		findingOpts := forge.ListIssuesOptions{Labels: []string{findingLabel}}
		if !m.Finding.IsZero() {
			findingOpts.UpdatedAfter = &m.Finding
		}
		findingIssues, err = f.ListIssues(ctx, forgeProjectID, findingOpts)
		if err != nil {
			return m, err
		}
	}
	// No marker matching and NO cursor advance here: an UpdatedAfter-filtered,
	// label-filtered page is not the complete set marker uniqueness needs.
	// pendingFindingGroups above settled the recorded iids of the current page;
	// the page rotates only in FullSync (the returned advance closure is
	// deliberately ignored), so incremental passes cannot shift which page the
	// next FullSync examines. Recorded-op settlement on later pages therefore
	// happens as FullSync rotates to them.
	if err := s.upsertIssues(ctx, repoID, issues); err != nil {
		return m, err
	}
	if err := s.upsertIssues(ctx, repoID, withoutLabel(openIssues, uziLabel)); err != nil {
		return m, err
	}
	if err := s.upsertIssues(ctx, repoID, withoutLabel(findingIssues, uziLabel)); err != nil {
		return m, err
	}
	return m.Advance(Marks{PRD: maxUpdatedAt(issues), Open: maxUpdatedAt(openIssues), Finding: maxUpdatedAt(findingIssues)}), nil
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
