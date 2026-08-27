package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// JudgeDispositionMaxItems caps the coordinates one bulk call may carry (PRD #98 M2).
//
// The request is now ONE resolve and ONE upsert whatever the member count (audit NB-A), so
// this no longer bounds round-trips — it bounds the SIZE of the two statements: the
// coordinate arrays the resolve unnests, and the member arrays the write unnests. That is
// still worth bounding explicitly on a no-CSRF RequireUser token path rather than leaving it
// to the 1 MiB body limit, because members-per-coordinate is itself unbounded (one
// coordinate matches every occurrence across all the caller's reviews).
//
// Comfortably above any real multi-select: the UI batches a screenful of groups, not a
// backlog. Over the cap is a 400, not a silent truncation.
const JudgeDispositionMaxItems = 100

// ErrTooManyItems — the bulk call carried more coordinates than JudgeDispositionMaxItems.
// The handler maps it to 400. Distinct from a resolve miss, which is not an error at all.
var ErrTooManyItems = errors.New("too many items")

// ErrInvalidScope — the scope was not one of the accepted values. The handler validates
// scope before calling, so reaching this means a caller bypassed that gate; the service
// refuses rather than falling through to the destructive ScopeAll semantics.
var ErrInvalidScope = errors.New("invalid scope")

// Scope values. Kept as named constants so the service's own switch and the validator
// cannot drift apart.
const (
	// ScopeOpen touches only members the shared ladder buckets as todo — a FILED member is
	// left filed (PRD #98 Decision 2's definition of open).
	ScopeOpen = "open"
	// ScopeAll re-asserts across every member of the coordinate, INCLUDING ones that
	// already carry a settled verdict.
	ScopeAll = "all"
)

// judgeDispositionScopes backs ValidJudgeDispositionScope.
//
// Unexported for the same reason as judgeBacklogBuckets: an exported package-level map is
// mutable from anywhere in the binary, so a validator built on one can be widened out of
// sight of the handler that relies on it. Here that would be a security-relevant widening,
// since the scope is what keeps a settled member's verdict from being overwritten.
//
// Note the asymmetry with `bucket`, which fails CLOSED downstream (an unknown bucket
// matches no group in filterGroups). Scope has no such natural safe default: the service
// selects members, so anything that is not recognised as `open` would historically have
// fallen through to `all` semantics — i.e. an unknown scope would RE-ASSERT over settled
// verdicts, the most destructive of the two. BulkSetDispositions now rejects an
// unrecognised scope outright rather than defaulting, so the handler's validation is no
// longer the only thing standing between a typo and an overwrite; this comment stays to
// explain why BOTH layers exist.
var judgeDispositionScopes = map[string]bool{ScopeOpen: true, ScopeAll: true}

// ValidJudgeDispositionScope reports whether s is an accepted scope. Anything else is a
// 400 — never a silent fallback to a scope the caller did not ask for.
func ValidJudgeDispositionScope(s string) bool { return judgeDispositionScopes[s] }

// memberKey is a RESOLVED member's identity: the (review_id, category, target) triple that
// recommendation_dispositions is actually keyed on. Distinct from coord, which is the
// display grain — see the dedup in BulkSetDispositions for why conflating them would break
// the cross-run fan-out.
type memberKey struct {
	reviewID uuid.UUID
	category string
	target   string
}

// JudgeDispositionCoord is one requested (category, target) coordinate — see
// apitypes.JudgeDispositionCoordDTO for the full contract.
//
// It is an ALIAS, not a copy (PRD #98 M7): the CLI has to encode this body and cannot
// import workersvc (that would drag pgx into the CLI binary), so the struct itself moved to
// the shared apitypes leaf. An alias keeps every existing workersvc.JudgeDispositionCoord
// reference — including its use as a map key in dedupeCoords/groupsForCoords — compiling
// unchanged, while leaving exactly one set of JSON tags for both sides of the wire.
type JudgeDispositionCoord = apitypes.JudgeDispositionCoordDTO

