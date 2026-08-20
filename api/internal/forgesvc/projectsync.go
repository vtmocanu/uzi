package forgesvc

// projectsync.go is the GitHub Projects v2 Status-sync provisioning service
// (PRD #364 M3): adopt/link an EXISTING project to a repo's label board and seed
// it. It lives OUTSIDE service.go deliberately — the issue-cache sync core has no
// reason to grow the Projects v2 dependency surface, and only the admin adopt/
// disable handlers (M3) and the poller (M5+) reach this type.
//
// It is forge-agnostic at the seam: it type-asserts the built forge.Forge to
// forge.ProjectBoardSyncer and errors cleanly when the assertion fails (a
// non-GitHub repo), which is the SC-4 forge-isolation guarantee. The project board
// is authoritative; the two store side-tables (github_project_links,
// github_project_items) are a reconciled projection, never the source of truth.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// statusFieldName is GitHub's built-in single-select field the sync projects the
// label board onto. If a project has no such field (e.g. it was renamed), the
// adopt path falls back to uziStatusFieldName — the name uzi's OWN field carries
// when M4 creates one — so a board uzi provisioned itself still resolves.
const (
	statusFieldName    = "Status"
	uziStatusFieldName = "uzi Status"
)

// Non-fatal adopt errors the admin handler maps to 4xx. They are CLEAR (carry no
// secret) and distinguishable via errors.Is so the handler need not string-match.
var (
	// ErrProjectSyncDisabled is the instance kill-switch being off (settings default).
	ErrProjectSyncDisabled = errors.New("github project sync is disabled instance-wide")
	// ErrProjectSyncNotGitHub is an adopt against a non-GitHub repo.
	ErrProjectSyncNotGitHub = errors.New("project sync is only supported on github repos")
	// ErrProjectSyncUnsupported is a forge whose driver does not implement
	// ProjectBoardSyncer (the type assertion failed) — the SC-4 isolation path.
	ErrProjectSyncUnsupported = errors.New("this forge does not support project board sync")
	// ErrProjectSyncMissingScope is a connection PAT that can be introspected and is
	// missing the `project` scope the Projects v2 mutations require.
	ErrProjectSyncMissingScope = errors.New("connection token is missing the required 'project' scope for project sync")
)

// ProjectSyncStore is the subset of store methods the provisioning service needs.
// *store.Queries satisfies it; narrowing to an interface lets Adopt/Disable be
// unit-tested against a fake store with no live database.
type ProjectSyncStore interface {
	GetRepoByID(ctx context.Context, id uuid.UUID) (store.GetRepoByIDRow, error)
	ListBoardColumns(ctx context.Context, repoID uuid.UUID) ([]store.BoardColumn, error)
	ListIssuesByRepo(ctx context.Context, repoID uuid.UUID) ([]store.Issue, error)
	UpsertGithubProjectLink(ctx context.Context, arg store.UpsertGithubProjectLinkParams) (store.GithubProjectLink, error)
	SetGithubProjectLinkError(ctx context.Context, arg store.SetGithubProjectLinkErrorParams) error
	ClearGithubProjectLinkError(ctx context.Context, repoID uuid.UUID) error
	UpsertGithubProjectItem(ctx context.Context, arg store.UpsertGithubProjectItemParams) (store.GithubProjectItem, error)
	ListGithubProjectItems(ctx context.Context, repoID uuid.UUID) ([]store.GithubProjectItem, error)
	DeleteGithubProjectItem(ctx context.Context, arg store.DeleteGithubProjectItemParams) error
	DeleteGithubProjectLink(ctx context.Context, repoID uuid.UUID) error
	// Forward sync (M5): the link row is the per-repo enable state, the item row is
	// the projection marker read to skip a redundant Status write.
	GetGithubProjectLinkByRepo(ctx context.Context, repoID uuid.UUID) (store.GithubProjectLink, error)
	GetGithubProjectItem(ctx context.Context, arg store.GetGithubProjectItemParams) (store.GithubProjectItem, error)
	SetGithubProjectItemStatusMarker(ctx context.Context, arg store.SetGithubProjectItemStatusMarkerParams) error
}

// ProjectForgeBuilder builds a forge driver from a stored (encrypted) connection.
// *Service satisfies it (ForgeForConnection). Kept as an interface so the sync's
// tests can inject a fake forge without the secretbox/HTTP machinery.
type ProjectForgeBuilder interface {
	ForgeForConnection(forgeType, baseURL string, tokenCiphertext []byte) (forge.Forge, error)
}

