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
	"time"
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

// Async-seed timing constants (PRD #576 M4). Adopt/Resync/Provision persist the link
// synchronously and run the slow item-seed in a background goroutine so the write
// handler returns immediately (fixing the cosmetic 502 the ~27s synchronous seed
// caused).
const (
	// seedTimeout bounds a wedged forge call so a background seed cannot run forever.
	// It caps the detached seed context.
	seedTimeout = 8 * time.Minute
	// seedSuppressLease is how long ReverseSync honors the per-repo seeding lease. It
	// is deliberately LONGER than seedTimeout so a normal seed always clears the lease
	// before it would expire; the lease-vs-boolean distinction only matters on a hard
	// process crash mid-seed, after which the poller reconciles once the lease ages out
	// (PRD M4 "converges on next tick").
	seedSuppressLease = 10 * time.Minute
	// seedFinalizeTimeout bounds the store writes that release the lease and stamp the
	// outcome. They run on a FRESH detached context (not the possibly-timed-out seed
	// context) so the lease is always cleared even after a seed timeout or panic.
	seedFinalizeTimeout = 30 * time.Second
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
	// ErrProjectSyncNotLinked is a Resync against a repo with no link row: there is
	// nothing to re-seed. The handler maps it to 404 ("not linked"). Distinct from a
	// bare pgx.ErrNoRows so the Resync path can 404 on a missing LINK without an
	// unknown-repo-id read (which would also be ErrNoRows) being conflated with it.
	ErrProjectSyncNotLinked = errors.New("this repo has no linked project to resync")
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
	// ResetGithubProjectItemMarkers (PRD #576 M6) clears EVERY tracked item's status
	// marker (→ NULL) for a repo. AutoCreateColumns calls it atomically with a
	// status_field_id switch to a freshly-created field: the new field reads "" for
	// every item, so leaving old-field markers in place would make the next reverse tick
	// see live("") != marker(old id) for all issues and fire the F-F mass-clear cascade.
	ResetGithubProjectItemMarkers(ctx context.Context, repoID uuid.UUID) error
	// TouchGithubProjectLinkSynced (M7) bumps last_synced_at on a completed reverse
	// pass WITHOUT touching last_error — a clean read records "we synced" without
	// clobbering a still-relevant forward-write error.
	TouchGithubProjectLinkSynced(ctx context.Context, repoID uuid.UUID) error
	// Seeding lease (PRD #576 M4): Mark takes the per-repo reverse-suppression lease
	// synchronously before an async seed launches; Clear releases it when the seed
	// finishes (success, error, or timeout).
	MarkGithubProjectLinkSeeding(ctx context.Context, repoID uuid.UUID) error
	ClearGithubProjectLinkSeeding(ctx context.Context, repoID uuid.UUID) error
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

	// background launches the async item-seed (PRD #576 M4). It defaults to
	// `go fn()` (see NewProjectSync); tests inject a synchronous `fn()` so the whole
	// adopt→seed flow runs in-line and ordering/lease assertions are deterministic
	// (no wall-clock, no sleeps, no goroutine races).
	background func(func())

	// reverseCapK / reverseCapPct are the two thresholds of the reverse per-tick
	// destructive-write cap (PRD #576 M5): a tick that would strip an existing column
	// label off more than reverseCapK issues AND more than reverseCapPct percent of the
	// tracked items is aborted wholesale (F-F / R1 / R1b). They are INJECTABLE service
	// fields (defaulted in NewProjectSync to 3 and 25) so a test can DISABLE the cap
	// (set reverseCapK huge) and assert the SAME fixture DOES fire the AutoMove calls —
	// the mutation control the cap's negative assertions need to be non-vacuous.
	reverseCapK   int
	reverseCapPct int
}

// NewProjectSync constructs the provisioning service. A nil log defaults to
// slog.Default() so callers (and tests) need not pass one.
func NewProjectSync(st ProjectSyncStore, forges ProjectForgeBuilder, settings ProjectSyncSettings, log *slog.Logger) *ProjectSyncService {
	if log == nil {
		log = slog.Default()
	}
	return &ProjectSyncService{
		store:    st,
		forges:   forges,
		settings: settings,
		log:      log,
		// Default: run the seed in a detached goroutine. Tests overwrite this with a
		// synchronous launcher to make the seam deterministic.
		background: func(fn func()) { go fn() },
		// Reverse destructive-write cap defaults (PRD #576 M5): a single genuine drag
		// (destructiveCount 1) always passes; a mass corruption event trips.
		reverseCapK:   3,
		reverseCapPct: 25,
	}
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

	// Resolve + persist the link SYNCHRONOUSLY (fast: a few forge resolves + one upsert),
	// then seed items in the background (PRD #576 M4) so the handler returns immediately.
	sp, note, err := s.adoptPrepare(ctx, repo, syncer, projectNumber, ownerKind)
	if err != nil {
		// This failed BEFORE any seed. Best-effort: stamp the failure if a link row
		// already exists (a resolve failure before the link is persisted matches zero
		// rows, which is fine). Kept synchronous — the async path only owns the seed.
		if serr := s.store.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
			LastError: pgtype.Text{String: truncateErr(err.Error()), Valid: true},
			RepoID:    repoID,
		}); serr != nil {
			s.log.Warn("project sync: record link error", "repo", repoID, "error", serr)
		}
		return err
	}
	// The link is persisted; the handler can return 200 now. An unmatched-columns note
	// is a non-fatal advisory, NOT an error, so it never lands in last_error — a UI or
	// the poller treating a non-null last_error as "sync broken" would misread a healthy
	// link. It is persisted on the link row (M3) and logged here.
	if note != "" {
		s.log.Info("project sync: adopt linked with advisory", "repo", repoID, "note", note)
	}
	// launchSeed marks the reverse-suppression lease synchronously, then seeds items on a
	// detached context; its finalize clears last_error on success or stamps it on failure.
	s.launchSeed(ctx, repo.ID, sp)
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

	// Create the board + field + persist the link SYNCHRONOUSLY (mirrors Adopt), then
	// seed items in the background (PRD #576 M4). Everything up to persist-link can hit
	// the forge and is captured on the link row's last_error once the link exists.
	sp, err := s.provisionPrepare(ctx, repo, syncer, ownerKind, title)
	if err != nil {
		// Pre-seed failure. Best-effort: stamp if a link row already exists (a failure
		// before the link is persisted matches zero rows, which is fine).
		if serr := s.store.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
			LastError: pgtype.Text{String: truncateErr(err.Error()), Valid: true},
			RepoID:    repoID,
		}); serr != nil {
			s.log.Warn("project sync: record link error", "repo", repoID, "error", serr)
		}
		return err
	}
	// Link persisted (owned_by_uzi=true); the handler can return 201 now. launchSeed marks
	// the reverse-suppression lease and seeds asynchronously; its finalize clears/stamps
	// last_error. Provision creates all options → the unmatched set is always empty.
	s.launchSeed(ctx, repo.ID, sp)
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

