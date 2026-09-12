package workersvc

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// AdminMarkDone fans a "done" verdict out to EVERY user's OPEN member of each requested
// (category, target) coordinate, in ONE upsert, stamped set_via='admin' and
// set_by_user_id=adminUserID (PRD #1184 M2, decision log 2026-09-07). It is the cross-user twin
// of BulkSetDispositions, and its whole trust story is in two places the owner path does not
// have: the resolve has NO user predicate (ListRecommendationsForCoordsAll reaches every owner),
// and the write is ON CONFLICT DO NOTHING so a human's existing verdict — done OR dismissed — is
// NEVER overwritten.
//
// It is reachable only through AdminSetJudgeDisposition, which sits in the cookie-only admin
// WRITE group (RequireAuth + RequireAdmin); a uza_ admin_ro Bearer 401s before the handler runs,
// so there is no in-service ownership branch and IsAdmin is never re-checked here.
//
// Scope is fixed to OPEN: only members the shared BucketOf ladder buckets as `todo` are written,
// so a filed member and an already-disposed member (human or system) are both skipped. There is
// no ScopeAll for the admin path — an admin never re-asserts over a settled verdict. The write
// arrays are the RESOLVED columns (review_id/category/target/rationale_hash), never the request
// body, keeping the 00071/00073 no-category-CHECK invariant. On error this returns the zero DTO
// and NOTHING was written (the single statement cannot half-apply).
func (s *Service) AdminMarkDone(ctx context.Context, adminUserID uuid.UUID, items []JudgeDispositionCoord) (apitypes.JudgeAdminDispositionResultDTO, error) {
	coords := dedupeCoords(items)
	if len(coords) > JudgeDispositionMaxItems {
		return apitypes.JudgeAdminDispositionResultDTO{}, ErrTooManyItems
	}
	categories := make([]string, 0, len(coords))
	targets := make([]string, 0, len(coords))
	for _, c := range coords {
		categories = append(categories, c.Category)
		targets = append(targets, c.Target)
	}

	members, err := s.q.ListRecommendationsForCoordsAll(ctx, store.ListRecommendationsForCoordsAllParams{
		Categories: categories,
		Targets:    targets,
	})
	if err != nil {
		return apitypes.JudgeAdminDispositionResultDTO{}, err
	}

	// Only OPEN members: a filed or already-settled member keeps its state (PRD #98 Decision 2's
	// definition of open, the SAME ladder scope=open uses). This Go filter is what keeps the common
	// case from even attempting to touch a settled row; the write below is the durable backstop for a
	// coordinate leaving `todo` between this read and the write, and it takes THREE mechanisms
	// together — never the NOT EXISTS alone (issue #1184 rework):
	//   * ON CONFLICT (review_id, category, target) DO NOTHING catches a concurrent HUMAN disposition
	//     landing. That is a unique-index re-check on the disposition table itself, so it needs no
	//     lock: the loser's INSERT simply conflicts on the coordinate key and is dropped.
	//   * the WHERE NOT EXISTS filed recheck PLUS the per-coordinate advisory lock (below) catches a
	//     concurrent FILING. Filing writes NO disposition row (SettleRecommendationFiledIssue only
	//     stamps recommendation_filed_issues.filed_at, a DIFFERENT table), so there is no unique
	//     conflict for DO NOTHING to catch — the NOT EXISTS is the only guard, and on its own it is
	//     NOT atomic: under the pool's default READ COMMITTED (OpenPool sets none) the NOT EXISTS
	//     reads the filed table at the INSERT statement's snapshot, so a filing committing just after
	//     that snapshot is missed, leaving both a settled filed row and an admin 'done' (BucketOf
	//     renders that as 'done', masking the filing). store.LockJudgeCoord — taken as the tx's first
	//     statement here AND by the filing settle (settleFiledIssue) — serializes the two on the
	//     coordinate: the loser blocks until the winner commits and, under READ COMMITTED, its NEXT
	//     statement takes a fresh snapshot that sees the winner's committed row. This depends on READ
	//     COMMITTED (see migrate.go's lock-class docs): at REPEATABLE READ or SERIALIZABLE the tx
	//     snapshot would be fixed at the lock statement — before the winner commits — and the race
	//     would reopen.
	// Residual, deliberately not serialized (the filed==settled design; tracked in a follow-up): a
	// CLAIMED-but-not-yet-SETTLED filing (filed_at still NULL, in the claim→forge→settle window) is
	// not covered, and an admin 'done' on a genuinely-`todo` coordinate that is filed AFTERWARDS is
	// not covered either. Every value below comes off the RESOLVED row.
	reviewIDs := make([]uuid.UUID, 0, len(members))
	writeCategories := make([]string, 0, len(members))
	writeTargets := make([]string, 0, len(members))
	hashes := make([]string, 0, len(members))
	for _, rec := range members {
		if BucketOf(rec.DispositionStatus.String, rec.FiledSettled) != BucketTodo {
			continue
		}
		reviewIDs = append(reviewIDs, rec.ReviewID)
		writeCategories = append(writeCategories, rec.Category)
		writeTargets = append(writeTargets, rec.Target)
		hashes = append(hashes, RationaleHash(rec.RationaleMd))
	}

	updated := int64(0)
	if len(reviewIDs) > 0 {
		// Fail-closed if the tx beginner was never wired (mirror completeRunWithPermit): the write
		// needs to take the per-coordinate advisory lock as the first statement of a real
		// transaction, so it can never run non-atomically. This is INSIDE the write block, so the
		// too-many-items and empty-resolve paths never reach it.
		if s.txBeginner == nil {
			return apitypes.JudgeAdminDispositionResultDTO{}, fmt.Errorf("admin mark-done transaction unavailable: no tx beginner wired")
		}
		tx, err := s.txBeginner.Begin(ctx)
		if err != nil {
			return apitypes.JudgeAdminDispositionResultDTO{}, err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		qtx := store.New(tx)

		// Acquire the per-coordinate advisory locks BEFORE the write, sorted ascending by the NUMERIC
		// objid (tie-break Category then Target for determinism). Sorting on the objid — the value the
		// lock actually keys on, not the tuple — is what keeps two concurrent admin writes deadlock-free
		// even when two coordinates collide on the hash. Sort a COPY; the write arrays keep their
		// resolve order.
		lockCoords := make([]JudgeDispositionCoord, len(coords))
		copy(lockCoords, coords)
		sort.Slice(lockCoords, func(i, j int) bool {
			oi := store.JudgeCoordLockObjID(lockCoords[i].Category, lockCoords[i].Target)
			oj := store.JudgeCoordLockObjID(lockCoords[j].Category, lockCoords[j].Target)
			if oi != oj {
				return oi < oj
			}
			if lockCoords[i].Category != lockCoords[j].Category {
				return lockCoords[i].Category < lockCoords[j].Category
			}
			return lockCoords[i].Target < lockCoords[j].Target
		})
		for _, c := range lockCoords {
			if err := store.LockJudgeCoord(ctx, tx, c.Category, c.Target); err != nil {
				return apitypes.JudgeAdminDispositionResultDTO{}, err
			}
		}

		n, err := qtx.UpsertAdminDispositionsForResolvedCoords(ctx, store.UpsertAdminDispositionsForResolvedCoordsParams{
			AdminUserID:     pgconv.UUID(adminUserID),
			ReviewIds:       reviewIDs,
			Categories:      writeCategories,
			Targets:         writeTargets,
			RationaleHashes: hashes,
		})
		if err != nil {
			return apitypes.JudgeAdminDispositionResultDTO{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return apitypes.JudgeAdminDispositionResultDTO{}, err
		}
		updated = n
	}

	return s.adminDispositionResult(ctx, int(updated), coords)
}

// AdminUndoDone removes the admin fan-out on each requested coordinate — ONLY the set_via='admin'
// rows, across every user — leaving any human done/dismissed and any system-provenance row intact
// (PRD #1184 M2). It is coordinate-scoped by design: an admin who marked done across users undoes
// across users. It needs no resolve and no rationale hash, just the coordinates.
func (s *Service) AdminUndoDone(ctx context.Context, items []JudgeDispositionCoord) (apitypes.JudgeAdminDispositionResultDTO, error) {
	coords := dedupeCoords(items)
	if len(coords) > JudgeDispositionMaxItems {
		return apitypes.JudgeAdminDispositionResultDTO{}, ErrTooManyItems
	}
	categories := make([]string, 0, len(coords))
	targets := make([]string, 0, len(coords))
	for _, c := range coords {
		categories = append(categories, c.Category)
		targets = append(targets, c.Target)
	}

	n, err := s.q.DeleteAdminDispositionsForCoords(ctx, store.DeleteAdminDispositionsForCoordsParams{
		Categories: categories,
		Targets:    targets,
	})
	if err != nil {
		return apitypes.JudgeAdminDispositionResultDTO{}, err
	}
	return s.adminDispositionResult(ctx, int(n), coords)
}

// adminDispositionResult re-reads the all-users aggregate after a write/undo and narrows the
// groups to the coordinates the call acted on, matching the owner path's post-write re-read
// (BulkSetDispositions → groupsForCoords). It re-reads at bucket=all / categories=nil so a group
// that just left To triage still comes back at its new rollup, and so a coordinate is returned
// whatever its label. Triage and Truncated are the aggregate's canonical values, carried through
// untouched. The narrowing keeps the write response focused on the acted-on rows rather than the
// whole all-users backlog.
func (s *Service) adminDispositionResult(ctx context.Context, updated int, coords []JudgeDispositionCoord) (apitypes.JudgeAdminDispositionResultDTO, error) {
	backlog, err := s.AdminJudgeRecommendationBacklog(ctx, BucketAll, nil)
	if err != nil {
		return apitypes.JudgeAdminDispositionResultDTO{}, err
	}
	return apitypes.JudgeAdminDispositionResultDTO{
		Updated:   updated,
		Groups:    adminGroupsForCoords(backlog.Groups, coords),
		Truncated: backlog.Truncated,
		Triage:    backlog.Triage,
	}, nil
}

// adminGroupsForCoords narrows the freshly-read all-users backlog to the coordinates the caller
// acted on — the admin twin of groupsForCoords. A requested coordinate that resolved to nothing
// simply has no group, the same silence as one that was never sent.
func adminGroupsForCoords(groups []apitypes.JudgeAdminGroupDTO, coords []JudgeDispositionCoord) []apitypes.JudgeAdminGroupDTO {
	want := make(map[JudgeDispositionCoord]bool, len(coords))
	for _, c := range coords {
		want[c] = true
	}
	out := make([]apitypes.JudgeAdminGroupDTO, 0, len(coords))
	for _, g := range groups {
		if want[JudgeDispositionCoord{Category: g.Category, Target: g.Target}] {
			out = append(out, g)
		}
	}
	return out
}
