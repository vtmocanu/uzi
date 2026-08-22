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
	// ErrProjectSyncAlreadyLinked is a Provision against a repo that already has a link
	// row (adopted or previously provisioned). Provision CREATES a new board, so it
	// must refuse rather than orphan the existing one — Disable never deletes a board,
	// so a silent re-provision would leak GitHub projects. Disable first, then re-provision.
	ErrProjectSyncAlreadyLinked = errors.New("this repo already has a linked project; disable sync before provisioning a new one")
	// ErrProjectSyncUserNotFound is a share/unshare against a GitHub login that does
	// not resolve (forge.ErrGitHubUserNotFound). The handler maps it to 422 so a bad
	// username is user-actionable, never a 500. Only a NOT_FOUND login maps here —
	// a stale board node id or any other forge error stays a generic 500.
	ErrProjectSyncUserNotFound = errors.New("no github user with that username")
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
	// TouchGithubProjectLinkSynced (M7) bumps last_synced_at on a completed reverse
	// pass WITHOUT touching last_error — a clean read records "we synced" without
	// clobbering a still-relevant forward-write error.
	TouchGithubProjectLinkSynced(ctx context.Context, repoID uuid.UUID) error
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
	repo, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
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

// projectSyncPreamble runs the preconditions Adopt and Provision share and returns
// the resolved repo row + the type-asserted ProjectBoardSyncer. The check order and
// the CLEAR sentinels it returns (ErrProjectSyncDisabled / NotGitHub / Unsupported /
// MissingScope, and a bare pgx.ErrNoRows for an unknown repo id) are exactly what the
// admin handler maps to a 4xx — extracting it keeps the two entry points DRY without
// changing either's observable behavior.
func (s *ProjectSyncService) projectSyncPreamble(ctx context.Context, repoID uuid.UUID) (store.GetRepoByIDRow, forge.ProjectBoardSyncer, error) {
	enabled, err := s.settings.GithubProjectSyncEnabled(ctx)
	if err != nil {
		return store.GetRepoByIDRow{}, nil, fmt.Errorf("project sync: read kill-switch: %w", err)
	}
	if !enabled {
		return store.GetRepoByIDRow{}, nil, ErrProjectSyncDisabled
	}

	repo, err := s.store.GetRepoByID(ctx, repoID)
	if err != nil {
		// pgx.ErrNoRows for an unknown id — the handler maps it to 404.
		return store.GetRepoByIDRow{}, nil, err
	}
	if repo.ForgeType != string(forge.TypeGitHub) {
		return store.GetRepoByIDRow{}, nil, ErrProjectSyncNotGitHub
	}

	f, err := s.forges.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		return store.GetRepoByIDRow{}, nil, fmt.Errorf("project sync: build forge: %w", err)
	}
	syncer, ok := f.(forge.ProjectBoardSyncer)
	if !ok {
		return store.GetRepoByIDRow{}, nil, ErrProjectSyncUnsupported
	}
	if err := ensureProjectScope(ctx, f); err != nil {
		return store.GetRepoByIDRow{}, nil, err
	}
	return repo, syncer, nil
}