// doneColumnName is the reserved name of the "Done" projection option uzi appends on the
// create path (PRD #584 M1), so a CLOSED issue can later (M2) be projected to a dedicated
// Done status. It is NOT a board column: the column→option map is built by iterating the
// board columns slice, so appending this option to the FIELD's option list never leaks it
// into status_options. When a board column is ALREADY literally named "Done" (case-
// sensitive exact match) uzi does NOT append — that column's own option carries the name,
// and done_option_id points at it (capturing by name would otherwise be ambiguous — R6).
const doneColumnName = "Done"

// doneColumnColor is the fixed distinct color for the appended "Done" option. GREEN is a
// valid member of GitHub's ProjectV2SingleSelectFieldOptionColor enum (see provisionColors,
// which cycles GREEN as one of its eight) and reads as "done". It is appended AFTER the
// per-column cycled colors, so provisionColor stays keyed on the board-column index and the
// palette assignment for real columns is unchanged.
const doneColumnColor = "GREEN"

// appendDoneOption appends the reserved "Done" projection option (PRD #584 M1) to a
// create-path field's option list, UNLESS a board column is already literally named "Done"
// (case-sensitive exact match against LabelName), in which case that column's own option
// carries the name and nothing is appended. It never mutates the caller's columns slice —
// "Done" is added only to the FIELD options, not to the board columns that build the
// column→option map.
func appendDoneOption(options []forge.ProjectV2NewOption, columns []store.BoardColumn) []forge.ProjectV2NewOption {
	for _, c := range columns {
		if c.LabelName == doneColumnName {
			return options // a real "Done" column already provides the option
		}
	}
	return append(options, forge.ProjectV2NewOption{Name: doneColumnName, Color: doneColumnColor})
}