// ProjectSyncSettings resolves the instance kill-switch. *settings.Cache satisfies
// it; the sync depends on the behavior, not the concrete cache.
type ProjectSyncSettings interface {
	GithubProjectSyncEnabled(ctx context.Context) (bool, error)
}

// ProjectMover is the narrow reverse-writeback collaborator the M6 poller needs:
// it writes a single-column label move forge-first (the ordinary AutoMove path).
// *forgesvc.Service satisfies it. It is injected via SetMover (an OPTIONAL
// collaborator) rather than threaded through NewProjectSync so the existing M3/M5
// constructor callers — and their positional tests — stay unchanged. AutoMove
// deliberately lives on *Service, not on this provisioning type, and is NOT one of
// the two forward-hooked call sites (D6), so a reverse label write does not
// re-project back onto Status.
type ProjectMover interface {
	AutoMove(ctx context.Context, f forge.Forge, forgeProjectID int64, issue store.Issue, columns []store.BoardColumn, target string) (store.Issue, error)
}

// ProjectSyncService adopts and seeds an existing GitHub Projects v2 board against
// a repo's label board (PRD #364 M3), and drives the reverse (Status → label) sync
// (M6) when a mover is wired.
type ProjectSyncService struct {
	store    ProjectSyncStore
	forges   ProjectForgeBuilder
	settings ProjectSyncSettings
	log      *slog.Logger

	// mover is the reverse-writeback collaborator (M6). Optional (nil-safe): a nil
	// mover disables reverse sync — ReverseSync logs and returns nil — so a
	// deployment or test without SetMover keeps forward-only behaviour. Set via
	// SetMover, mirroring the optional-collaborator pattern the poller uses.
	mover ProjectMover
}

// NewProjectSync constructs the provisioning service. A nil log defaults to
// slog.Default() so callers (and tests) need not pass one.
func NewProjectSync(st ProjectSyncStore, forges ProjectForgeBuilder, settings ProjectSyncSettings, log *slog.Logger) *ProjectSyncService {
	if log == nil {
		log = slog.Default()
	}
	return &ProjectSyncService{store: st, forges: forges, settings: settings, log: log}
}

// SetMover wires the reverse-writeback collaborator (PRD #364 M6). Call once at
// startup, before the poller runs. A nil mover (the default) disables reverse sync.
func (s *ProjectSyncService) SetMover(m ProjectMover) { s.mover = m }

// Adopt links an EXISTING Projects v2 board (identified by owner-kind + number) to
// repoID and seeds it from the repo's cached issues (PRD #364 M3). It is
// idempotent: re-adopting refreshes the link and re-diffs every item. owned_by_uzi
// is persisted false — M4 owns the create-and-own path.
//
// Preconditions that return a CLEAR, non-fatal error (mapped to 4xx by the
// handler): the instance kill-switch is off, the repo is not GitHub, the forge
// driver has no Projects v2 capability, or an introspectable PAT lacks `project`.
// An unintrospectable PAT proceeds best-effort — the first mutation surfaces the
// real error, captured in the link row's last_error.
func (s *ProjectSyncService) Adopt(ctx context.Context, repoID uuid.UUID, projectNumber int, ownerKind forge.ProjectV2OwnerKind) error {
	enabled, err := s.settings.GithubProjectSyncEnabled(ctx)
	if err != nil {
		return fmt.Errorf("project sync: read kill-switch: %w", err)
	}
	if !enabled {
		return ErrProjectSyncDisabled
	}

	repo, err := s.store.GetRepoByID(ctx, repoID)
	if err != nil {
		// pgx.ErrNoRows for an unknown id — the handler maps it to 404.
		return err
	}
	if repo.ForgeType != string(forge.TypeGitHub) {
		return ErrProjectSyncNotGitHub
	}

	f, err := s.forges.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		return fmt.Errorf("project sync: build forge: %w", err)
	}
	syncer, ok := f.(forge.ProjectBoardSyncer)
	if !ok {
		return ErrProjectSyncUnsupported
	}
	if err := ensureProjectScope(ctx, f); err != nil {
		return err
	}

	// Everything below can hit the forge and is captured on the link row's
	// last_error for observability once the link exists.
	note, err := s.adoptAndSeed(ctx, repo, syncer, projectNumber, ownerKind)
	if err != nil {
		// Best-effort: stamp the failure if a link row already exists (a resolve
		// failure before the link is persisted matches zero rows, which is fine).
		if serr := s.store.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
			LastError: pgtype.Text{String: truncateErr(err.Error()), Valid: true},
			RepoID:    repoID,
		}); serr != nil {
			s.log.Warn("project sync: record link error", "repo", repoID, "error", serr)
		}
		return err
	}
	// Success clears last_error unconditionally. An unmatched-columns note is a
	// non-fatal advisory, NOT an error, so it must not land in last_error — a UI
	// or the poller treating a non-null last_error as "sync broken" would misread
	// a healthy link. The note is logged (see adoptAndSeed); a proper health
	// surface for advisories is M7's job.
	if note != "" {
		s.log.Info("project sync: adopt completed with advisory", "repo", repoID, "note", note)
	}
	if serr := s.store.ClearGithubProjectLinkError(ctx, repoID); serr != nil {
		s.log.Warn("project sync: clear link error", "repo", repoID, "error", serr)
	}
	return nil
}