// BulkSetDispositions fans a group disposition out to its member coordinates (PRD #98
// Decision 3) — the Judge menu's "Dismiss" / "Mark done" on one group, and the
// multi-select bar's action across several, in one round-trip.
//
// OWNER-ONLY BY CONSTRUCTION. Every member comes from ListOwnedRecommendationsForCoords,
// whose WHERE is `rv.user_id = @user_id`. There is no ownership branch in this function
// and IsAdmin is never consulted, so a uza_ admin_ro token — which keeps IsAdmin on
// RequireUser — fans out over its OWN rows and resolves nothing on anyone else's. A
// coordinate that is absent and one that is another user's are indistinguishable in the
// response: both contribute zero members (#94 Decision 5).
//
// THE COORDINATE WRITTEN IS THE RESOLVED ONE. `rec.Category` / `rec.Target` below come
// from review_recommendations, never from the request body. That is load-bearing, not
// stylistic: migrations 00071 and 00073 both omit a category CHECK *on purpose*, on the
// stated grounds that "the handler never accepts a category from the request body — it
// reads it off the resolved recommendation". This endpoint is the first place a category
// arrives from a body, so echoing it back into the upsert would let a uzc_ token write
// arbitrary text into a table with no CHECK to catch it.
//
// The write is ONE multi-row statement, not a loop. The item cap bounds COORDINATES, but a
// single coordinate matches every occurrence across all the caller's reviews and the
// resolve has no LIMIT — so a loop turned ≤100 coordinates into an unbounded number of
// sequential round-trips. One statement also means the write cannot half-apply: it either
// commits every member or none, so there is no partial state for the response to
// misrepresent. On error this returns the zero DTO and the error (the handler answers 500),
// and NOTHING was written. The rationale_hash is re-stamped from each member's CURRENT
// rationale (#94 Decision 3) and the upsert keeps #94's last-writer-wins semantics, so a
// double-click converges rather than duplicating.
func (s *Service) BulkSetDispositions(ctx context.Context, ownerUserID uuid.UUID, items []JudgeDispositionCoord, status, reason, scope string) (apitypes.JudgeDispositionResultDTO, error) {
	// Fail CLOSED on an unrecognised scope. The handler validates this too, but scope is
	// what protects a settled member's verdict from being re-asserted, and a plain
	// `scope == ScopeOpen` comparison would silently treat anything unknown as ScopeAll —
	// the destructive rung. Neither layer may be the only one holding (see
	// judgeDispositionScopes).
	switch scope {
	case ScopeOpen, ScopeAll:
	default:
		return apitypes.JudgeDispositionResultDTO{}, ErrInvalidScope
	}
	coords := dedupeCoords(items)
	if len(coords) > JudgeDispositionMaxItems {
		return apitypes.JudgeDispositionResultDTO{}, ErrTooManyItems
	}
	categories := make([]string, 0, len(coords))
	targets := make([]string, 0, len(coords))
	for _, c := range coords {
		categories = append(categories, c.Category)
		targets = append(targets, c.Target)
	}

	members, err := s.q.ListOwnedRecommendationsForCoords(ctx, store.ListOwnedRecommendationsForCoordsParams{
		UserID:     ownerUserID,
		Categories: categories,
		Targets:    targets,
	})
	if err != nil {
		return apitypes.JudgeDispositionResultDTO{}, err
	}

	// Select the members to write, then send them as four ordinal-aligned arrays. Every
	// value here comes off the RESOLVED row — never the request body — which is what keeps
	// the 00071/00073 no-category-CHECK invariant true (see the query's comment for what
	// actually enforces it).
	// Fresh slices, deliberately not reusing the request-side ones above: those hold the
	// CALLER's spellings, and quietly recycling their backing arrays here is exactly the
	// kind of aliasing that turns "we only write resolved values" into a lie after a later
	// edit.
	reviewIDs := make([]uuid.UUID, 0, len(members))
	writeCategories := make([]string, 0, len(members))
	writeTargets := make([]string, 0, len(members))
	hashes := make([]string, 0, len(members))
	// The undo addresses for exactly the members this call settles (PRD #98 review
	// BLK-UNDO). Built in THIS loop rather than by the caller, because this loop IS the
	// scope decision: it runs against rows read moments ago, and a client's "which members
	// are open" is necessarily older than that. Appended in lockstep with the write arrays
	// below, so a member cannot be reported settled without being written.
	settled := make([]apitypes.JudgeSettledMemberDTO, 0, len(members))

	// SECOND layer against duplicate (review_id, category, target) triples. A single
	// multi-row ON CONFLICT DO UPDATE raises SQLSTATE 21000 ("cannot affect row a second
	// time") if the same triple appears twice — a hard 500, not a degradation — and the
	// input that triggers it (a judge emitting one coordinate twice in one review) is legal
	// and data-dependent.
	//
	// UNREACHABLE ON THE LIVE PATH, BY CONSTRUCTION. The resolve is SELECT DISTINCT ON
	// (rv.id, rr.category, rr.target), so it cannot hand this loop a duplicate triple and
	// `seen` can never fire against a real database. Only the fake-backed tests exercise it.
	// That is deliberate, not dead code to delete: a fake cannot model SQL, so a fake-backed
	// duplicate test is theatre unless the guard also exists here — and this earns its place
	// the day someone relaxes the DISTINCT ON or adds a second caller of the write.
	//
	// AND IT ADDED A NEW WAY TO BE WRONG, which is the honest price of the layering rule.
	// Divergence between the two layers is ASYMMETRIC, and the SQL side splits in two —
	// "a wrong SQL key" is not one outcome (measured, PRD #98 review AL, not reasoned):
	//   * a REMOVED SQL guard (delete the DISTINCT ON entirely) is fully MASKED here: this
	//     pass still dedupes on the conflict key, so the write is CORRECT, not merely legal —
	//     the whole live-DB suite stays green and the Go layer is silently doing the work.
	//   * a NARROWED SQL key (DISTINCT ON (rr.category, rr.target), dropping rv.id) is NOT
	//     masked at all: SQL drops rows this pass never sees, and Go cannot restore what SQL
	//     already discarded, so three live-DB tests fail loudly (updated = 1 want 3; scope;
	//     idempotency). Only "the statement stays legal" is true of BOTH.
	//   * a wrong key HERE is NOT masked by SQL either. This pass runs downstream and can
	//     REMOVE members the query correctly kept — silently under-disposing, with no error
	//     and no crash, just some runs left unsettled.
	// So this is the layer that must carry the test, and it does
	// (TestBulkDispositionDedupesMembersWithinAReview, including the negative control that
	// a pair-shaped key would collapse the cross-run fan-out). "Add a second layer" is not
	// automatically free.
	// NOTE the key is the full TRIPLE, not `coord`. Members legitimately repeat a
	// (category, target) across DIFFERENT reviews — that recurrence is the entire point of
	// the cross-run fan-out — so keying on the pair would collapse the fan-out to a single
	// run and silently under-dispose every group. Only a repeat of the same coordinate
	// WITHIN one review is the duplicate that breaks the write.
	seen := make(map[memberKey]bool, len(members))
	for _, rec := range members {
		key := memberKey{reviewID: rec.ReviewID, category: rec.Category, target: rec.Target}
		if seen[key] {
			continue
		}
		// Marked BEFORE the scope switch below, and that is safe for a reason rather than
		// by luck: duplicates of one triple always carry identical disposition_status /
		// filed_settled, because those joins are on the coordinate and not on rr.id — so
		// both copies would take the same scope branch anyway. Do not reorder on the
		// assumption that a skipped member should stay unmarked.
		seen[key] = true
		// Every scope must state its member selection EXPLICITLY here, with a default that
		// refuses. An `if scope == ScopeOpen` would have the same fail-open shape the
		// top-level guard exists to remove, just moved: adding a third scope to the
		// accepted set without touching this line would silently give it the WIDEST
		// semantics — re-asserting over settled verdicts. A switch makes the compiler's
		// reader ask "what does this new scope select?" at the point it matters.
		var include bool
		switch scope {
		case ScopeOpen:
			// Only what the SHARED ladder calls todo: a filed or already-settled member
			// keeps its state (PRD #98 Decision 2's definition of open).
			include = BucketOf(rec.DispositionStatus.String, rec.FiledSettled) == "todo"
		case ScopeAll:
			include = true
		default:
			return apitypes.JudgeDispositionResultDTO{}, ErrInvalidScope
		}
		if !include {
			continue
		}
		reviewIDs = append(reviewIDs, rec.ReviewID)
		writeCategories = append(writeCategories, rec.Category)
		writeTargets = append(writeTargets, rec.Target)
		hashes = append(hashes, RationaleHash(rec.RationaleMd))
		settled = append(settled, apitypes.JudgeSettledMemberDTO{
			RunID: rec.RunID.String(),
			RecID: rec.RecID.String(),
		})
	}

	updated := int64(0)
	if len(reviewIDs) > 0 {
		n, err := s.q.UpsertDispositionsForResolvedCoords(ctx, store.UpsertDispositionsForResolvedCoordsParams{
			Status: status,
			// "" → NULL: a 'done' carries no reason (the table CHECK is the backstop).
			DismissReason:   pgText(reason),
			SetByUserID:     pgUUID(ownerUserID),
			ReviewIds:       reviewIDs,
			Categories:      writeCategories,
			Targets:         writeTargets,
			RationaleHashes: hashes,
		})
		if err != nil {
			return apitypes.JudgeDispositionResultDTO{}, err
		}
		updated = n
	}

	// Re-read through the M1 grouped model so the response's groups and triage are the
	// SAME shape and the SAME ladder the page already renders — no second projection to
	// drift. bucket=all because a just-dismissed group has left To triage but must still
	// come back so the row can re-render at its new rollup.
	// nil categories: the post-write re-read is unfiltered by label — it must return the
	// just-settled group whatever its category, the same reason it passes bucket=all.
	backlog, err := s.JudgeRecommendationBacklog(ctx, ownerUserID, BucketAll, uuid.Nil, nil)
	if err != nil {
		return apitypes.JudgeDispositionResultDTO{}, err
	}
	return apitypes.JudgeDispositionResultDTO{
		Updated: int(updated),
		// The addresses the caller undoes through — the members THIS call settled, not the
		// ones a client believed were open. Nil-safe for a consumer: `settled` is always a
		// non-nil slice, so an empty fan-out marshals as [] rather than null.
		Settled: settled,
		Groups:  groupsForCoords(backlog.Groups, coords),
		// Carried through: past the cap, a settled coordinate can fall outside the read
		// window and have no group here. The flag is how a consumer tells that from
		// "settled and gone" (see JudgeDispositionResultDTO).
		Truncated: backlog.Truncated,
		Triage:    backlog.Triage,
	}, nil
}