// provisionPrepare does the resolve → create-project → create-field → map →
// persist-link work for Provision, and returns the seedParams for the caller to seed
// asynchronously (PRD #576 M4). It does NOT seed items. Unlike adoptPrepare there is NO
// unmatched-column note: uzi CREATES the field's options FROM the board columns, so
// every column matches by construction. The column→option map is still built from the
// CREATED field's returned Options because the option ids only exist after creation.
func (s *ProjectSyncService) provisionPrepare(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, ownerKind forge.ProjectV2OwnerKind, title string) (seedParams, error) {
	owner, name, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
	if err != nil {
		return seedParams{}, fmt.Errorf("project sync: resolve repo slug: %w", err)
	}
	if title == "" {
		title = "uzi: " + name
	}

	ownerID, err := syncer.ResolveProjectV2OwnerID(ctx, owner, ownerKind)
	if err != nil {
		return seedParams{}, fmt.Errorf("project sync: resolve owner id: %w", err)
	}
	repoNodeID, err := syncer.ResolveRepositoryNodeID(ctx, owner, name)
	if err != nil {
		return seedParams{}, fmt.Errorf("project sync: resolve repository node id: %w", err)
	}

	// createProjectV2 with repositoryId already links the project to the repo; the
	// explicit LinkProjectV2ToRepository below is a best-effort second link for the
	// repo's Projects tab and is NON-FATAL.
	project, err := syncer.CreateProjectV2(ctx, ownerID, title, repoNodeID)
	if err != nil {
		return seedParams{}, fmt.Errorf("project sync: create project: %w", err)
	}
	if lerr := syncer.LinkProjectV2ToRepository(ctx, project.ID, repoNodeID); lerr != nil {
		s.log.Warn("project sync: link project to repository", "repo", repo.ID, "error", lerr)
	}

	columns, err := s.store.ListBoardColumns(ctx, repo.ID)
	if err != nil {
		return seedParams{}, fmt.Errorf("project sync: list board columns: %w", err)
	}
	newOptions := make([]forge.ProjectV2NewOption, 0, len(columns)+1)
	for i, c := range columns {
		newOptions = append(newOptions, forge.ProjectV2NewOption{
			Name:  c.LabelName,
			Color: provisionColor(i),
		})
	}
	// Append the reserved "Done" projection option (PRD #584 M1) unless a board column is
	// already named "Done". It rides the FIELD options only — the columnOption loop below
	// iterates the board columns slice, so "Done" never becomes a status option/column here.
	newOptions = appendDoneOption(newOptions, columns)
	field, err := syncer.CreateProjectV2Field(ctx, project.ID, uziStatusFieldName, newOptions)
	if err != nil {
		return seedParams{}, fmt.Errorf("project sync: create status field: %w", err)
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
	// The reserved "Done" option id (PRD #584 M1): the appended option, or a pre-existing
	// "Done" board column's option — either way it resolves by name off the created field.
	// "" only if neither was present (defensive; the append guarantees one of the two).
	doneOptionID := optionByName[doneColumnName]

	// Persist the link BEFORE seeding, so a mid-seed failure still records the link (and
	// its last_error). owned_by_uzi=TRUE — this is a uzi-created board (unlike adopt).
	optionsJSON, err := json.Marshal(columnOption)
	if err != nil {
		return seedParams{}, fmt.Errorf("project sync: marshal status options: %w", err)
	}
	if _, err := s.store.UpsertGithubProjectLink(ctx, store.UpsertGithubProjectLinkParams{
		RepoID:        repo.ID,
		ProjectNodeID: project.ID,
		ProjectNumber: int64(project.Number),
		StatusFieldID: field.ID,
		StatusOptions: optionsJSON,
		OwnedByUzi:    true, // provision create-and-owns
		// Provision CREATES the field's options from the columns, so every column
		// matches by construction — the unmatched set is always empty here. Pass an
		// explicit empty slice (the query's COALESCE makes nil safe too).
		UnmatchedColumns: []string{},
		DoneOptionID:     doneOptionID, // PRD #584 M1: reserved "Done" projection option
	}); err != nil {
		return seedParams{}, fmt.Errorf("project sync: persist link: %w", err)
	}

	return seedParams{
		repo:         repo,
		syncer:       syncer,
		owner:        owner,
		name:         name,
		projectID:    project.ID,
		fieldID:      field.ID,
		columnOption: columnOption,
		position:     position,
		doneOptionID: doneOptionID, // PRD #584 M2: seed closed issues to Done on re-seed
	}, nil
}

// seedParams carries the resolved inputs prepareSeedLink computes and hands to the
// seed step. Bundling them as one value keeps the seed a single, wrappable call —
// M4 will run that step in a background goroutine, so the persist-link work (which
// stays synchronous) and the seed work are cleanly separable seams.
type seedParams struct {
	repo         store.GetRepoByIDRow
	syncer       forge.ProjectBoardSyncer
	owner        string
	name         string
	projectID    string
	fieldID      string
	columnOption map[string]string
	position     map[string]int
	// doneOptionID is the link's reserved "Done" projection option id (PRD #584 M2),
	// threaded so a manual re-seed (Adopt/Resync/Provision/AutoCreateColumns) projects a
	// CLOSED issue to Done — mirroring the periodic reconcile path — instead of skipping
	// it. "" = no Done option, so closed issues are skipped (the pre-M2 behavior).
	doneOptionID string
}

// seed runs the item-seeding step for a prepared link. It is a thin wrapper over
// seedItems so callers (adopt/resync) invoke a single method — the seam M4 wraps in
// a goroutine. seedItems keeps its explicit positional signature (unchanged since
// PRD #364) so its many internal references stay stable.
func (s *ProjectSyncService) seed(ctx context.Context, p seedParams) error {
	return s.seedItems(ctx, p.repo, p.syncer, p.owner, p.name, p.projectID, p.fieldID, p.columnOption, p.position, p.doneOptionID)
}

// launchSeed runs the item-seeding step asynchronously (PRD #576 M4) so Adopt/Resync/
// Provision return immediately instead of blocking on the ~27s item loop (the cosmetic
// 502). The link is already persisted by prepareSeedLink/provisionPrepare before this is
// called.
//
// Ordering that matters:
//   - MarkGithubProjectLinkSeeding runs SYNCHRONOUSLY (on reqCtx) so the reverse-
//     suppression lease is set BEFORE launchSeed returns — a reverse poll landing right
//     after Adopt is already suppressed (ReverseSync checks the lease).
//   - The seed runs on a DETACHED context: context.WithoutCancel drops the request's
//     cancellation (which fires when the response is written and would kill the seed mid-
//     flight) while keeping its values; a WithTimeout bound (seedTimeout) caps a wedged
//     forge call.
//   - A single finalize defer ALWAYS runs — on normal return, seed error, OR panic — and
//     releases the lease + stamps the outcome on a FRESH short-lived context (not the
//     possibly-timed-out seed context), so the lease is cleared even after a timeout.
func (s *ProjectSyncService) launchSeed(reqCtx context.Context, repoID uuid.UUID, p seedParams) {
	if err := s.store.MarkGithubProjectLinkSeeding(reqCtx, repoID); err != nil {
		s.log.Warn("project sync: mark seeding lease", "repo", repoID, "error", err)
	}
	seedCtx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), seedTimeout)
	s.background(func() {
		defer cancel()
		var seedErr error
		defer func() {
			// A panic in the seed must not crash the process; capture it as the outcome.
			if r := recover(); r != nil {
				seedErr = fmt.Errorf("project sync: seed panicked: %v", r)
				s.log.Error("project sync: seed panic", "repo", repoID, "panic", r)
			}
			// Release the lease + stamp on a FRESH detached context: seedCtx may be done
			// (timed out / cancelled) and the clear MUST still run so the lease cannot
			// wedge reverse sync. WithoutCancel(reqCtx) keeps values but not reqCtx's
			// cancellation, and reqCtx is derived from the completed request.
			doneCtx, doneCancel := context.WithTimeout(context.WithoutCancel(reqCtx), seedFinalizeTimeout)
			defer doneCancel()
			if cerr := s.store.ClearGithubProjectLinkSeeding(doneCtx, repoID); cerr != nil {
				s.log.Warn("project sync: clear seeding lease", "repo", repoID, "error", cerr)
			}
			if seedErr != nil {
				// A seed failure is a real health signal → stamp last_error (mirrors the
				// synchronous stamp Adopt/Resync did before M4).
				if serr := s.store.SetGithubProjectLinkError(doneCtx, store.SetGithubProjectLinkErrorParams{
					LastError: pgtype.Text{String: truncateErr(seedErr.Error()), Valid: true},
					RepoID:    repoID,
				}); serr != nil {
					s.log.Warn("project sync: record seed error", "repo", repoID, "error", serr)
				}
				return
			}
			// Success clears last_error (the async counterpart of the old synchronous clear).
			if cerr := s.store.ClearGithubProjectLinkError(doneCtx, repoID); cerr != nil {
				s.log.Warn("project sync: clear link error", "repo", repoID, "error", cerr)
			}
		}()
		seedErr = s.seed(seedCtx, p)
	})
}