// adoptAndSeed does the resolve → map → persist-link → seed-items work. It returns
// a human-readable NOTE describing any board columns that did not match a Status
// option (recorded on the link row) and an error on any hard failure.
func (s *ProjectSyncService) adoptAndSeed(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, projectNumber int, ownerKind forge.ProjectV2OwnerKind) (string, error) {
	owner, name, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
	if err != nil {
		return "", fmt.Errorf("project sync: resolve repo slug: %w", err)
	}

	project, err := syncer.ResolveProjectV2(ctx, owner, projectNumber, ownerKind)
	if err != nil {
		return "", fmt.Errorf("project sync: resolve project #%d: %w", projectNumber, err)
	}

	field, err := syncer.ProjectV2StatusFieldByName(ctx, project.ID, statusFieldName)
	if err != nil {
		// Fall back to uzi's own field name before giving up (a board uzi created).
		field, err = syncer.ProjectV2StatusFieldByName(ctx, project.ID, uziStatusFieldName)
		if err != nil {
			return "", fmt.Errorf("project sync: resolve status field: %w", err)
		}
	}

	// Column-name → option-id map: exact-match each board column label to a Status
	// option name. Unmatched columns are omitted and reported in the note.
	columns, err := s.store.ListBoardColumns(ctx, repo.ID)
	if err != nil {
		return "", fmt.Errorf("project sync: list board columns: %w", err)
	}
	optionByName := make(map[string]string, len(field.Options))
	for _, o := range field.Options {
		optionByName[o.Name] = o.ID
	}
	columnOption := make(map[string]string, len(columns))
	position := make(map[string]int, len(columns))
	var unmatched []string
	for _, c := range columns {
		position[c.LabelName] = int(c.Position)
		if optID, ok := optionByName[c.LabelName]; ok {
			columnOption[c.LabelName] = optID
		} else {
			unmatched = append(unmatched, c.LabelName)
		}
	}
	if len(unmatched) > 0 {
		s.log.Info("project sync: board columns without a matching Status option",
			"repo", repo.ID, "columns", unmatched)
	}

	// Best-effort link into the repo's Projects tab; never fatal to the seed.
	if repoNodeID, rerr := syncer.ResolveRepositoryNodeID(ctx, owner, name); rerr != nil {
		s.log.Warn("project sync: resolve repository node id", "repo", repo.ID, "error", rerr)
	} else if lerr := syncer.LinkProjectV2ToRepository(ctx, project.ID, repoNodeID); lerr != nil {
		s.log.Warn("project sync: link project to repository", "repo", repo.ID, "error", lerr)
	}

	// Persist the link BEFORE seeding items, so a mid-seed failure still records the
	// link (and its last_error) rather than losing the resolved coordinates.
	optionsJSON, err := json.Marshal(columnOption)
	if err != nil {
		return "", fmt.Errorf("project sync: marshal status options: %w", err)
	}
	if _, err := s.store.UpsertGithubProjectLink(ctx, store.UpsertGithubProjectLinkParams{
		RepoID:        repo.ID,
		ProjectNodeID: project.ID,
		ProjectNumber: int64(project.Number),
		StatusFieldID: field.ID,
		StatusOptions: optionsJSON,
		OwnedByUzi:    false, // adopt persists false; M4 create-and-owns
	}); err != nil {
		return "", fmt.Errorf("project sync: persist link: %w", err)
	}

	if err := s.seedItems(ctx, repo, syncer, owner, name, project.ID, field.ID, columnOption, position); err != nil {
		return "", err
	}
	return unmatchedNote(unmatched), nil
}

