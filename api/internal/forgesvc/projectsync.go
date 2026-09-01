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

// projectSyncResolve runs the preconditions that need NO live forge round-trip: the
// instance kill-switch, repo lookup (unknown id → a bare pgx.ErrNoRows the handler maps
// to 404), the GitHub-only check, the forge build, and the ProjectBoardSyncer capability
// assertion. It returns the resolved repo row, the type-asserted syncer, and the
// underlying forge.Forge (so a caller that still wants the scope preflight can run it).
// The CLEAR sentinels it returns (ErrProjectSyncDisabled / NotGitHub / Unsupported, plus
// the bare pgx.ErrNoRows) are exactly what the admin handler maps to a 4xx.
//
// projectSyncPreamble layers the live scope preflight (ensureProjectScope → TokenInfo)
// on top; the pure read path (GetVisibility) calls projectSyncResolve directly to skip
// that extra introspection round-trip on the common panel-open path (issue #569 finding
// #2) — a scope-missing token fails the visibility read itself.
func (s *ProjectSyncService) projectSyncResolve(ctx context.Context, repoID uuid.UUID) (store.GetRepoByIDRow, forge.ProjectBoardSyncer, forge.Forge, error) {
	enabled, err := s.settings.GithubProjectSyncEnabled(ctx)
	if err != nil {
		return store.GetRepoByIDRow{}, nil, nil, fmt.Errorf("project sync: read kill-switch: %w", err)
	}
	if !enabled {
		return store.GetRepoByIDRow{}, nil, nil, ErrProjectSyncDisabled
	}

	repo, err := s.store.GetRepoByID(ctx, repoID)
	if err != nil {
		// pgx.ErrNoRows for an unknown id — the handler maps it to 404.
		return store.GetRepoByIDRow{}, nil, nil, err
	}
	if repo.ForgeType != string(forge.TypeGitHub) {
		return store.GetRepoByIDRow{}, nil, nil, ErrProjectSyncNotGitHub
	}

	f, err := s.forges.ForgeForConnection(repo.ForgeType, repo.BaseUrl, repo.TokenCiphertext)
	if err != nil {
		return store.GetRepoByIDRow{}, nil, nil, fmt.Errorf("project sync: build forge: %w", err)
	}
	syncer, ok := f.(forge.ProjectBoardSyncer)
	if !ok {
		return store.GetRepoByIDRow{}, nil, nil, ErrProjectSyncUnsupported
	}
	return repo, syncer, f, nil
}

// projectSyncPreamble runs the preconditions the write paths (Adopt, Provision,
// SetVisibility, ShareWithUser, Unshare) and the RepoOwnerType read share: everything
// projectSyncResolve checks, plus the live scope preflight (ensureProjectScope, which
// adds MissingScope to the sentinel set). The check order is exactly what the admin
// handler maps to a 4xx — keeping the entry points DRY without changing any observable
// behavior.
func (s *ProjectSyncService) projectSyncPreamble(ctx context.Context, repoID uuid.UUID) (store.GetRepoByIDRow, forge.ProjectBoardSyncer, error) {
	repo, syncer, f, err := s.projectSyncResolve(ctx, repoID)
	if err != nil {
		return store.GetRepoByIDRow{}, nil, err
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
