package forgesvc

// projectsync_provision.go holds GitHub Projects v2 board provisioning, adoption,
// seeding and resync (PRD #364): Adopt/Provision/Resync/Disable, the seed pipeline
// (provisionPrepare/seed/launchSeed/prepareSeedLink/seedItems), AutoCreateColumns,
// and the provision-only helpers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

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