// seedItems reads the project's live item Statuses once, then for each OPEN cached
// issue ensures its project item exists and its Status matches the target derived
// from the label board.
func (s *ProjectSyncService) seedItems(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, owner, name, projectID, fieldID string, columnOption map[string]string, position map[string]int) error {
	live, err := syncer.ReadProjectV2ItemStatuses(ctx, projectID, fieldID)
	if err != nil {
		return fmt.Errorf("project sync: read project items: %w", err)
	}
	type itemState struct {
		itemID   string
		optionID string
	}
	byIssue := make(map[int64]itemState, len(live))
	for _, it := range live {
		if it.IssueNumber != 0 {
			byIssue[it.IssueNumber] = itemState{itemID: it.ItemID, optionID: it.OptionID}
		}
	}

	issues, err := s.store.ListIssuesByRepo(ctx, repo.ID)
	if err != nil {
		return fmt.Errorf("project sync: list issues: %w", err)
	}
	for _, issue := range issues {
		// D1: closed issues carry no Status option — they are skipped entirely.
		if issue.State == "closed" {
			continue
		}
		var labels []string
		if len(issue.Labels) > 0 {
			if err := json.Unmarshal(issue.Labels, &labels); err != nil {
				labels = nil
			}
		}
		column, closed, _ := board.ResolveColumn(labels, issue.State, position)
		if closed {
			continue // defensive: ResolveColumn also treats state=="closed" as closed
		}

		// Target option: Open ("" column) → CLEAR (D2: uzi's implicit Open maps to
		// GitHub's native "No Status"). Otherwise the mapped option, if any; a column
		// with no matching option is skipped (its issues keep their current Status).
		var targetOption string
		if column != "" {
			optID, mapped := columnOption[column]
			if !mapped {
				continue
			}
			targetOption = optID
		}

		state, present := byIssue[issue.ForgeIssueIid]
		if !present {
			// The item is not on the board yet: resolve the issue content node and add
			// it, then treat its current Status as unset.
			contentID, err := syncer.ResolveIssueNodeID(ctx, owner, name, int(issue.ForgeIssueIid))
			if err != nil {
				return fmt.Errorf("project sync: resolve issue #%d node: %w", issue.ForgeIssueIid, err)
			}
			itemID, err := syncer.AddProjectV2Item(ctx, projectID, contentID)
			if err != nil {
				return fmt.Errorf("project sync: add issue #%d to project: %w", issue.ForgeIssueIid, err)
			}
			state = itemState{itemID: itemID, optionID: ""}
		}

		// Only mutate when the live value differs from the target (idempotent).
		if state.optionID != targetOption {
			if err := syncer.SetProjectV2ItemStatus(ctx, projectID, state.itemID, fieldID, targetOption); err != nil {
				return fmt.Errorf("project sync: set issue #%d status: %w", issue.ForgeIssueIid, err)
			}
		}

		// Persist the projection: item node id + the target we drove it to. A cleared
		// (Open → No Status) item stores a NULL marker, matching the live "" a later
		// reverse-sync reads back.
		if _, err := s.store.UpsertGithubProjectItem(ctx, store.UpsertGithubProjectItemParams{
			RepoID:             repo.ID,
			ForgeIssueIid:      issue.ForgeIssueIid,
			ItemNodeID:         state.itemID,
			LastStatusOptionID: optionMarker(targetOption),
		}); err != nil {
			return fmt.Errorf("project sync: persist issue #%d item: %w", issue.ForgeIssueIid, err)
		}
	}
	return nil
}

// Disable tears down a repo's project sync (PRD #364 M3): delete the item rows and
// the link row. It does NOT touch the project board itself (the board is the user's;
// M7 refines whether uzi-owned boards are archived). Item rows are deleted
// explicitly because they cascade with the repo, not the link.
func (s *ProjectSyncService) Disable(ctx context.Context, repoID uuid.UUID) error {
	items, err := s.store.ListGithubProjectItems(ctx, repoID)
	if err != nil {
		return fmt.Errorf("project sync: list items for teardown: %w", err)
	}
	for _, it := range items {
		if err := s.store.DeleteGithubProjectItem(ctx, store.DeleteGithubProjectItemParams{
			RepoID:        repoID,
			ForgeIssueIid: it.ForgeIssueIid,
		}); err != nil {
			return fmt.Errorf("project sync: delete item #%d: %w", it.ForgeIssueIid, err)
		}
	}
	if err := s.store.DeleteGithubProjectLink(ctx, repoID); err != nil {
		return fmt.Errorf("project sync: delete link: %w", err)
	}
	return nil
}