// prepareSeedLink is the shared adopt/resync core: given an ALREADY-resolved project
// (node id + number), it resolves the Status field via the caller-supplied resolveField
// closure (Adopt resolves BY NAME — "Status" falling back to uzi's own field name;
// Resync resolves BY ID off the stored link.StatusFieldID so it re-reads the SAME field
// the link already points at rather than re-resolving by name — PRD #582), builds the
// column→option map, computes the unmatched set and per-column positions, best-effort
// links the project into the repo's Projects tab, and PERSISTS the link INCLUDING the
// unmatched set. It returns the seed parameters for the caller to feed to seed(), plus
// the unmatched columns for the human-readable note. It does NOT seed items — that is
// the caller's separately invocable step (see seed()), so the persist-link and seed
// seams stay independent.
func (s *ProjectSyncService) prepareSeedLink(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, owner, name, projectID string, projectNumber int64, ownedByUzi bool, resolveField func(context.Context) (forge.ProjectV2StatusField, error)) (seedParams, []string, error) {
	field, err := resolveField(ctx)
	if err != nil {
		return seedParams{}, nil, fmt.Errorf("project sync: resolve status field: %w", err)
	}

	// Column-name → option-id map: exact-match each board column label to a Status
	// option name. Unmatched columns are omitted, reported in the note, and persisted.
	columns, err := s.store.ListBoardColumns(ctx, repo.ID)
	if err != nil {
		return seedParams{}, nil, fmt.Errorf("project sync: list board columns: %w", err)
	}
	optionByName := make(map[string]string, len(field.Options))
	for _, o := range field.Options {
		optionByName[o.Name] = o.ID
	}
	// Adopt-path "Done" projection option (PRD #584 M1): if the resolved field already has
	// a "Done" option, capture its id; else "" (no Done projection). "Done" is a RESERVED
	// name — it is NOT added to columnOption/unmatched below (the loop iterates board
	// columns, so a "Done" option with no matching board column is simply never visited).
	doneOptionID := optionByName[doneColumnName]
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
	} else if lerr := syncer.LinkProjectV2ToRepository(ctx, projectID, repoNodeID); lerr != nil {
		s.log.Warn("project sync: link project to repository", "repo", repo.ID, "error", lerr)
	}

	// Persist the link BEFORE seeding items, so a mid-seed failure still records the
	// link (and its last_error) rather than losing the resolved coordinates. The
	// unmatched set is persisted here (PRD #576 M3) so ProjectSyncStatus can surface
	// it with no forge call.
	optionsJSON, err := json.Marshal(columnOption)
	if err != nil {
		return seedParams{}, nil, fmt.Errorf("project sync: marshal status options: %w", err)
	}
	if _, err := s.store.UpsertGithubProjectLink(ctx, store.UpsertGithubProjectLinkParams{
		RepoID:           repo.ID,
		ProjectNodeID:    projectID,
		ProjectNumber:    projectNumber,
		StatusFieldID:    field.ID,
		StatusOptions:    optionsJSON,
		OwnedByUzi:       ownedByUzi,
		UnmatchedColumns: unmatched,
		DoneOptionID:     doneOptionID, // PRD #584 M1: reserved "Done" projection option (adopt/resync)
	}); err != nil {
		return seedParams{}, nil, fmt.Errorf("project sync: persist link: %w", err)
	}

	return seedParams{
		repo:         repo,
		syncer:       syncer,
		owner:        owner,
		name:         name,
		projectID:    projectID,
		fieldID:      field.ID,
		columnOption: columnOption,
		position:     position,
		doneOptionID: doneOptionID, // PRD #584 M2: seed closed issues to Done on re-seed
	}, unmatched, nil
}

// adoptPrepare does the resolve → prepare-link work for Adopt: it resolves the repo
// slug and the project, then PERSISTS the link (including the unmatched set, PRD #576
// M3) via prepareSeedLink. It supplies prepareSeedLink a BY-NAME field resolver (PRD
// #582): try the built-in "Status" field, falling back to uzi's own field name — the
// original adopt behavior, unchanged, since a first adopt holds no field id yet. It
// does NOT seed items — the caller launches that asynchronously (PRD #576 M4, see
// launchSeed) — so it returns the seedParams for the background step plus a
// human-readable NOTE describing any board columns that did not match a Status option
// (also persisted to unmatched_columns). owned_by_uzi is false.
func (s *ProjectSyncService) adoptPrepare(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, projectNumber int, ownerKind forge.ProjectV2OwnerKind) (seedParams, string, error) {
	owner, name, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
	if err != nil {
		return seedParams{}, "", fmt.Errorf("project sync: resolve repo slug: %w", err)
	}

	project, err := syncer.ResolveProjectV2(ctx, owner, projectNumber, ownerKind)
	if err != nil {
		return seedParams{}, "", fmt.Errorf("project sync: resolve project #%d: %w", projectNumber, err)
	}

	// By-name resolver: the built-in "Status" field, falling back to uzi's own field
	// name (a board uzi created) — identical to adopt's pre-#582 behavior.
	resolveField := func(ctx context.Context) (forge.ProjectV2StatusField, error) {
		field, err := syncer.ProjectV2StatusFieldByName(ctx, project.ID, statusFieldName)
		if err != nil {
			field, err = syncer.ProjectV2StatusFieldByName(ctx, project.ID, uziStatusFieldName)
			if err != nil {
				return forge.ProjectV2StatusField{}, err
			}
		}
		return field, nil
	}

	sp, unmatched, err := s.prepareSeedLink(ctx, repo, syncer, owner, name, project.ID, int64(project.Number), false, resolveField)
	if err != nil {
		return seedParams{}, "", err
	}
	return sp, unmatchedNote(unmatched), nil
}

// Resync re-runs the adopt seed against a repo's ALREADY-linked board (PRD #576 M3),
// re-reading the Status field so newly-added options are picked up — that is the whole
// point of Resync — and re-persisting the link (including a recomputed unmatched set).
// It takes NO owner_kind/project_number from the caller: it reuses the stored
// project_node_id, so a user who added the missing Status options in GitHub can one-
// click reconcile. A repo with no link row returns ErrProjectSyncNotLinked (→ 404).
// Clears/stamps last_error exactly as Adopt does. It is fully synchronous in M3; the
// seed seam is kept separable for M4's async wrapping.
func (s *ProjectSyncService) Resync(ctx context.Context, repoID uuid.UUID) (string, error) {
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrProjectSyncNotLinked
		}
		return "", fmt.Errorf("project sync: resync load link: %w", err)
	}

	repo, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return "", err
	}

	sp, note, err := s.resyncPrepare(ctx, repo, syncer, link)
	if err != nil {
		// Pre-seed failure. Best-effort: stamp on the (already-existing) link row,
		// mirroring Adopt. Kept synchronous — the async path only owns the seed.
		if serr := s.store.SetGithubProjectLinkError(ctx, store.SetGithubProjectLinkErrorParams{
			LastError: pgtype.Text{String: truncateErr(err.Error()), Valid: true},
			RepoID:    repoID,
		}); serr != nil {
			s.log.Warn("project sync: record link error", "repo", repoID, "error", serr)
		}
		return "", err
	}
	// The link is re-persisted; the handler can return 200 with the note now. The
	// unmatched note is a non-fatal advisory, logged not stamped (see Adopt). Seed items
	// in the background (PRD #576 M4); launchSeed's finalize clears/stamps last_error.
	if note != "" {
		s.log.Info("project sync: resync linked with advisory", "repo", repoID, "note", note)
	}
	s.launchSeed(ctx, repoID, sp)
	return note, nil
}

