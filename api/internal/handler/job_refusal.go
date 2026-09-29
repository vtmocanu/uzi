package handler

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// notForJobReason is the machine-readable code of the PRD #1908 refusal: a worker route that
// reaches third-party content or a forge (publish, memory, forge reads and writes, the judge's
// trace/review and task-review) is not available for a kind='job' run. The check is server-side,
// keyed on runs.kind, so the agent's tool set is not the only barrier. A later job type that needs
// one of these routes opts in explicitly per type.
const notForJobReason = "not_for_job"

// refuseJobRuns refuses (403 not_for_job) a worker route whose path run {id} is a kind='job' run
// that the calling worker holds. Every other request, including one for a run the worker does not
// hold, passes to the handler untouched, so a non-job response is unchanged.
func (h *Handler) refuseJobRuns(next http.Handler) http.Handler {
	return h.refuseJobRunsWith(next, false)
}

// refuseJobReviewTargets is refuseJobRuns for the judge and task-review routes (trace, review,
// task-review), where {id} is the REVIEWED run rather than the run the worker holds: it refuses
// when the reviewed run is a job.
func (h *Handler) refuseJobReviewTargets(next http.Handler) http.Handler {
	return h.refuseJobRunsWith(next, true)
}

func (h *Handler) refuseJobRunsWith(next http.Handler, reviewed bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wkr, ok := mw.WorkerFromContext(r.Context())
		if !ok {
			next.ServeHTTP(w, r) // the handler answers the missing-auth case itself
			return
		}
		id, err := uuid.Parse(chi.URLParam(r, "id"))
		if err != nil {
			next.ServeHTTP(w, r) // the handler answers the malformed-id case itself
			return
		}
		isJob, err := h.wsvc.RunIsJobForRoute(r.Context(), wkr, id, reviewed)
		if err != nil {
			slog.Error("job route refusal lookup", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if isJob {
			httpx.ErrorReason(w, http.StatusForbidden, "this route is not available for job runs", notForJobReason)
			return
		}
		next.ServeHTTP(w, r)
	})
}