// ForwardMove projects a uzi-originated label move onto the linked project's
// Status field (PRD #364 M5): the issue's card moved to targetColumn on the label
// board, so drive the matching Projects v2 Status option. It is BEST-EFFORT and
// NON-FATAL — every failure path returns nil so a drag or a run-lifecycle move
// never fails on account of the projection.
//
// It is hooked at the two uzi-originated AutoMove call sites (the drag handler and
// the run lifecycle), AFTER the label write succeeds — never inside AutoMove
// itself, which the reverse poller (M6) also calls: projecting a reverse write
// back onto the board would loop.
//
// targetColumn "" is uzi's implicit Open, which maps to GitHub's native "No
// Status" (D2): the option id is "" and the Status is CLEARED.
func (s *ProjectSyncService) ForwardMove(ctx context.Context, repoID uuid.UUID, issueIID int64, targetColumn string) error {
	// Instance kill-switch: sync off instance-wide → no-op.
	enabled, err := s.settings.GithubProjectSyncEnabled(ctx)
	if err != nil {
		s.log.Warn("project sync: forward move read kill-switch", "repo", repoID, "issue", issueIID, "error", err)
		return nil
	}
	if !enabled {
		return nil
	}

	// The LINK ROW presence is the per-repo enable state: no row → this repo is not
	// sync-enabled, nothing to project.
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		s.log.Warn("project sync: forward move load link", "repo", repoID, "issue", issueIID, "error", err)
		return nil
	}

	repo, err := s.store.GetRepoByID(ctx, repoID)
	if err != nil {
		s.log.Warn("project sync: forward move load repo", "repo", repoID, "issue", issueIID, "error", err)
		return nil
	}
	if repo.ForgeType != string(forge.TypeGitHub) {
		return nil
	}
	f, err := s.forges.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		s.log.Warn("project sync: forward move build forge", "repo", repoID, "issue", issueIID, "error", err)
		return nil
	}
	syncer, ok := f.(forge.ProjectBoardSyncer)
	if !ok {
		return nil
	}

	// Resolve the target Status option. Open ("" column) → "" (clear). A mapped
	// column → its option id. An UNMAPPED column (e.g. a "Later" with no matching
	// Status option) cannot be projected: log and no-op, leaving the card's current
	// Status untouched.
	var targetOption string
	if targetColumn != "" {
		var columnOption map[string]string
		if err := json.Unmarshal(link.StatusOptions, &columnOption); err != nil {
			s.log.Warn("project sync: forward move parse status options", "repo", repoID, "issue", issueIID, "error", err)
			return nil
		}
		optID, mapped := columnOption[targetColumn]
		if !mapped {
			s.log.Info("project sync: forward move skipped, column has no Status option",
				"repo", repoID, "issue", issueIID, "column", targetColumn)
			return nil
		}
		targetOption = optID
	}

	// Ensure the item exists on the board. A tracked item carries its node id and
	// last-known marker; a missing one is added now.
	item, err := s.store.GetGithubProjectItem(ctx, store.GetGithubProjectItemParams{RepoID: repoID, ForgeIssueIid: issueIID})
	switch {
	case err == nil:
		// existing item — fall through to the marker no-op check below.
	case errors.Is(err, pgx.ErrNoRows):
		itemID, aerr := s.addForwardItem(ctx, repo, syncer, link.ProjectNodeID, issueIID)
		if aerr != nil {
			s.stampLinkError(ctx, repoID, aerr)
			return nil
		}
		// Drive the just-added item to the target and persist item node id + marker.
		if err := syncer.SetProjectV2ItemStatus(ctx, link.ProjectNodeID, itemID, link.StatusFieldID, targetOption); err != nil {
			s.stampLinkError(ctx, repoID, err)
			return nil
		}
		if _, err := s.store.UpsertGithubProjectItem(ctx, store.UpsertGithubProjectItemParams{
			RepoID:             repoID,
			ForgeIssueIid:      issueIID,
			ItemNodeID:         itemID,
			LastStatusOptionID: optionMarker(targetOption),
		}); err != nil {
			s.log.Warn("project sync: forward move persist new item", "repo", repoID, "issue", issueIID, "error", err)
		}
		return nil
	default:
		s.log.Warn("project sync: forward move load item", "repo", repoID, "issue", issueIID, "error", err)
		return nil
	}

	// Stored-marker no-op (D7): the marker is uzi's last-known value from either
	// direction (both forward and the M6 reverse writeback advance it), so it is the
	// convergence basis both directions share — M8 depends on the two using the SAME
	// basis. This is deliberately NOT a per-move live forge read: if the marker
	// already equals the target (NULL marker treated as ""), the board is already
	// there, so skip the Status mutation. Worst case under a concurrent poller
	// interleave is a redundant same-value write, never a wrong label (PRD risk
	// analysis) — the same guarantee a live read would give, without the extra call.
	if markerValue(item.LastStatusOptionID) == targetOption {
		return nil
	}
	if err := syncer.SetProjectV2ItemStatus(ctx, link.ProjectNodeID, item.ItemNodeID, link.StatusFieldID, targetOption); err != nil {
		s.stampLinkError(ctx, repoID, err)
		return nil
	}
	// Advance the marker so the next reverse poll does not read this self-inflicted
	// change as forge-side drift.
	if err := s.store.SetGithubProjectItemStatusMarker(ctx, store.SetGithubProjectItemStatusMarkerParams{
		LastStatusOptionID: optionMarker(targetOption),
		RepoID:             repoID,
		ForgeIssueIid:      issueIID,
	}); err != nil {
		s.log.Warn("project sync: forward move update marker", "repo", repoID, "issue", issueIID, "error", err)
	}
	return nil
}