// resyncPrepare is Resync's forge-touching prepare core (split out so Resync owns the
// stamp/launch wrapping, like Adopt/adoptPrepare): resolve the repo slug, then prepare
// (persist) the link against the STORED project coordinates (reusing the stored node id,
// number, and ownership). It supplies prepareSeedLink a BY-ID field resolver keyed on
// the stored link.StatusFieldID (PRD #582), so Resync re-reads the SAME field the link
// already points at — never re-resolving by name, which would silently re-point a
// uzi-Status-synced board at the built-in "Status" field (#582). Re-reading that field
// is what lets newly-added options on it resolve and recomputes the unmatched set
// against its current options. A field deleted on GitHub makes the by-id read return
// not-found, which propagates as the existing pre-seed error (stamped to last_error by
// Resync). It does NOT seed — the caller launches that asynchronously (PRD #576 M4).
func (s *ProjectSyncService) resyncPrepare(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, link store.GithubProjectLink) (seedParams, string, error) {
	owner, name, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
	if err != nil {
		return seedParams{}, "", fmt.Errorf("project sync: resolve repo slug: %w", err)
	}
	// By-id resolver: re-read the exact field the link already points at.
	resolveField := func(ctx context.Context) (forge.ProjectV2StatusField, error) {
		return syncer.ProjectV2StatusFieldByID(ctx, link.ProjectNodeID, link.StatusFieldID)
	}
	sp, unmatched, err := s.prepareSeedLink(ctx, repo, syncer, owner, name, link.ProjectNodeID, link.ProjectNumber, link.OwnedByUzi, resolveField)
	if err != nil {
		return seedParams{}, "", err
	}
	return sp, unmatchedNote(unmatched), nil
}

// AutoCreateColumns is the M6 safe column auto-create (PRD #576): it turns a repo's
// SKIPPED (unmatched) board columns into synced ones WITHOUT any destructive GitHub
// mutation and without ever risking existing item labels. It creates a FRESH uzi-owned
// single-select field (F-E — a new field has no item values, nothing to clear) on the
// EXISTING adopted board and switches the link's status_field_id to it. It never touches
// the destructive full-list option-replace path (F-B replace, D3): that GraphQL mutation
// does not exist in this codebase and is deliberately not built.
//
// For an ORG repo the equivalent is just Provision (uzi's own fresh field), already
// built; this method is the ADOPT (user-repo) path — it makes uzi own the FIELD, not
// the PROJECT, so an adopted board stays owned_by_uzi=false and teardown never deletes
// the user's board.
//
// 🔴 Atomic field switch + marker reset (F-H / R1). A fresh field reads EMPTY for every
// item; if status_field_id is switched while item markers still hold the OLD field's
// option ids, the very next reverse tick sees live("") != marker(old id) for EVERY issue
// and fires the F-F mass-clear cascade (strips every board-column label off the real
// forge issues). So the switch MUST both (a) reset all item markers to "" AND (b) pause
// reverse sync across it (reuse M4's seeding lease) — defense in depth. The ordering
// below closes the reverse race:
//
//  1. MarkGithubProjectLinkSeeding — take the reverse-suppression lease BEFORE the switch
//     is visible (launchSeed re-marks it; the mark is idempotent).
//  2. ResetGithubProjectItemMarkers — old-field markers → NULL, so a reverse tick that
//     somehow ran would read live("") == marker(NULL→"") = no-op, not a cascade.
//  3. UpsertGithubProjectLink — switch status_field_id to the new field; the unmatched
//     set is now empty (every column matched by construction); owned_by_uzi is PRESERVED.
//  4. launchSeed — re-seed every open issue's Status on the NEW field (async), advancing
//     markers; its finalize clears the lease + last_error.
//
// Crash-safety: because markers were reset to NULL and the new field reads "" for every
// item, even a crash mid-reseed converges safely — an unseeded item has
// live("") == marker(NULL→"") = no-op, and the lease ages out (M4, seedSuppressLease).
// No cascade either way.
//
// A repo with no link row returns ErrProjectSyncNotLinked (→ 404). It deliberately does
// NOT reuse prepareSeedLink: this flow has just CREATED the field and holds it in hand,
// so it switches the link to that fresh field directly rather than re-resolving it at
// all — no by-name or by-id read is needed or wanted here.
func (s *ProjectSyncService) AutoCreateColumns(ctx context.Context, repoID uuid.UUID) (string, error) {
	link, err := s.store.GetGithubProjectLinkByRepo(ctx, repoID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrProjectSyncNotLinked
		}
		return "", fmt.Errorf("project sync: autocreate load link: %w", err)
	}

	repo, syncer, err := s.projectSyncPreamble(ctx, repoID)
	if err != nil {
		return "", err
	}

	owner, name, err := syncer.RepoSlug(ctx, repo.ForgeProjectID)
	if err != nil {
		return "", fmt.Errorf("project sync: resolve repo slug: %w", err)
	}

	// Build the new field's options from ALL board columns (mirror provisionPrepare):
	// every column becomes an option, so nothing is skipped after the switch.
	columns, err := s.store.ListBoardColumns(ctx, repo.ID)
	if err != nil {
		return "", fmt.Errorf("project sync: list board columns: %w", err)
	}
	newOptions := make([]forge.ProjectV2NewOption, 0, len(columns)+1)
	for i, c := range columns {
		newOptions = append(newOptions, forge.ProjectV2NewOption{
			Name:  c.LabelName,
			Color: provisionColor(i),
		})
	}
	// Append the reserved "Done" projection option (PRD #584 M1) unless a board column is
	// already named "Done". It rides the FIELD options only — the columnOption loop below
	// iterates the board columns slice, so "Done" never becomes a status option/column here.
	newOptions = appendDoneOption(newOptions, columns)

	// Fresh uzi-owned field on the EXISTING adopted project (link.ProjectNodeID) — NOT a
	// new project. F-E: a fresh field has no item values, so setting its options is safe.
	field, err := syncer.CreateProjectV2Field(ctx, link.ProjectNodeID, uziStatusFieldName, newOptions)
	if err != nil {
		return "", fmt.Errorf("project sync: create status field: %w", err)
	}

	// Build the column→option map + position from the CREATED field's option ids. Every
	// column matches by construction (we created the options from the columns), so the
	// unmatched set is now empty.
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
	// The reserved "Done" option id (PRD #584 M1): the appended option, or a pre-existing
	// "Done" board column's option — resolved by name off the freshly created field.
	doneOptionID := optionByName[doneColumnName]
	optionsJSON, err := json.Marshal(columnOption)
	if err != nil {
		return "", fmt.Errorf("project sync: marshal status options: %w", err)
	}

	// --- Atomic field switch under the seeding lease (F-H / R1), ordering as documented. ---
	// 1. Pause reverse BEFORE the switch is visible. launchSeed re-marks; Mark is idempotent.
	if err := s.store.MarkGithubProjectLinkSeeding(ctx, repoID); err != nil {
		s.log.Warn("project sync: autocreate mark seeding lease", "repo", repoID, "error", err)
	}
	// 2. Old-field markers → NULL, so no item reads as live("") != marker(old id).
	if err := s.store.ResetGithubProjectItemMarkers(ctx, repoID); err != nil {
		return "", fmt.Errorf("project sync: reset item markers: %w", err)
	}
	// 3. Switch status_field_id to the fresh field; unmatched now empty; PRESERVE
	//    owned_by_uzi — auto-create makes uzi own the FIELD, not the adopted PROJECT.
	if _, err := s.store.UpsertGithubProjectLink(ctx, store.UpsertGithubProjectLinkParams{
		RepoID:           repo.ID,
		ProjectNodeID:    link.ProjectNodeID,
		ProjectNumber:    link.ProjectNumber,
		StatusFieldID:    field.ID,
		StatusOptions:    optionsJSON,
		OwnedByUzi:       link.OwnedByUzi,
		UnmatchedColumns: []string{},
		DoneOptionID:     doneOptionID, // PRD #584 M1: reserved "Done" projection option
	}); err != nil {
		return "", fmt.Errorf("project sync: persist link: %w", err)
	}
	// 4. Re-seed every open issue's Status on the NEW field (async); finalize clears the
	//    lease + last_error. Uses the freshly created field directly, not prepareSeedLink.
	s.launchSeed(ctx, repoID, seedParams{
		repo:         repo,
		syncer:       syncer,
		owner:        owner,
		name:         name,
		projectID:    link.ProjectNodeID,
		fieldID:      field.ID,
		columnOption: columnOption,
		position:     position,
		doneOptionID: doneOptionID, // PRD #584 M2: seed closed issues to Done on re-seed
	})

	return fmt.Sprintf("created %d column(s) as a new %q field", len(columns), uziStatusFieldName), nil
}