// Provision AUTONOMOUSLY creates a GitHub Projects v2 board with uzi's OWN "uzi
// Status" single-select field (whose options are the repo's board columns), links it
// to the repo, persists the link with owned_by_uzi=true, and seeds it from the repo's
// cached issues (PRD #364 M4). It is the create-and-own counterpart to Adopt (which
// links a user's EXISTING board): an admin gets a working, seeded board with zero
// manual GitHub-UI clicks.
//
// v1 is BOOTSTRAP-ONCE (D5): the field's options are set at creation and there is NO
// ongoing option reconcile — a later board-column add/rename/remove is a documented
// manual step. This is deliberate: the destructive full-list-replace update (F9) that
// ongoing reconcile needs can clear every item's Status on a mis-echoed option id, so
// it is not built here. Creating uzi's own field is the SAFE half of F9 — a fresh
// field has no existing values to clear.
//
// owned_by_uzi=true records that uzi created this board; it governs the FUTURE
// teardown divergence (D8), but in v1 Disable stays local-only / non-destructive (M7)
// for both ownership values, so an accidental disable can never lose a board.
//
// Preconditions and the CLEAR sentinels it returns are identical to Adopt (see
// projectSyncPreamble). title defaults to a sensible value when empty.
func (s *ProjectSyncService) Provision(ctx context.Context, repoID uuid.UUID, ownerKind forge.ProjectV2OwnerKind, title string) error {
	repo, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return err
	}

	// Refuse to provision over an existing link. Provision CREATES a fresh board;
	// re-running it would build a duplicate and abandon the prior one (Disable never
	// deletes a board, so orphans would accumulate). Adopt is the idempotent path; a
	// deliberate re-provision requires Disable first.
	if _, lerr := s.store.GetGithubProjectLinkByRepo(ctx, repoID); lerr == nil {
		return ErrProjectSyncAlreadyLinked
	} else if !errors.Is(lerr, pgx.ErrNoRows) {
		return fmt.Errorf("project sync: check existing link: %w", lerr)
	}

	// Everything below can hit the forge and is captured on the link row's last_error
	// for observability once the link exists (mirrors Adopt).
	if err := s.provisionAndSeed(ctx, repo, syncer, ownerKind, title); err != nil {
		// Best-effort: stamp the failure if a link row already exists (a failure before
		// the link is persisted matches zero rows, which is fine).
		if serr := s.store.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
			LastError: pgtype.Text{String: truncateErr(err.Error()), Valid: true},
			RepoID:    repoID,
		}); serr != nil {
			s.log.Warn("project sync: record link error", "repo", repoID, "error", serr)
		}
		return err
	}
	if serr := s.store.ClearGithubProjectLinkError(ctx, repoID); serr != nil {
		s.log.Warn("project sync: clear link error", "repo", repoID, "error", serr)
	}
	return nil
}

// provisionColors is the fixed 8-color palette uzi cycles through when creating its
// own Status field's options (M4). GitHub's createProjectV2Field requires each
// SINGLE_SELECT option carry one of its 8-color enum; picking deterministically by
// column index means a re-provision produces the same colors and spans the enum so
// adjacent columns are visually distinct. Every entry is a valid GitHub color enum.
var provisionColors = []string{"BLUE", "GREEN", "YELLOW", "ORANGE", "RED", "PURPLE", "PINK", "GRAY"}

// provisionColor picks the palette color for the option at board-column index i,
// cycling the palette so a board with more columns than colors still resolves.
func provisionColor(i int) string {
	return provisionColors[i%len(provisionColors)]
}