// addForwardItem resolves an issue's content node and adds it to the project,
// returning the new item node id. Split out so the ForwardMove item-missing branch
// stays readable.
func (s *ProjectSyncService) addForwardItem(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, projectNodeID string, issueIID int64) (string, error) {
	owner, name, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
	if err != nil {
		return "", fmt.Errorf("project sync: forward move resolve slug: %w", err)
	}
	contentID, err := syncer.ResolveIssueNodeID(ctx, owner, name, int(issueIID))
	if err != nil {
		return "", fmt.Errorf("project sync: forward move resolve issue #%d node: %w", issueIID, err)
	}
	itemID, err := syncer.AddProjectV2Item(ctx, projectNodeID, contentID)
	if err != nil {
		return "", fmt.Errorf("project sync: forward move add issue #%d: %w", issueIID, err)
	}
	return itemID, nil
}

// ReverseSync is the M6 reverse (Status → label) sync: for a GitHub sync-enabled
// repo it reads the linked project's live item Statuses, diffs each against the
// STORED marker (last_status_option_id), and for every GitHub-side change writes
// the matching column label through the ordinary AutoMove path — uzi's normal
// issue reconcile then follows the label to move its own board.
//
// It is BEST-EFFORT / NON-FATAL: every failure path returns nil so a wedged forge,
// a missing link, or a per-item error never surfaces a fatal error to the poller
// (which logs defensively but expects nil in practice).
//
// The convergence invariant (M8, SC-2): the no-op basis is the SAME stored marker
// the forward no-op (ForwardMove, M5) uses. ForwardMove advances the marker to the
// live OptionID whenever uzi itself sets Status, so:
//   - live OptionID == marker → NO-OP. This is exactly the case where uzi made the
//     change via forward (the marker already advanced), so reverse must NOT write a
//     label — that is what stops oscillation.
//   - live OptionID != marker → a GitHub-side drag → map the option back to a column
//     and write the label via AutoMove, then advance the marker to the live OptionID.
//
// Because AutoMove is NOT one of the forward-hooked call sites (D6), this writeback
// does not re-project to Status; and advancing the marker means the next poll reads
// live == marker → no-op. That double guard is the convergence. Concurrency: the
// forward path can interleave with this read from the HTTP handler; the stored-marker
// basis tolerates it — worst case a redundant same-value label write, never a wrong
// label.
func (s *ProjectSyncService) ReverseSync(ctx context.Context, repoID uuid.UUID) error {
	// Instance kill-switch: sync off instance-wide → no-op.
	enabled, err := s.settings.GithubProjectSyncEnabled(ctx)
	if err != nil {
		s.log.Warn("project sync: reverse read kill-switch", "repo", repoID, "error", err)
		return nil
	}
	if !enabled {
		return nil
	}

	// The LINK ROW presence is the per-repo enable state: no row → not sync-enabled.
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		s.log.Warn("project sync: reverse load link", "repo", repoID, "error", err)
		return nil
	}

	// A nil mover is the reverse kill-switch (SetMover not wired): nothing to write.
	if s.mover == nil {
		s.log.Info("project sync: reverse sync disabled (no mover wired)", "repo", repoID)
		return nil
	}

	repo, err := s.store.GetRepoByID(ctx, repoID)
	if err != nil {
		s.log.Warn("project sync: reverse load repo", "repo", repoID, "error", err)
		return nil
	}
	if repo.ForgeType != string(forge.TypeGitHub) {
		return nil
	}
	f, err := s.forges.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		s.log.Warn("project sync: reverse build forge", "repo", repoID, "error", err)
		return nil
	}
	syncer, ok := f.(forge.ProjectBoardSyncer)
	if !ok {
		return nil
	}

	// Reverse map option-id → column-name, by inverting the link's stored
	// column→option map. An option id not in this map is one uzi does not manage.
	var columnOption map[string]string
	if err := json.Unmarshal(link.StatusOptions, &columnOption); err != nil {
		s.log.Warn("project sync: reverse parse status options", "repo", repoID, "error", err)
		return nil
	}
	optionColumn := make(map[string]string, len(columnOption))
	for column, optID := range columnOption {
		optionColumn[optID] = column
	}

	// Issue cache (by iid) — AutoMove needs the full row; closed/absent issues are
	// skipped. Item rows (by iid) carry the stored marker + item node id.
	issues, err := s.store.ListIssuesByRepo(ctx, repoID)
	if err != nil {
		s.log.Warn("project sync: reverse list issues", "repo", repoID, "error", err)
		return nil
	}
	issuesByIID := make(map[int64]store.Issue, len(issues))
	for _, iss := range issues {
		issuesByIID[iss.ForgeIssueIid] = iss
	}

	items, err := s.store.ListGithubProjectItems(ctx, repoID)
	if err != nil {
		s.log.Warn("project sync: reverse list items", "repo", repoID, "error", err)
		return nil
	}
	itemsByIID := make(map[int64]store.GithubProjectItem, len(items))
	for _, it := range items {
		itemsByIID[it.ForgeIssueIid] = it
	}

	columns, err := s.store.ListBoardColumns(ctx, repoID)
	if err != nil {
		s.log.Warn("project sync: reverse list board columns", "repo", repoID, "error", err)
		return nil
	}

	live, err := syncer.ReadProjectV2ItemStatuses(ctx, link.ProjectNodeID, link.StatusFieldID)
	if err != nil {
		s.stampLinkErrorReverse(ctx, repoID, err)
		return nil
	}

	for _, it := range live {
		// Non-issue content (PR/draft) carries IssueNumber 0 — nothing to move.
		if it.IssueNumber == 0 {
			continue
		}

		// Stored-marker no-op: an absent item row is marker "" (never seen). live ==
		// marker is precisely the uzi-forward case (the marker already advanced), so
		// reverse must not write — this is the convergence guard.
		item, itemPresent := itemsByIID[it.IssueNumber]
		var marker string
		if itemPresent {
			marker = markerValue(item.LastStatusOptionID)
		}
		if it.OptionID == marker {
			continue
		}

		// A GitHub-side change. Resolve the target column: Open ("" option → No
		// Status) maps to uzi's implicit Open (target column ""). A non-empty option
		// not in the reverse map is one uzi does not manage (D5): log and SKIP,
		// LEAVING the marker as-is so it stays visible (and re-evaluates next tick if
		// the mapping later changes). This costs one Info line per unmanaged item per
		// tick — deliberate, so an unmapped drag is not silently swallowed.
		var targetColumn string
		if it.OptionID != "" {
			column, mapped := optionColumn[it.OptionID]
			if !mapped {
				s.log.Info("project sync: reverse skip, live Status option not in board map",
					"repo", repoID, "issue", it.IssueNumber, "option", it.OptionID)
				continue
			}
			targetColumn = column
		}

		// The issue must be in uzi's cache (M7 backfill / issue reconcile handles
		// unknown issues). A closed issue's board state is its issue state (D1) — skip.
		issue, ok := issuesByIID[it.IssueNumber]
		if !ok {
			s.log.Info("project sync: reverse skip, issue not in cache", "repo", repoID, "issue", it.IssueNumber)
			continue
		}
		if issue.State == "closed" {
			continue
		}

		// Write the label via the ordinary AutoMove path. On error, DO NOT advance the
		// marker (so the change is retried next tick) and stamp the link error.
		if _, err := s.mover.AutoMove(ctx, f, repo.ForgeProjectID, issue, columns, targetColumn); err != nil {
			s.stampLinkErrorReverse(ctx, repoID, err)
			continue
		}

		// Advance the marker to the live OptionID so the next poll reads live == marker
		// → no-op (the second convergence guard). An existing item row advances just
		// the marker; an absent one is upserted, carrying the live ItemID as the node id.
		if itemPresent {
			if err := s.store.SetGithubProjectItemStatusMarker(ctx, store.SetGithubProjectItemStatusMarkerParams{
				LastStatusOptionID: optionMarker(it.OptionID),
				RepoID:             repoID,
				ForgeIssueIid:      it.IssueNumber,
			}); err != nil {
				s.log.Warn("project sync: reverse advance marker", "repo", repoID, "issue", it.IssueNumber, "error", err)
			}
		} else {
			if _, err := s.store.UpsertGithubProjectItem(ctx, store.UpsertGithubProjectItemParams{
				RepoID:             repoID,
				ForgeIssueIid:      it.IssueNumber,
				ItemNodeID:         it.ItemID,
				LastStatusOptionID: optionMarker(it.OptionID),
			}); err != nil {
				s.log.Warn("project sync: reverse persist item", "repo", repoID, "issue", it.IssueNumber, "error", err)
			}
		}
	}

	// Deliberately do NOT clear last_error on a clean pass: a reverse READ succeeding
	// says nothing about the health of the forward WRITE path, and both directions
	// share this one link row's last_error (ForwardMove stamps it and, by convention,
	// never clears on success). Clearing here on every read tick would wipe a still-
	// relevant forward error within one poll interval. So reverse only STAMPS errors,
	// matching M5's convention.
	return nil
}

