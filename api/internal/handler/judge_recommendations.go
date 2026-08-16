package handler

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// JudgeRecommendations serves the Judge menu's grouped backlog (PRD #98 M1, Decision 1):
// every recommendation across all of the caller's runs, deduped by (category, target),
// each group carrying its per-run occurrences, open_count, run_count and the most-recent
// rationale preview.
//
// Mounted on RequireUser next to /me/judge/stats, so `uzi review backlog` (M7) works from
// a CLI token. Owner-scoped by the query's user_id filter — IsAdmin is never consulted, so
// a uza_ admin_ro token reads its OWN backlog and nothing else. Read-only: no token spend,
// no forge write.
//
// ?bucket=todo|filed|done|dismissed|all (default todo) filters by the GROUP rollup;
// ?run=<uuid> is the notification deep-link anchor (/judge?run={id}) and keeps only groups
// that recur in that run — pushed down into the query's owner-scoped WHERE, so an anchor
// naming another user's run matches nothing. ?category= is a comma-separated list of
// recommendation labels (OR within the set); it is validated against the ingest-time
// taxonomy and pushed down into the same WHERE before the row cap, so a selected label
// never truncates off-page. All three are validated here — an unknown bucket, an unparseable
// run id, or an unknown category is a 400 rather than a silently-ignored filter, so a typo
// in a CLI flag can never look like an empty backlog. A well-formed but unknown/foreign run
// id is NOT an error: it returns an empty list, leaking no existence oracle. An absent or
// present-but-empty ?category= (and empty tokens like ?category=,improve_uzi) means "all
// labels" — it is normalized away before validation, never a 400.
//
// The pull is bounded by a hard row cap; the response's `truncated` flag says when it bit,
// so no consumer presents a cut backlog as complete. `triage` is never affected by either
// filter or by truncation — it is the canonical GET /me/judge/stats aggregate.
func (h *Handler) JudgeRecommendations(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	bucket := r.URL.Query().Get("bucket")
	if bucket == "" {
		// The backlog's reason to exist: what still needs triage. Referenced, not spelled —
		// the structural twin of the scope default below it, and drifting BucketTodo would
		// otherwise 400 every default-bucket request.
		bucket = workersvc.BucketTodo
	}
	if !workersvc.ValidJudgeBacklogBucket(bucket) {
		httpx.Error(w, http.StatusBadRequest, "invalid bucket")
		return
	}
	var runAnchor uuid.UUID
	if raw := r.URL.Query().Get("run"); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid run id")
			return
		}
		runAnchor = parsed
	}

	// NORMALIZE BEFORE VALIDATING. Comma-split, trim each token, and DROP empties, so a
	// present-but-empty ?category= (which splits to [""]) or a stray comma yields NO tokens
	// rather than a "" that would 400. An empty result stays nil, never []string{}: nil maps
	// to SQL NULL and means "all labels", while an empty Postgres array would match nothing —
	// the same NULL-sentinel distinction the service's Categories param comment spells out.
	var categories []string
	if raw := r.URL.Query().Get("category"); raw != "" {
		// DEDUP as we parse. The category set is a CLOSED 6-value enum, so a valid slice
		// carries at most one of each value — deduping here bounds it to ≤6 elements no
		// matter how many comma tokens the client sends (?category=improve_uzi,improve_uzi,…
		// is otherwise capped only by max header size and would build a large text[] that
		// costs a needless linear array scan in Postgres). Order of first appearance is kept.
		seen := make(map[string]bool)
		for _, tok := range strings.Split(raw, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			if !workersvc.ValidRecommendationCategory(tok) {
				httpx.Error(w, http.StatusBadRequest, "invalid category")
				return
			}
			if seen[tok] {
				continue
			}
			seen[tok] = true
			categories = append(categories, tok)
		}
	}

	backlog, err := h.wsvc.JudgeRecommendationBacklog(r.Context(), user.ID, bucket, runAnchor, categories)
	if err != nil {
		slog.Error("judge recommendation backlog", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, backlog)
}
