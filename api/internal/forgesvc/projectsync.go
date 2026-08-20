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

	"github.com/google/uuid"
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

// ProjectSyncService adopts and seeds an existing GitHub Projects v2 board against
// a repo's label board (PRD #364 M3).
type ProjectSyncService struct {
	store    ProjectSyncStore
	forges   ProjectForgeBuilder
	settings ProjectSyncSettings
	log      *slog.Logger
}

// NewProjectSync constructs the provisioning service. A nil log defaults to
// slog.Default() so callers (and tests) need not pass one.
func NewProjectSync(st ProjectSyncStore, forges ProjectForgeBuilder, settings ProjectSyncSettings, log *slog.Logger) *ProjectSyncService {
	if log == nil {
		log = slog.Default()
	}
	return &ProjectSyncService{store: st, forges: forges, settings: settings, log: log}
}

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
	// Success. A non-empty note (unmatched board columns) is still worth surfacing
	// on the link row rather than clearing it; a clean run clears last_error.
	if note != "" {
		if serr := s.store.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
			LastError: pgtype.Text{String: truncateErr(note), Valid: true},
			RepoID:    repoID,
		}); serr != nil {
			s.log.Warn("project sync: record adopt note", "repo", repoID, "error", serr)
		}
	} else if serr := s.store.ClearGithubProjectLinkError(ctx, repoID); serr != nil {
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
// forge error cannot bloat the row.
func truncateErr(s string) string {
	const max = 500
	if len(s) > max {
		return s[:max]
	}
	return s
}