// stampLinkErrorReverse records a reverse-sync failure on the link row's last_error,
// best-effort — a stamp failure is only logged. Mirrors stampLinkError (M5) but with
// a reverse-specific log line.
func (s *ProjectSyncService) stampLinkErrorReverse(ctx context.Context, repoID uuid.UUID, cause error) {
	s.log.Warn("project sync: reverse sync failed", "repo", repoID, "error", cause)
	if serr := s.store.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
		LastError: pgtype.Text{String: truncateErr(cause.Error()), Valid: true},
		RepoID:    repoID,
	}); serr != nil {
		s.log.Warn("project sync: reverse record link error", "repo", repoID, "error", serr)
	}
}

// stampLinkError records a forward-move failure on the link row's last_error,
// best-effort — a forward move is not a full reconcile, so a stamp failure is only
// logged. A SUCCESSFUL forward move deliberately does NOT clear last_error (it
// leaves M3's adopt-time state).
func (s *ProjectSyncService) stampLinkError(ctx context.Context, repoID uuid.UUID, cause error) {
	s.log.Warn("project sync: forward move failed", "repo", repoID, "error", cause)
	if serr := s.store.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
		LastError: pgtype.Text{String: truncateErr(cause.Error()), Valid: true},
		RepoID:    repoID,
	}); serr != nil {
		s.log.Warn("project sync: forward move record link error", "repo", repoID, "error", serr)
	}
}

