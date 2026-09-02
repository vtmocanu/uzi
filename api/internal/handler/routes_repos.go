package handler

// routes_repos.go carries the /api/repos/* route group split out of Routes()
// (PRD #1008, epic #915 Batch 3). Pure motion: the RequireUser (CLI-reachable) /
// RequireAuth (cookie-only) split within the sub-router moves intact.

import (
	"github.com/go-chi/chi/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// mountRepoRoutes registers the repos group: list/run-create/task-run/remove/
// schedules on RequireUser, and the cookie-only board, project-sync and issue
// routes on RequireAuth.
func (h *Handler) mountRepoRoutes(r chi.Router, forgeLimiter, boardOrderLimiter *mw.Limiter) {
	// Repos (PRD #64): GET / (uzi repo list), POST /{id}/runs (uzi run create) and
	// POST /{id}/task-runs (uzi handoff) are RequireUser; every other repo route is
	// cookie-only. PATCH /{id} is the F1 admin-write path (repo_skills_enabled /
	// repo_devbox_opt_in via PatchRepo's
	// IsAdmin branches), so it stays cookie-only — a Bearer credential 401s before
	// those branches. Split WITHIN the sub-router (not two /repos mounts) so chi
	// keeps one registration site per path+method.
	r.Route("/repos", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireUser(h.q, h.cfg))
			r.Get("/", h.ListRepos)
			// Queue an agent run from a card (PRD #4). forgeLimiter runs AFTER
			// RequireUser: PerUserMiddleware keys on the userKey RequireUser populates
			// and falls back (silently) to a shared IP bucket if it runs before auth —
			// so auth first, limiter second (B.4).
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/runs", h.CreateRun)
			// Explicit per-repo remove (PRD #357). RequireUser (NOT the cookie-only
			// RequireAuth group below) so the CLI's Bearer token is accepted — a
			// cookie-only mount would 401 it (issue #428 regression). No forge limiter:
			// the handler makes no forge call, like POST /{id}/schedules. Owner-scoped
			// and guarded server-side (GetRepoForUser 404, enabled/active-run 409) inside
			// the handler.
			r.Delete("/{id}", h.DeleteRepo)
			// Queue a task/handoff run (PRD #400): ephemeral, branch-scoped,
			// issue-less. Same per-user forge budget as the other run creators.
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/task-runs", h.CreateTaskRun)
			// Create a scheduled run on this repo (PRD #241 M4). Owner-scoped
			// (GetRepoForUser inside the handler → 404 for a foreign repo). No forge
			// limiter: create validates config and computes next_fire_at without a
			// forge read (run-now, which does read the forge, carries the limiter).
			r.Post("/{id}/schedules", h.CreateSchedule)
			// Enable a builtin default scheduled job on this repo (PRD #589 M2).
			// Owner-scoped (repoForRequest → 404 for a foreign repo); idempotent per
			// (user, repo, slug) via the partial unique index (a repeat enable returns
			// the existing row, 200). No forge limiter: it computes next_fire_at from the
			// catalog cron without a forge read, mirroring CreateSchedule.
			r.Post("/{id}/schedule-catalog/{slug}", h.EnableCatalogSchedule)
			// Sweep-label guardrail primitives (PRD #589 M4): check which selector
			// labels are missing on the repo (WARN) and create them (CONFIRM). Both
			// read/write the repo's forge (ListLabels / EnsureLabels), so they carry
			// the per-user forge budget like the other proxying routes (e.g.
			// run-now, /{id}/issues). Owner-scoped (repoForRequest → 404 for a
			// foreign repo). Generic in {labels} so both the enable-sweep and
			// custom-sweep create/edit flows (and M6's web) reuse them.
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/labels/check", h.CheckRepoLabels)
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/labels/ensure", h.EnsureRepoLabels)
			// GitHub Projects v2 sync READS + RESYNC for the CLI (PRD #576 M7):
			// RequireUser (NOT the cookie-only RequireAuth group below) so the CLI's
			// `uzc_` Bearer token is accepted — a cookie-only mount would 401 it
			// (issue #428 regression). Both are owner-scoped and guarded server-side by
			// the same GetRepoForUser preflight inside each handler (404 for a
			// foreign/unknown repo); under the CLI token's IsAdmin=false ceiling they
			// are correctly owner-only. No forge limiter: the status read hits only the
			// stored projection, and resync re-seeds without charging the forge budget.
			// The mutating link setup (Adopt/Provision/autocreate/disable) and the
			// board-access controls stay on RequireAuth (cookie-only) below — web-only.
			r.Get("/{id}/github-project-sync", h.GetGithubProjectSyncStatus)
			r.Post("/{id}/github-project-sync/resync", h.ResyncGithubProjectSync)
		})
		r.Group(func(r chi.Router) {
			r.Use(mw.RequireAuth(h.q, h.cfg))
			r.Put("/{id}", h.SetRepoEnabled)
			// Repo-skills opt-in toggle (PRD #16): repo owner or admin.
			r.Patch("/{id}", h.PatchRepo)
			// GitHub Projects v2 sync, owner-or-admin (issue #534, PRD #364 follow-up):
			// relocated out of /admin (D4). Owner path is guarded by a GetRepoForUser
			// preflight inside each handler; admin skips it. Instance flag still gates.
			// The status read and resync moved UP to the RequireUser group (PRD #576 M7)
			// so the CLI's Bearer token reaches them; the mutating link setup below stays
			// cookie-only (web-only — Adopt/Provision/autocreate/disable, D4).
			// Owner-type read for the Adopt-first Provision nudge (PRD #576 M1):
			// a live forge round-trip (repositoryOwner __typename), fetched for a
			// not-yet-linked repo so it needs no link row. Web-only; the CLI does
			// not use it.
			r.Get("/{id}/github-project-sync/owner-type", h.GetGithubProjectOwnerType)
			r.Post("/{id}/github-project-sync", h.AdoptGithubProjectSync)
			r.Post("/{id}/github-project-sync/provision", h.ProvisionGithubProjectSync)
			// Safe column auto-create (PRD #576 M6): create a fresh uzi-owned
			// Status field with all the repo's columns and switch the link to it,
			// turning skipped columns into synced ones with no destructive replace.
			// Same owner-or-admin write group → noLimiter.
			r.Post("/{id}/github-project-sync/autocreate-columns", h.AutoCreateGithubProjectColumns)
			r.Delete("/{id}/github-project-sync", h.DisableGithubProjectSync)
			// Board access controls (PRD #557): read/flip the board's visibility, and
			// grant/revoke Reader access by username. Same owner-or-admin preflight +
			// instance-flag gate as the routes above.
			r.Get("/{id}/github-project-sync/visibility", h.GetGithubProjectVisibility)
			r.Put("/{id}/github-project-sync/visibility", h.SetGithubProjectVisibility)
			r.Post("/{id}/github-project-sync/collaborators", h.ShareGithubProjectSync)
			r.Delete("/{id}/github-project-sync/collaborators", h.UnshareGithubProjectSync)
			// Per-repo tool profile (PRD #18 M4): the owner's tier-1 package list.
			// Owner-only (a repo belongs to one user's connection).
			r.Get("/{id}/tool-profile", h.GetRepoToolProfile)
			r.Put("/{id}/tool-profile", h.SetRepoToolProfile)
			r.Get("/{id}/board", h.GetBoard)
			// Per-user, per-repo board view preferences (PRD #196 M3): the
			// membership extras override and "show all other issues" toggle, moved
			// server-side from per-browser localStorage. No limiter — no forge call,
			// like the board/tool-profile reads. VISIBILITY only, never eligibility.
			r.Get("/{id}/board/prefs", h.GetBoardPrefs)
			r.Put("/{id}/board/prefs", h.PutBoardPrefs)
			r.Put("/{id}/board/columns", h.ConfigureColumns)
			// Manual card order (PRD #102 M5). The other whole-board replace, so it
			// sits beside the columns route and is a PUT for the same reason.
			//
			// Its OWN limiter, not forgeLimiter and not none. Not the forge budget
			// because this endpoint makes ZERO forge calls, and charging a drag there
			// would let a burst of reordering starve the user's actual forge
			// operations (move, sync, issue create) — the thing that budget protects.
			// Not bare either: every request is a whole-board renumber in a
			// transaction plus a full board rebuild, which makes it the most
			// write-amplifying authenticated route on the board. "Give it its own" is
			// the answer this codebase has already reached five times.
			r.With(boardOrderLimiter.PerUserMiddleware).Put("/{id}/board/order", h.SetBoardOrder)
			// The in-app issue view fetches the issue (with its description) live
			// from the forge → per-user budget.
			r.With(forgeLimiter.PerUserMiddleware).Get("/{id}/issues/{iid}", h.GetIssueDetail)
			// move + sync write/read through to the forge → per-user budget.
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues/{iid}/move", h.MoveIssue)
			// Promote (PRD #102 Decision 15, PRD #764): add the uzi run-eligibility
			// label to a non-uzi card, forge-first. Same limiter as the other
			// forge-writing issue routes — it is one EnsureLabels plus one
			// UpdateIssueLabels, like move beside it.
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues/{iid}/promote", h.PromoteIssue)
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/sync", h.SyncRepo)
			// Create a PRD issue on the forge (source of truth) → per-user budget.
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/issues", h.CreateIssue)
			// Queue a CI-fix run for a failed pipeline (PRD #6). Snapshots the
			// failed pipeline's jobs + logs from the forge → per-user budget.
			r.With(forgeLimiter.PerUserMiddleware).Post("/{id}/ci-fix-runs", h.CreateCIFixRun)
		})
	})
}