// seedItems reads the project's live item Statuses once, then for each cached issue
// ensures its project item exists and its Status matches the target derived from the
// label board. An OPEN issue's target is its mapped column option (Open/unmapped →
// cleared); a CLOSED issue's target is the reserved Done option (PRD #584 M2) when the
// link has one — so a manual re-seed projects and KEEPS closed issues on the board,
// mirroring the periodic reconcile path — or is skipped entirely when there is no Done
// option (doneOptionID == "", the pre-M2 D1 behavior).
func (s *ProjectSyncService) seedItems(ctx context.Context, repo store.GetRepoByIDRow, syncer forge.ProjectBoardSyncer, owner, name, projectID, fieldID string, columnOption map[string]string, position map[string]int, doneOptionID string) error {
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
		// Target option, derived per issue-state:
		//   - CLOSED (PRD #584 M2): project to the reserved Done option and KEEP the item
		//     on the board — but only when the link has a Done option; with none there is
		//     nowhere to project it, so skip the issue (the pre-M2 D1 skip behavior).
		//   - OPEN: Open ("" column) → CLEAR (D2: uzi's implicit Open maps to GitHub's
		//     native "No Status"); otherwise the mapped option, if any; a column with no
		//     matching option is skipped (its issues keep their current Status).
		var targetOption string
		if issue.State == "closed" {
			if doneOptionID == "" {
				continue
			}
			targetOption = doneOptionID
		} else {
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
			if column != "" {
				optID, mapped := columnOption[column]
				if !mapped {
					continue
				}
				targetOption = optID
			}
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

	// Seeding-in-progress suppression (PRD #576 M4): while an async Adopt/Resync/Provision
	// seed holds the per-repo lease, skip reverse work — a reverse tick against a
	// partially-seeded board could backfill/mis-move issues the seed has not written yet
	// (F-F / R1). The lease is a bounded window (seedSuppressLease), so a crash mid-seed
	// cannot suppress reverse sync forever: past the lease the poller reconciles.
	if link.SeedingStartedAt.Valid && time.Since(link.SeedingStartedAt.Time) < seedSuppressLease {
		s.log.Info("project sync: reverse skip, seeding in progress", "repo", repoID)
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
	// created since adopt (so they appear on the board and in the diff below), project
	// since-closed issues to Done and KEEP their rows (PRD #584 M2; or prune them when
	// the link has no Done option), and restore since-reopened issues off Done.
	// reconcileItems mutates itemsByIID in place so the diff sees the reconciled set — a
	// freshly-backfilled item carries marker == its seeded value == the live value the
	// diff then reads, so the same-tick diff no-ops it (no oscillation).
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
//   - Close→Done (PRD #584 M2): a CLOSED issue that still has a tracked item row has its
//     Status projected to the reserved Done option and its item row KEPT (not deleted),
//     so the closed issue lands in a dedicated Done status on the board. The write is
//     idempotent (skipped when the marker already reads Done) and best-effort (a set
//     failure keeps the row and retries next tick). When the link has NO Done option
//     (link.DoneOptionID == ""), there is nowhere to project it, so it falls back to the
//     pre-M2 D1 close-prune: DELETE the item row locally with no GitHub call.
//   - Reopen restoration (PRD #584 M2): a since-reopened issue (now open) that is still
//     tracked and whose marker sits on Done has its Status restored to its CURRENT label
//     column (a mapped column → that option; Open/unmapped → cleared). Neither the diff
//     nor Pass 2 (open UNtracked) would touch it — the diff no-ops it (live == marker ==
//     Done) and Pass 2 skips tracked issues — so reconcile restores it explicitly. When
//     the issue legitimately sits in a real "Done" board column (target == Done, the R6
//     case) nothing is written, so it does not thrash.
//
// It mutates itemsByIID in place (adds backfilled rows, removes pruned ones, advances
// Done/restore markers) so the caller's diff reads the reconciled projection. The repo
// slug is resolved lazily — only when at least one issue actually needs backfilling — so
// a steady-state tick with nothing new makes no extra forge call.
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

	// Pass 1 — close→Done (PRD #584 M2), else close-prune. This is slug-INDEPENDENT
	// (it drives the already-stored item node id + field id + project node id, never the
	// repo slug), so it runs first and to completion even when the forge slug cannot be
	// resolved; a backfill slug failure below must never strand a closed issue's row.
	for _, issue := range issues {
		if issue.State != "closed" {
			continue
		}
		iid := issue.ForgeIssueIid
		item, tracked := itemsByIID[iid]
		if !tracked {
			continue
		}
		if link.DoneOptionID == "" {
			// No Done option to project to: fall back to the pre-M2 close-prune — DELETE
			// the item row locally with no GitHub call, and stop tracking the issue.
			if err := s.store.DeleteGithubProjectItem(ctx, store.DeleteGithubProjectItemParams{
				RepoID:        repo.ID,
				ForgeIssueIid: iid,
			}); err != nil {
				s.log.Warn("project sync: reverse prune closed item", "repo", repo.ID, "issue", iid, "error", err)
				continue
			}
			delete(itemsByIID, iid)
			continue
		}
		// Idempotent: the marker already reads Done → nothing to do, KEEP the row.
		if markerValue(item.LastStatusOptionID) == link.DoneOptionID {
			continue
		}
		// Project to Done and KEEP the row. Best-effort: on error DO NOT advance the
		// marker and DO NOT delete (retry next tick), mirroring the prune warn above.
		if err := syncer.SetProjectV2ItemStatus(ctx, link.ProjectNodeID, item.ItemNodeID, link.StatusFieldID, link.DoneOptionID); err != nil {
			s.log.Warn("project sync: reverse set closed issue to Done", "repo", repo.ID, "issue", iid, "error", err)
			continue
		}
		if err := s.store.SetGithubProjectItemStatusMarker(ctx, store.SetGithubProjectItemStatusMarkerParams{
			LastStatusOptionID: optionMarker(link.DoneOptionID),
			RepoID:             repo.ID,
			ForgeIssueIid:      iid,
		}); err != nil {
			s.log.Warn("project sync: reverse advance Done marker", "repo", repo.ID, "issue", iid, "error", err)
		}
		// Advance the in-memory marker so the same-tick reverseDiff reads marker == live
		// (== Done) and no-ops it — the convergence no-op the reverse diff relies on.
		item.LastStatusOptionID = optionMarker(link.DoneOptionID)
		itemsByIID[iid] = item
	}

	// Pass 1b — reopen restoration (PRD #584 M2). A since-reopened issue (now open) that
	// is STILL tracked and whose marker sits on Done is restored to its CURRENT label
	// column's Status. Slug-INDEPENDENT (the item node id is already stored, so no
	// resolve/add), so it runs alongside Pass 1 before the slug-dependent backfill. Pass 2
	// skips tracked issues and the diff no-ops a marker == live == Done item, so without
	// this pass a reopened issue would stay stuck on Done.
	if link.DoneOptionID != "" {
		for _, issue := range issues {
			if issue.State == "closed" {
				continue
			}
			iid := issue.ForgeIssueIid
			item, tracked := itemsByIID[iid]
			if !tracked {
				continue
			}
			if markerValue(item.LastStatusOptionID) != link.DoneOptionID {
				continue // not sitting on Done → nothing to restore
			}
			// Compute the restore target from the issue's CURRENT column (like backfillItem):
			// a mapped column → its option; Open/unmapped → cleared ("").
			var labels []string
			if len(issue.Labels) > 0 {
				if err := json.Unmarshal(issue.Labels, &labels); err != nil {
					labels = nil
				}
			}
			column, _, _ := board.ResolveColumn(labels, issue.State, position)
			target := ""
			if column != "" {
				if optID, mapped := columnOption[column]; mapped {
					target = optID
				}
			}
			if target == link.DoneOptionID {
				// R6: a real "Done" board column the issue currently sits in — the item
				// legitimately stays on Done. Do NOT write (no thrash).
				continue
			}
			// Restore Status off Done. Best-effort: on error DO NOT advance (retry next tick).
			if err := syncer.SetProjectV2ItemStatus(ctx, link.ProjectNodeID, item.ItemNodeID, link.StatusFieldID, target); err != nil {
				s.log.Warn("project sync: reverse restore reopened issue status", "repo", repo.ID, "issue", iid, "error", err)
				continue
			}
			if err := s.store.SetGithubProjectItemStatusMarker(ctx, store.SetGithubProjectItemStatusMarkerParams{
				LastStatusOptionID: optionMarker(target),
				RepoID:             repo.ID,
				ForgeIssueIid:      iid,
			}); err != nil {
				s.log.Warn("project sync: reverse advance restore marker", "repo", repo.ID, "issue", iid, "error", err)
			}
			// Advance the in-memory marker so the same-tick reverseDiff reads marker == live
			// (== target) and no-ops it.
			item.LastStatusOptionID = optionMarker(target)
			itemsByIID[iid] = item
		}
	}

	// Pass 2 — backfill open untracked issues. Needs the repo slug (resolved lazily,
	// once). A slug failure is a per-repo condition: no issue can be backfilled, so
	// stamp and stop this pass — Pass 1 (close→Done or close-prune) and Pass 1b (reopen
	// restore) above already completed.
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

// plannedMove is one intended reverse AutoMove for the tick, computed in reverseDiff's
// PLAN pass (no AutoMove, no store writes) so the whole tick's destructive-write count
// is known BEFORE any AutoMove fires. destructive marks a move that would strip an
// EXISTING column label off the real forge issue — either a clear (targetColumn ""
// while the issue currently sits in a column, the F-F empty cascade) or a remap to a
// different valid column (R1b). This is the corruption shape M5's cap bounds; a naive
// running counter would strip the first N issues before tripping (partial corruption),
// which is why the plan is materialized in full first.
type plannedMove struct {
	issue        store.Issue
	targetColumn string
	itemPresent  bool
	itemID       string
	liveOptionID string
	destructive  bool
}

// reverseDiff is the M6 diff pass (extracted from ReverseSync in M7), restructured for
// PRD #576 M5 into count-then-decide-then-execute so a single reverse tick cannot mass-
// strip real forge issue labels (F-F / R1 / R1b, the standing PRD #364 data-loss bug):
//
//   - PASS 1 (PLAN) applies the existing skip/no-op logic unchanged and records every
//     item that survives to an intended AutoMove, classifying each as destructive.
//   - DECIDE: if the destructive count both exceeds reverseCapK AND is more than
//     reverseCapPct percent of the tracked items, the whole tick is aborted — execute
//     none (not even non-destructive adds: a corrupted tick is untrustworthy wholesale),
//     advance no markers, stamp last_error, and log loudly.
//   - PASS 2 (EXECUTE) runs the existing per-item AutoMove + marker-advance verbatim.
//
// A single genuine user drag (destructive count 1) always passes both gates. See
// ReverseSync's doc for the convergence invariant the marker advance preserves.
func (s *ProjectSyncService) reverseDiff(ctx context.Context, repo store.GetRepoByIDRow, f forge.Forge, live []forge.ProjectV2ItemStatus, optionColumn map[string]string, issuesByIID map[int64]store.Issue, itemsByIID map[int64]store.GithubProjectItem, columns []store.BoardColumn) {
	repoID := repo.ID

	// Column-name → position map, so board.ResolveColumn can report each issue's CURRENT
	// column for the destructive classification (identical construction to seedItems).
	position := make(map[string]int, len(columns))
	for _, c := range columns {
		position[c.LabelName] = int(c.Position)
	}

	// Pass 1 — PLAN. No AutoMove, no store writes: just compute the full set of intended
	// moves so the tick's total destructive-write count is known before any executes.
	var plan []plannedMove
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
		// either projected it to Done and KEPT its row (PRD #584 M2) or, when the link
		// has no Done option, pruned it — either way reverse must not drive a label from
		// a closed issue's card.
		issue, ok := issuesByIID[it.IssueNumber]
		if !ok {
			s.log.Info("project sync: reverse skip, issue not in cache", "repo", repoID, "issue", it.IssueNumber)
			continue
		}
		if issue.State == "closed" {
			continue
		}

		// Classify destructive: resolve the issue's CURRENT column from its labels and
		// state (exactly as seedItems unmarshals + resolves). The move strips an existing
		// column label iff the issue currently sits in a non-empty column that differs
		// from the target — a clear (target "") or a remap to a different valid column.
		// A move that only ADDS a label (currentColumn "" → some column) or is already in
		// the target strips nothing → NOT destructive.
		var labels []string
		if len(issue.Labels) > 0 {
			if err := json.Unmarshal(issue.Labels, &labels); err != nil {
				labels = nil
			}
		}
		currentColumn, _, _ := board.ResolveColumn(labels, issue.State, position)
		destructive := currentColumn != "" && currentColumn != targetColumn

		plan = append(plan, plannedMove{
			issue:        issue,
			targetColumn: targetColumn,
			itemPresent:  itemPresent,
			itemID:       it.ItemID,
			liveOptionID: it.OptionID,
			destructive:  destructive,
		})
	}

	// DECIDE. Count the planned destructive moves and compare against the per-tick cap.
	// trackedItems is the count of items uzi has markers for; when it is 0 the percentage
	// side reduces to "any destructive move" (destructiveCount*100 > 0), so the reverseCapK
	// gate alone governs — a mass destructive backfill on an untracked board still trips.
	destructiveCount := 0
	for _, p := range plan {
		if p.destructive {
			destructiveCount++
		}
	}
	trackedItems := len(itemsByIID)
	if destructiveCount > s.reverseCapK && destructiveCount*100 > s.reverseCapPct*trackedItems {
		// Cap tripped: a corrupted tick is untrustworthy wholesale, so execute NONE of
		// the planned moves (not even non-destructive adds), advance NO markers, stamp
		// last_error, and log LOUDLY with the count.
		err := fmt.Errorf("project sync: reverse tick aborted: %d destructive moves exceed cap (k=%d, pct=%d, tracked=%d)",
			destructiveCount, s.reverseCapK, s.reverseCapPct, trackedItems)
		s.log.Error("project sync: reverse tick aborted, destructive-write cap tripped",
			"repo", repoID, "destructive", destructiveCount, "capK", s.reverseCapK, "capPct", s.reverseCapPct, "tracked", trackedItems)
		s.stampLinkErrorReverse(ctx, repoID, err)
		return
	}

	// Pass 2 — EXECUTE. The existing per-item logic, verbatim: AutoMove, then advance the
	// marker on success (or stamp + continue without advancing on error).
	for _, p := range plan {
		issue := p.issue

		// Write the label via the ordinary AutoMove path. On error, DO NOT advance the
		// marker (so the change is retried next tick) and stamp the link error.
		if _, err := s.mover.AutoMove(ctx, f, repo.ForgeProjectID, issue, columns, p.targetColumn); err != nil {
			s.stampLinkErrorReverse(ctx, repoID, err)
			continue
		}

		// Advance the marker to the live OptionID so the next poll reads live == marker
		// → no-op (the second convergence guard). An existing item row advances just
		// the marker; an absent one is upserted, carrying the live ItemID as the node id.
		if p.itemPresent {
			if err := s.store.SetGithubProjectItemStatusMarker(ctx, store.SetGithubProjectItemStatusMarkerParams{
				LastStatusOptionID: optionMarker(p.liveOptionID),
				RepoID:             repoID,
				ForgeIssueIid:      issue.ForgeIssueIid,
			}); err != nil {
				s.log.Warn("project sync: reverse advance marker", "repo", repoID, "issue", issue.ForgeIssueIid, "error", err)
			}
		} else {
			if _, err := s.store.UpsertGithubProjectItem(ctx, store.UpsertGithubProjectItemParams{
				RepoID:             repoID,
				ForgeIssueIid:      issue.ForgeIssueIid,
				ItemNodeID:         p.itemID,
				LastStatusOptionID: optionMarker(p.liveOptionID),
			}); err != nil {
				s.log.Warn("project sync: reverse persist item", "repo", repoID, "issue", issue.ForgeIssueIid, "error", err)
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
	// UnmatchedColumns are the board columns that had no matching Status option at the
	// last adopt/resync (PRD #576 M3). Read straight off the link row (SELECT *), so
	// this stays a pure store read with no forge call (D5). Empty when every column
	// matched.
	UnmatchedColumns []string
	// NoDoneOption is true when the linked Status field has no reserved "Done" option
	// (link.done_option_id == ""), so a CLOSED issue cannot be projected to Done (PRD
	// #584 M4). The panel surfaces an advisory: add a "Done" option and Resync, or
	// re-provision. False for an adopted built-in Status (which ships a Done option) and
	// for any uzi-created field (which appends one). A pure store read — no forge call.
	NoDoneOption bool
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
		ProjectNumber:    link.ProjectNumber,
		OwnedByUzi:       link.OwnedByUzi,
		LastSyncedAt:     link.LastSyncedAt,
		LastError:        link.LastError,
		ItemCount:        len(items),
		UnmatchedColumns: link.UnmatchedColumns,
		NoDoneOption:     link.DoneOptionID == "",
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

// unmatchedNote renders the human-readable advisory RETURNED to the caller for board
// columns that had no matching Status option. The columns themselves are persisted to
// the link's unmatched_columns (PRD #576 M3, in prepareSeedLink); this is the string
// form of the same set. Empty when every column matched.
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
