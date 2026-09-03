package forgesvc

// projectsync_sync.go holds the GitHub Projects v2 forward + reverse board sync
// (PRD #364): ForwardMove pushes a single issue's column to the board; ReverseSync
// reconciles the board back into the label cache. Includes the sync-only stamp and
// marker helpers.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

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
//
// targetColumn "closed" is the issue-state sentinel the board handler sends on a
// drag-to-Closed (PRD #1034 M3): it is NOT a board column, and it drives the board's
// reserved Done option (link.DoneOptionID) so a closed card lands in Done on the
// linked Projects v2 board. A board with no Done option logs and no-ops (Status left
// untouched), mirroring the unmapped-column no-op. Reopen rides the ordinary
// column/"" paths — the handler passes the drop-target column (or "" for Backlog),
// never "closed".
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
	// Status untouched. The "closed" sentinel → the board's reserved Done option.
	var targetOption string
	switch {
	case targetColumn == "closed":
		// Drag-to-Closed (PRD #1034 M3): drive the board's reserved Done option. No
		// board column is literally "closed" (it is the issue-state sentinel the board
		// handler sends), so this never shadows a real column mapping. A board with no
		// Done option has nowhere to project: log and no-op like an unmapped column.
		if link.DoneOptionID == "" {
			s.log.Info("project sync: forward move to closed skipped, board has no Done option",
				"repo", repoID, "issue", issueIID)
			return nil
		}
		targetOption = link.DoneOptionID
	case targetColumn != "":
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
	default:
		// targetColumn "" → clear (No Status); targetOption stays "".
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
