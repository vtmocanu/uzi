package handler

// routes_judge.go carries the /api/me/judge + /api/findings route groups split out
// of Routes() (PRD #1008, epic #915 Batch 3). Pure motion.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountJudgeRoutes registers the global judge-triage strip (/me/judge) and the
// incidental-findings backlog (/findings).
func (h *Handler) mountJudgeRoutes(r chi.Router, forgeLimiter *mw.Limiter) {
	// Global judge-triage strip (PRD #94 Decision 8): the caller's "across all your
	// runs" tally. RequireUser (mirrors /me/memory) so `uzi review stats` works from a
	// CLI token; owner-scoped by the query's user_id filter, bucketed by the shared
	// Go ladder so it agrees with the per-review bar.
	r.Route("/me/judge", func(r chi.Router) {
		r.Use(mw.RequireUser(h.q, h.cfg))
		r.Get("/stats", h.JudgeStats)
		// Per-category GROUP counts for the Judge filter chips (PRD #244): a
		// SEPARATE endpoint + DTO from /stats so the nav badge (which reads only
		// triage.todo) is structurally unreachable from category data. Whole-backlog,
		// uncapped and triage-invariant, so the Judge page fetches it once on mount.
		r.Get("/category-stats", h.JudgeCategoryStats)
		// The Judge menu's grouped backlog (PRD #98 M1): the same owner-scoped,
		// all-time aggregate, deduped by (category, target) with the per-run
		// occurrences. Same mount as /stats so `uzi review backlog` works from a CLI
		// token; read-only, so no CSRF and no spend.
		r.Get("/recommendations", h.JudgeRecommendations)
		// Bulk group disposition (PRD #98 M2, Decision 3): one triage verdict fanned
		// out to the member coordinates of N groups. Same RequireUser mount as the
		// read (so `uzi review resolve|dismiss` works from a CLI token) and
		// owner-only by construction — the service's resolve is user_id-scoped, so
		// this route needs no authz of its own. A local upsert applied N times: no
		// spend, no forge write; the item cap bounds the work per request.
		r.Put("/recommendations/disposition", h.BulkSetDispositions)
	})

	// Incidental Findings backlog (PRD #333 M4/M5, D7/D8; PRD #365 M1): the per-repo,
	// owner-scoped, coordinate-deduped backlog of off-task bugs the worker flagged mid-run,
	// its issue draft, and the human-gated file/dismiss WRITES. All on RequireUser so the
	// `uzi findings` CLI verbs work from a uzc_ Bearer token, while browser callers still
	// carry a cookie with CSRF enforced via RequireUser's presence dispatch. Filing is a
	// forge write, but an owner-scoped, reversible one behind the per-user forge limiter;
	// dismiss is a LOCAL write (no forge call, no spend, no forge blast radius) and carries
	// no limiter. Mints/reveals/consent routes stay cookie-only RequireAuth.
	r.Route("/findings", func(r chi.Router) {
		r.Use(mw.RequireUser(h.q, h.cfg))
		// Reads: owner-scoped by the query's user_id filter, no forge write, no spend.
		r.Get("/", h.ListFindings)
		r.Get("/{id}/issue-draft", h.GetFindingIssueDraft)
		// Writes: filing rides the per-user forge limiter (mirroring FileIssue); dismiss is
		// a local write and carries no limiter.
		r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issue", h.FileFinding)
		r.Post("/{id}/dismiss", h.DismissFinding)
	})
}
