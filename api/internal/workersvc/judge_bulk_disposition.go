package workersvc

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// JudgeDispositionMaxItems caps the coordinates one bulk call may carry (PRD #98 M2).
// #94's single-coordinate route needed no such bound — it was one resolve and one upsert —
// but this endpoint is N resolves and N upserts on the no-CSRF RequireUser token path, so
// the work per request is bounded explicitly rather than left to the 1 MiB body limit.
// Comfortably above any real multi-select: the UI batches a screenful of groups, not a
// backlog. Over the cap is a 400, not a silent truncation.
const JudgeDispositionMaxItems = 100

// ErrTooManyItems — the bulk call carried more coordinates than JudgeDispositionMaxItems.
// The handler maps it to 400. Distinct from a resolve miss, which is not an error at all.
var ErrTooManyItems = errors.New("too many items")

// judgeDispositionScopes is the scope enum: "open" (the default) touches only members the
// shared ladder buckets as todo — a FILED member is left filed (PRD #98 Decision 2's
// definition of open) — while "all" re-asserts across every member of the coordinate.
//
// Unexported for the same reason as judgeBacklogBuckets: an exported package-level map is
// mutable from anywhere in the binary, so a validator built on one can be widened out of
// sight of the handler that relies on it. Here that would be a security-relevant widening,
// since the scope is what keeps a settled member's verdict from being overwritten.
var judgeDispositionScopes = map[string]bool{"open": true, "all": true}

// ValidJudgeDispositionScope reports whether s is an accepted scope. Anything else is a
// 400 — never a silent fallback to a scope the caller did not ask for.
func ValidJudgeDispositionScope(s string) bool { return judgeDispositionScopes[s] }

// JudgeDispositionCoord is one requested (category, target) coordinate. It is the
// caller's REQUEST, not a resolved row: nothing from here is ever written to the database.
// The values are used only to match against review_recommendations; the disposition is
// written from the resolved row's own columns (see BulkSetDispositions).
type JudgeDispositionCoord struct {
	Category string `json:"category"`
	Target   string `json:"target"`
}

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
// Each write is #94's own idempotent coordinate upsert with the rationale_hash re-stamped
// from the CURRENT rationale (its Decisions 3/6), so a double-click converges rather than
// duplicating.
//
// PARTIAL-FAILURE CONTRACT. The N upserts are NOT wrapped in a transaction, deliberately:
// each is a local, side-effect-free, last-writer-wins upsert (there is no forge write and
// no token spend to make exactly-once), so a partial apply is safely retried and converges.
// On the first upsert error this returns the ZERO DTO and the error, which the handler maps
// to a 500 — it does NOT report how many writes landed. So if upsert 2 of 3 fails, one
// disposition is already committed and the client is told only "internal error".
//
// That is the intended behavior, not an oversight: a 500 makes no false claim of success,
// the landed subset is visible on the very next read, and a retry converges because every
// write is idempotent. The requirement is that the endpoint must never CLAIM completeness
// it does not have, and returning nothing satisfies it. A partial-success report (207, or
// 200 with a `partial` flag) is a deliberate non-goal for v1 — revisit `updated` and the
// re-read together if that ever changes.
func (s *Service) BulkSetDispositions(ctx context.Context, ownerUserID uuid.UUID, items []JudgeDispositionCoord, status, reason, scope string) (apitypes.JudgeDispositionResultDTO, error) {
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

	updated := 0
	for _, rec := range members {
		// scope=open skips anything the SHARED ladder does not call todo, so a filed or
		// already-settled member keeps its state unless the caller asked for `all`.
		if scope == "open" && BucketOf(rec.DispositionStatus.String, rec.FiledSettled) != "todo" {
			continue
		}
		_, err := s.q.UpsertRecommendationDisposition(ctx, store.UpsertRecommendationDispositionParams{
			ReviewID: rec.ReviewID,
			Category: rec.Category, // the RESOLVED row's, never the request body's
			Target:   rec.Target,   // likewise
			Status:   status,
			// "" → NULL: a 'done' carries no reason (the table CHECK is the backstop).
			DismissReason: pgText(reason),
			RationaleHash: RationaleHash(rec.RationaleMd),
			SetByUserID:   pgUUID(ownerUserID),
		})
		if err != nil {
			return apitypes.JudgeDispositionResultDTO{}, err
		}
		updated++
	}

	// Re-read through the M1 grouped model so the response's groups and triage are the
	// SAME shape and the SAME ladder the page already renders — no second projection to
	// drift. bucket=all because a just-dismissed group has left To triage but must still
	// come back so the row can re-render at its new rollup.
	backlog, err := s.JudgeRecommendationBacklog(ctx, ownerUserID, "all", uuid.Nil)
	if err != nil {
		return apitypes.JudgeDispositionResultDTO{}, err
	}
	return apitypes.JudgeDispositionResultDTO{
		Updated: updated,
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
