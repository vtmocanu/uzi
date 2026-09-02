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
	"errors"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

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