// provisionAndSeed does the resolve → create-project → create-field → map →
// persist-link → seed-items work for Provision. Unlike adoptAndSeed there is NO
// unmatched-column note: uzi CREATES the field's options FROM the board columns, so
// every column matches by construction. The column→option map is still built from the
// CREATED field's returned Options because the option ids only exist after creation.
func (s *ProjectSyncService) provisionAndSeed(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, ownerKind forge.ProjectV2OwnerKind, title string) error {
	owner, name, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
	if err != nil {
		return fmt.Errorf("project sync: resolve repo slug: %w", err)
	}
	if title == "" {
		title = "uzi: " + name
	}

	ownerID, err := syncer.ResolveProjectV2OwnerID(ctx, owner, ownerKind)
	if err != nil {
		return fmt.Errorf("project sync: resolve owner id: %w", err)
	}
	repoNodeID, err := syncer.ResolveRepositoryNodeID(ctx, owner, name)
	if err != nil {
		return fmt.Errorf("project sync: resolve repository node id: %w", err)
	}

	// createProjectV2 with repositoryId already links the project to the repo; the
	// explicit LinkProjectV2ToRepository below is a best-effort second link for the
	// repo's Projects tab and is NON-FATAL.
	project, err := syncer.CreateProjectV2(ctx, ownerID, title, repoNodeID)
	if err != nil {
		return fmt.Errorf("project sync: create project: %w", err)
	}
	if lerr := syncer.LinkProjectV2ToRepository(ctx, project.ID, repoNodeID); lerr != nil {
		s.log.Warn("project sync: link project to repository", "repo", repo.ID, "error", lerr)
	}

	columns, err := s.store.ListBoardColumns(ctx, repo.ID)
	if err != nil {
		return fmt.Errorf("project sync: list board columns: %w", err)
	}
	newOptions := make([]forge.ProjectV2NewOption, 0, len(columns))
	for i, c := range columns {
		newOptions = append(newOptions, forge.ProjectV2NewOption{
			Name:  c.LabelName,
			Color: provisionColor(i),
		})
	}
	field, err := syncer.CreateProjectV2Field(ctx, project.ID, uziStatusFieldName, newOptions)
	if err != nil {
		return fmt.Errorf("project sync: create status field: %w", err)
	}

	// Build the column→option map + position from the CREATED field's option ids. Every
	// column matches by construction (we created the options from the columns).
	optionByName := make(map[string]string, len(field.Options))
	for _, o := range field.Options {
		optionByName[o.Name] = o.ID
	}
	columnOption := make(map[string]string, len(columns))
	position := make(map[string]int, len(columns))
	for _, c := range columns {
		position[c.LabelName] = int(c.Position)
		if optID, ok := optionByName[c.LabelName]; ok {
			columnOption[c.LabelName] = optID
		}
	}

	// Persist the link BEFORE seeding, so a mid-seed failure still records the link (and
	// its last_error). owned_by_uzi=TRUE — this is a uzi-created board (unlike adopt).
	optionsJSON, err := json.Marshal(columnOption)
	if err != nil {
		return fmt.Errorf("project sync: marshal status options: %w", err)
	}
	if _, err := s.store.UpsertGithubProjectLink(ctx, store.UpsertGithubProjectLinkParams{
		RepoID:        repo.ID,
		ProjectNodeID: project.ID,
		ProjectNumber: int64(project.Number),
		StatusFieldID: field.ID,
		StatusOptions: optionsJSON,
		OwnedByUzi:    true, // provision create-and-owns
	}); err != nil {
		return fmt.Errorf("project sync: persist link: %w", err)
	}

	if err := s.seedItems(ctx, repo, syncer, owner, name, project.ID, field.ID, columnOption, position); err != nil {
		return err
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

// Disable tears down a repo's project sync (PRD #364 M3, D8 semantics fixed in M7):
// it deletes ONLY uzi's LOCAL projection rows — every item row (which cascades with
// the repo, not the link, so it is deleted explicitly) and then the link row — and
// NEVER issues a destructive GitHub mutation. uzi does NOT delete or archive a user's
// GitHub project or its items on disable:
//
//   - An ADOPTED project (owned_by_uzi=false) is the user's; unlinking it from uzi
//     leaves it fully intact on GitHub, exactly as adopted.
//   - A uzi-OWNED project (owned_by_uzi=true — a future M4 create-and-own path) is
//     likewise KEPT/frozen on GitHub in v1: project deletion is a deliberate future
//     step, never done here, so an accidental disable can never lose a user's board.
//
// Both owned_by_uzi branches therefore do the same local-only teardown in v1; the
// branch is made explicit so the future M4 divergence has a home. Re-enabling is just
// re-Adopt, which is idempotent. Loading the link first lets an already-disabled repo
// (no link row) return nil rather than churn deletes.
func (s *ProjectSyncService) Disable(ctx context.Context, repoID uuid.UUID) error {
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already disabled — nothing to tear down.
		}
		return fmt.Errorf("project sync: load link for teardown: %w", err)
	}

	// Both branches delete only uzi's local rows and issue NO destructive GitHub
	// mutation. The switch documents the future M4 divergence without acting on it.
	switch {
	case link.OwnedByUzi:
		// uzi-owned (future M4): v1 keeps/freezes the project on GitHub — no delete,
		// no archive — and drops only the local projection, same as an adopted board.
	default:
		// Adopted (owned_by_uzi=false): the project is the user's and is kept intact
		// on GitHub; only the local link/items are removed.
	}

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

// GetVisibility reads the linked board's current `public` flag from GitHub (PRD
// #557 M2). It runs the shared projectSyncPreamble (instance-flag gate, GitHub-only,
// forge build, ProjectBoardSyncer assertion, scope preflight), then reads the link
// row for the board's node id and issues a single `node(...ProjectV2{public})`
// query. A repo with no link row surfaces pgx.ErrNoRows verbatim (handler → 404).
//
// A stale/deleted board node id makes graphqlDo wrap GitHub's NOT_FOUND with
// forge.ErrGitHubUserNotFound (that wrap is applied to EVERY NOT_FOUND, not just a
// user lookup). We deliberately do NOT translate it here: only the resolve step in
// ShareWithUser/Unshare maps to ErrProjectSyncUserNotFound (422). So a stale board
// propagates as a generic error → the handler's default 500, exactly as intended.
func (s *ProjectSyncService) GetVisibility(ctx context.Context, repoID uuid.UUID) (bool, error) {
	_, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return false, err
	}
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		// pgx.ErrNoRows passes through so the handler can 404 ("not sync-enabled").
		return false, err
	}
	return syncer.GetProjectV2Visibility(ctx, link.ProjectNodeID)
}