// markerValue reads a stored last_status_option_id marker as its option-id string,
// treating NULL (a cleared/Open item) as "" — the inverse of optionMarker.
func markerValue(m pgtype.Text) string {
	if !m.Valid {
		return ""
	}
	return m.String
}

// ensureProjectScope preflight-checks the connection PAT for the `project` scope.
// An introspection-unsupported forge (no scopes header) proceeds best-effort — the
// first mutation surfaces any real permission error. A transient introspection
// error is surfaced so the caller does not silently proceed on unknown scopes.
func ensureProjectScope(ctx context.Context, f forge.Forge) error {
	info, err := f.TokenInfo(ctx)
	if err != nil {
		if errors.Is(err, forge.ErrTokenIntrospectionUnsupported) {
			return nil
		}
		return fmt.Errorf("project sync: token introspection: %w", err)
	}
	for _, sc := range info.Scopes {
		if sc == "project" {
			return nil
		}
	}
	return ErrProjectSyncMissingScope
}

// optionMarker maps a target option id to the item's last_status_option_id marker:
// a mapped option is a valid text, while a cleared (Open → No Status) item is NULL,
// matching the live "" a reverse-sync reads back for a no-status item.
func optionMarker(optionID string) pgtype.Text {
	return pgtype.Text{String: optionID, Valid: optionID != ""}
}

// unmatchedNote renders the note recorded on the link row for board columns that
// had no matching Status option. Empty when every column matched.
func unmatchedNote(unmatched []string) string {
	if len(unmatched) == 0 {
		return ""
	}
	return fmt.Sprintf("%d board column(s) had no matching project Status option and were skipped: %v", len(unmatched), unmatched)
}

// truncateErr bounds the text written to the link row's last_error so a verbose
// forge error cannot bloat the row. It cuts on a rune boundary so a multibyte
// character straddling the limit is not split into an invalid UTF-8 sequence
// (which Postgres would reject on the write).
func truncateErr(s string) string {
	const max = 500
	if len(s) <= max {
		return s
	}
	// Back up off the limit until it lands on a rune start byte.
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