// dedupeCoords collapses repeated coordinates, preserving first-seen order. A body that
// repeats a coordinate must not resolve its members twice (which would double the upserts
// and inflate `updated`), and de-duplicating BEFORE the cap check means the cap counts
// distinct work rather than body length.
func dedupeCoords(items []JudgeDispositionCoord) []JudgeDispositionCoord {
	seen := make(map[JudgeDispositionCoord]bool, len(items))
	out := make([]JudgeDispositionCoord, 0, len(items))
	for _, it := range items {
		if seen[it] {
			continue
		}
		seen[it] = true
		out = append(out, it)
	}
	return out
}

// groupsForCoords narrows the freshly-read backlog to the coordinates the caller acted on.
// A requested coordinate that resolved to nothing simply has no group — the same silence
// as one that was never sent, which is what keeps the response free of an existence oracle.
func groupsForCoords(groups []apitypes.JudgeRecommendationGroupDTO, coords []JudgeDispositionCoord) []apitypes.JudgeRecommendationGroupDTO {
	want := make(map[JudgeDispositionCoord]bool, len(coords))
	for _, c := range coords {
		want[c] = true
	}
	out := make([]apitypes.JudgeRecommendationGroupDTO, 0, len(coords))
	for _, g := range groups {
		if want[JudgeDispositionCoord{Category: g.Category, Target: g.Target}] {
			out = append(out, g)
		}
	}
	return out
}