// RepoOwnerType reports whether the repo's owner is a GitHub User or Organization
// (PRD #576 M1, F-G), for the sync panel's Provision feasibility nudge. It runs the
// shared projectSyncPreamble (instance-flag gate, GitHub-only, forge build,
// ProjectBoardSyncer assertion, scope preflight), resolves the repo's owner login via
// RepoSlug, then issues a single `repositoryOwner(login){ __typename }` query. Unlike
// GetVisibility it needs NO link row — the nudge is for a not-yet-linked repo — so it
// never reads github_project_links. Errors (a bad slug, an unexpected __typename)
// propagate to the handler's default 500; the frontend treats any failure as
// "unresolved" and falls back to showing both paths.
func (s *ProjectSyncService) RepoOwnerType(ctx context.Context, repoID uuid.UUID) (forge.ProjectV2OwnerType, error) {
	repo, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return "", err
	}
	owner, _, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
	if err != nil {
		return "", fmt.Errorf("project sync: resolve repo slug: %w", err)
	}
	return syncer.ResolveRepositoryOwnerType(ctx, owner)
}

// SetVisibility writes the linked board's `public` flag on GitHub (PRD #557 M2) via
// `updateProjectV2`. Same preamble + link-row read as GetVisibility. As with
// GetVisibility, a stale board node id propagates as a generic error (→ 500), not
// ErrProjectSyncUserNotFound — the not-found translation is scoped to the username
// resolve step in ShareWithUser/Unshare, never the visibility path.
func (s *ProjectSyncService) SetVisibility(ctx context.Context, repoID uuid.UUID, public bool) error {
	_, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return err
	}
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	return syncer.SetProjectV2Visibility(ctx, link.ProjectNodeID, public)
}

// ShareWithUser grants a GitHub user READER access to the linked board (PRD #557
// M2): resolve the login to its node id, then set the collaborator role to READER
// via `updateProjectV2Collaborators` (upsert — a duplicate grant is a no-op success).
//
// D6 scoping: the not-found translation is applied ONLY to the ResolveUserNodeID
// return value. graphqlDo wraps EVERY GitHub NOT_FOUND with
// forge.ErrGitHubUserNotFound — including one from a stale board node id on the
// visibility path — so a blanket errors.Is over the whole method would mis-map an
// unrelated 500 to a 422. Here the only NOT_FOUND that can reach this errors.Is is a
// bad username, which is the one case that is user-actionable → ErrProjectSyncUserNotFound
// (422). Every other resolve error (transient/permission) propagates → default 500.
func (s *ProjectSyncService) ShareWithUser(ctx context.Context, repoID uuid.UUID, username string) error {
	_, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return err
	}
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	userID, err := syncer.ResolveUserNodeID(ctx, username)
	if err != nil {
		if errors.Is(err, forge.ErrGitHubUserNotFound) {
			return ErrProjectSyncUserNotFound
		}
		return err
	}
	return syncer.SetProjectV2Collaborator(ctx, link.ProjectNodeID, userID, forge.RoleReaderCollaborator)
}

// Unshare revokes a GitHub user's access to the linked board (PRD #557 M2): resolve
// the login, then set the collaborator role to NONE. The username not-found mapping
// is scoped to the resolve step exactly as in ShareWithUser (see its D6 note); the
// only behavioral difference is the role set — NONE instead of READER.
func (s *ProjectSyncService) Unshare(ctx context.Context, repoID uuid.UUID, username string) error {
	_, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return err
	}
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		return err
	}
	userID, err := syncer.ResolveUserNodeID(ctx, username)
	if err != nil {
		if errors.Is(err, forge.ErrGitHubUserNotFound) {
			return ErrProjectSyncUserNotFound
		}
		return err
	}
	return syncer.SetProjectV2Collaborator(ctx, link.ProjectNodeID, userID, forge.RoleNoneCollaborator)
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
		// existing item — fall through to the live-value no-op check below.
	case errors.Is(err, pgx.ErrNoRows):
		itemID, aerr := s.addForwardItem(ctx, repo, syncer, link.ProjectNodeID, issueIID)
		if aerr != nil {
			s.stampLinkError(ctx, repoID, aerr)
			return nil
		}
		// Drive the just-added item to the target and persist item node id + marker.
		// An Open/clear target ("") needs no Set: a just-added item already carries
		// "No Status", so a Set with "" would be a redundant clear mutation.
		if targetOption != "" {
			if err := syncer.SetProjectV2ItemStatus(ctx, link.ProjectNodeID, itemID, link.StatusFieldID, targetOption); err != nil {
				s.stampLinkError(ctx, repoID, err)
				return nil
			}
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

	// Live-value no-op (D7): read the item's CURRENT Status from the forge and skip
	// the mutation only when the board is ALREADY at the target. Comparing against a
	// LIVE read — not the stored marker — is what lets a uzi move win a race with a
	// concurrent GitHub-side drag: if the card was dragged since our last sync the
	// marker is stale but the live value is not, so we still re-assert the target the
	// user just chose. A live-read failure must not fail the drag: log and fall
	// through to the write (worst case a redundant same-value write).
	liveOption, lerr := syncer.ReadProjectV2ItemStatus(ctx, item.ItemNodeID, link.StatusFieldID)
	if lerr == nil && liveOption == targetOption {
		// Board already at target. Keep the marker in step with reality so the next
		// reverse poll does not read a stale marker as forge-side drift.
		if markerValue(item.LastStatusOptionID) != targetOption {
			if err := s.store.SetGithubProjectItemStatusMarker(ctx, store.SetGithubProjectItemStatusMarkerParams{
				LastStatusOptionID: optionMarker(targetOption),
				RepoID:             repoID,
				ForgeIssueIid:      issueIID,
			}); err != nil {
				s.log.Warn("project sync: forward move sync marker", "repo", repoID, "issue", issueIID, "error", err)
			}
		}
		return nil
	}
	if lerr != nil {
		s.log.Warn("project sync: forward move live-status read", "repo", repoID, "issue", issueIID, "error", lerr)
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
	// Column-name → position map, for board.ResolveColumn to seed a backfilled item's
	// Status from its current label column during reconcile.
	position := make(map[string]int, len(columns))
	for _, c := range columns {
		position[c.LabelName] = int(c.Position)
	}

	// Reconcile the item set BEFORE reading live statuses (M7): backfill open issues
	// created since adopt (so they appear on the board and in the diff below) and prune
	// items for issues that have since closed. reconcileItems mutates itemsByIID in
	// place so the diff sees the reconciled set — a freshly-backfilled item carries
	// marker == its seeded value == the live value the diff then reads, so the same-tick
	// diff no-ops it (no oscillation).
	s.reconcileItems(ctx, repo, syncer, link, issues, itemsByIID, columnOption, position)

	live, err := syncer.ReadProjectV2ItemStatuses(ctx, link.ProjectNodeID, link.StatusFieldID)
	if err != nil {
		s.stampLinkErrorReverse(ctx, repoID, err)
		return nil
	}

	s.reverseDiff(ctx, repo, f, live, optionColumn, issuesByIID, itemsByIID, columns)

	// Observability (M7): record that a reverse pass completed by bumping last_synced_at.
	// This deliberately does NOT clear last_error — a reverse READ succeeding says
	// nothing about the health of the forward WRITE path, and both directions share this
	// one link row's last_error (ForwardMove stamps it and, by convention, never clears
	// on success). We touch on EVERY completed pass, even one that stamped a per-item
	// error above, so last_synced_at means "last attempted sync" and last_error conveys
	// health independently. Clearing here on every read tick would wipe a still-relevant
	// forward error within one poll interval, so reverse only STAMPS errors (M5's
	// convention) and TOUCHES the sync time.
	if err := s.store.TouchGithubProjectLinkSynced(ctx, repoID); err != nil {
		s.log.Warn("project sync: reverse touch last_synced_at", "repo", repoID, "error", err)
	}
	return nil
}

// reconcileItems is the M7 reconcile step run at the START of a reverse tick, before
// the live-status diff, so the single per-tick poller call covers issues that appeared
// or closed since adopt:
//
//   - Backfill: an OPEN issue with no tracked item row is added to the linked project
//     and its Status seeded from its current label column (a mapped column → that
//     option; Open or an unmapped column → cleared "No Status"). Best-effort per issue:
//     an error is stamped on last_error and the loop continues, so one unresolvable
//     issue never aborts the whole tick.
//   - Close-prune (D1): a CLOSED issue that still has a tracked item row has that row
//     DELETED locally. uzi does NOT call GitHub — there is no archive method, and
//     leaving the closed issue's card on the project (with its last Status) is
//     acceptable: D1 makes Closed an issue-state, not a board column, and the reverse
//     diff already skips closed issues, so a stale card never drives a label. uzi
//     simply stops tracking the closed issue.
//
// It mutates itemsByIID in place (adds backfilled rows, removes pruned ones) so the
// caller's diff reads the reconciled projection. The repo slug is resolved lazily —
// only when at least one issue actually needs backfilling — so a steady-state tick with
// nothing new makes no extra forge call.
func (s *ProjectSyncService) reconcileItems(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, link store.GithubProjectLink, issues []store.Issue, itemsByIID map[int64]store.GithubProjectItem, columnOption map[string]string, position map[string]int) {
	var owner, name string
	slugResolved := false
	resolveSlug := func() error {
		if slugResolved {
			return nil
		}
		o, n, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
		if err != nil {
			return fmt.Errorf("project sync: reverse reconcile resolve slug: %w", err)
		}
		owner, name, slugResolved = o, n, true
		return nil
	}

	// Pass 1 — close-prune. This is slug-INDEPENDENT (a purely local delete), so it
	// runs first and to completion even when the forge slug cannot be resolved; a
	// backfill slug failure below must never strand a closed issue's stale item row.
	for _, issue := range issues {
		if issue.State != "closed" {
			continue
		}
		if _, tracked := itemsByIID[issue.ForgeIssueIid]; !tracked {
			continue
		}
		if err := s.store.DeleteGithubProjectItem(ctx, store.DeleteGithubProjectItemParams{
			RepoID:        repo.ID,
			ForgeIssueIid: issue.ForgeIssueIid,
		}); err != nil {
			s.log.Warn("project sync: reverse prune closed item", "repo", repo.ID, "issue", issue.ForgeIssueIid, "error", err)
			continue
		}
		delete(itemsByIID, issue.ForgeIssueIid)
	}

	// Pass 2 — backfill open untracked issues. Needs the repo slug (resolved lazily,
	// once). A slug failure is a per-repo condition: no issue can be backfilled, so
	// stamp and stop this pass — the close-prunes above already completed.
	for _, issue := range issues {
		if issue.State == "closed" {
			continue
		}
		if _, tracked := itemsByIID[issue.ForgeIssueIid]; tracked {
			continue
		}
		if err := resolveSlug(); err != nil {
			s.stampLinkErrorReverse(ctx, repo.ID, err)
			return
		}
		if err := s.backfillItem(ctx, repo, syncer, link, owner, name, issue, itemsByIID, columnOption, position); err != nil {
			s.stampLinkErrorReverse(ctx, repo.ID, err)
			continue
		}
	}
}

// backfillItem adds one open, untracked issue to the linked project and seeds its
// Status from its current label column, persisting the item row + marker and recording
// it in itemsByIID for the same-tick diff. The seeded marker == the value the just-set
// Status will read back, so the same-tick diff no-ops the new item (convergence). A
// freshly-added item defaults to "No Status", so a cleared (Open/unmapped) seed needs
// no redundant Set — only a mapped column drives a Status write.
func (s *ProjectSyncService) backfillItem(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, link store.GithubProjectLink, owner, name string, issue store.Issue, itemsByIID map[int64]store.GithubProjectItem, columnOption map[string]string, position map[string]int) error {
	contentID, err := syncer.ResolveIssueNodeID(ctx, owner, name, int(issue.ForgeIssueIid))
	if err != nil {
		return fmt.Errorf("project sync: reverse backfill resolve issue #%d node: %w", issue.ForgeIssueIid, err)
	}
	itemID, err := syncer.AddProjectV2Item(ctx, link.ProjectNodeID, contentID)
	if err != nil {
		return fmt.Errorf("project sync: reverse backfill add issue #%d: %w", issue.ForgeIssueIid, err)
	}

	// Seed the Status from the issue's current column: a mapped column → its option;
	// Open ("") or an unmapped column → cleared ("No Status").
	var labels []string
	if len(issue.Labels) > 0 {
		if err := json.Unmarshal(issue.Labels, &labels); err != nil {
			labels = nil
		}
	}
	column, _, _ := board.ResolveColumn(labels, issue.State, position)
	seededOption := ""
	if column != "" {
		if optID, mapped := columnOption[column]; mapped {
			seededOption = optID
		}
	}
	if seededOption != "" {
		if err := syncer.SetProjectV2ItemStatus(ctx, link.ProjectNodeID, itemID, link.StatusFieldID, seededOption); err != nil {
			return fmt.Errorf("project sync: reverse backfill seed issue #%d status: %w", issue.ForgeIssueIid, err)
		}
	}
	if _, err := s.store.UpsertGithubProjectItem(ctx, store.UpsertGithubProjectItemParams{
		RepoID:             repo.ID,
		ForgeIssueIid:      issue.ForgeIssueIid,
		ItemNodeID:         itemID,
		LastStatusOptionID: optionMarker(seededOption),
	}); err != nil {
		return fmt.Errorf("project sync: reverse backfill persist issue #%d item: %w", issue.ForgeIssueIid, err)
	}
	// Record in the in-memory map so the same-tick diff reads live == marker → no-op.
	itemsByIID[issue.ForgeIssueIid] = store.GithubProjectItem{
		RepoID:             repo.ID,
		ForgeIssueIid:      issue.ForgeIssueIid,
		ItemNodeID:         itemID,
		LastStatusOptionID: optionMarker(seededOption),
	}
	return nil
}

// reverseDiff is the M6 diff pass, extracted from ReverseSync (M7 refactor): for each
// live project item it compares the live Status option against the stored marker and,
// on a GitHub-side change, writes the mapped column label via the ordinary AutoMove
// path, then advances the marker. See ReverseSync's doc for the convergence invariant.
func (s *ProjectSyncService) reverseDiff(ctx context.Context, repo store.GetRepoByIDRow, f forge.Forge, live []forge.ProjectV2ItemStatus, optionColumn map[string]string, issuesByIID map[int64]store.Issue, itemsByIID map[int64]store.GithubProjectItem, columns []store.BoardColumn) {
	repoID := repo.ID
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

		// The issue must be in uzi's cache (reconcile above backfills new open issues).
		// A closed issue's board state is its issue state (D1) — skip; reconcile has
		// already pruned its item row.
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
}

// ProjectSyncStatus is the health/observability snapshot of a repo's project sync
// link (PRD #364 M7), returned by ProjectSyncStatus for the admin health endpoint.
// It carries no secret and no forge coordinate the UI cannot already see — just the
// project number, ownership, the two health signals (last_synced_at, last_error), and
// the count of tracked items.
type ProjectSyncStatus struct {
	ProjectNumber int64
	OwnedByUzi    bool
	LastSyncedAt  pgtype.Timestamptz
	LastError     pgtype.Text
	ItemCount     int
}

// ProjectSyncStatus reads a repo's link row + tracked-item count for the admin health
// endpoint (PRD #364 M7). It is a pure STORE read of the reconciled projection — no
// forge call — so it is cheap and cannot fail on a wedged upstream. It returns
// pgx.ErrNoRows when the repo has no link row, which the handler maps to 404 ("no link
// = not sync-enabled").
func (s *ProjectSyncService) ProjectSyncStatus(ctx context.Context, repoID uuid.UUID) (ProjectSyncStatus, error) {
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		// pgx.ErrNoRows passes through verbatim so the handler can 404 on it.
		return ProjectSyncStatus{}, err
	}
	items, err := s.store.ListGithubProjectItems(ctx, repoID)
	if err != nil {
		return ProjectSyncStatus{}, fmt.Errorf("project sync: status list items: %w", err)
	}
	return ProjectSyncStatus{
		ProjectNumber: link.ProjectNumber,
		OwnedByUzi:    link.OwnedByUzi,
		LastSyncedAt:  link.LastSyncedAt,
		LastError:     link.LastError,
		ItemCount:     len(items),
	}, nil
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
