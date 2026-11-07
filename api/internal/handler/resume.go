package handler

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ResumeRunNow resumes an owner-held pool_wait or paused run, or starts a new
// worker-recovery episode for recovery_wait/worker_requeue_exhausted. The owner-scoped
// read returns 404 for foreign/absent runs. Each status-scoped write refuses races
// and ineligible holds with 409; successful transitions write a resume audit row.
//
// The ordinary verbs take no payload: an empty body is today's path and nothing in it is read.
// A non-empty body is the compact policy-hold continuation (issue #3607): a strict
// {mode, expected_episode_id, guidance} that resolves exactly one applied codex_policy_hold
// episode (see resumePolicyHold). An empty request never consents to a policy continuation.
func (h *Handler) ResumeRunNow(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	// Read FIRST (PRD #1190 M1): ownership is the predicate, so a foreign/absent run is 404
	// before any write, and the dispatch below branches on the current status.
	run, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, policyResumeBodyLimit+1))
	if err != nil || int64(len(raw)) > policyResumeBodyLimit {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		h.resumePolicyHold(w, r, user.ID, runID, raw)
		return
	}
	// Only an unresolved episode blocks the ordinary resume; a resolved preserving run keeps its
	// constraint and resumes ordinary holds normally.
	if workersvc.UnresolvedPolicyEpisode(run) {
		httpx.Error(w, http.StatusConflict, "policy hold continuation is unavailable without an explicit mode; resume it with mode and expected_episode_id")
		return
	}

	// auditBody is the body of the kind='resume' audit row; empty for an ordinary resume, and the
	// recorded weekly-reserve override (PRD #3522 M4a) when the owner resumed a reserve pause.
	var auditBody pgtype.Text
	switch run.Status {
	case "recovery_wait":
		if run.RecoveryWaitCause.Valid && run.RecoveryWaitCause.String == "provider_outage" {
			// PRD #1733: a typed provider park retries on its own backoff. Only an enabled
			// RunProviderParkMaxDuration turns the streak into an owner-resumed hold.
			hint := "run is waiting out a provider outage and retries automatically"
			if h.cfg.RunProviderParkMaxDuration > 0 {
				hint += "; once the outage streak passes its limit it pauses for you to resume"
			}
			httpx.Error(w, http.StatusConflict, hint)
			return
		}
		if !run.RecoveryWaitCause.Valid || (run.RecoveryWaitCause.String != "forge_unreachable" && run.RecoveryWaitCause.String != "data_volume_full" && run.RecoveryWaitCause.String != "worker_requeue_exhausted" && run.RecoveryWaitCause.String != "worker_memory_pressure" && run.RecoveryWaitCause.String != "retained_recovery_exhausted" && run.RecoveryWaitCause.String != "codex_idle_stall" && run.RecoveryWaitCause.String != "retained_recovery_adoption_failed" && run.RecoveryWaitCause.String != "retained_continuation_unwitnessed") {
			httpx.Error(w, http.StatusConflict, fmt.Sprintf("run is %s", run.Status))
			return
		}
		var resumeErr error
		switch run.RecoveryWaitCause.String {
		case "data_volume_full":
			_, resumeErr = h.q.ResumeDataVolumeFullHold(r.Context(), store.ResumeDataVolumeFullHoldParams{
				ID: runID, UserID: user.ID, GlobalTimeoutSeconds: int32(h.cfg.RunTimeout.Seconds()),
			})
		case "forge_unreachable":
			if run.RecoveryWaitPhase.Valid {
				// Issue #1324: a publication-phase hold has its own Resume, which also starts a new
				// episode on the intent. It runs ahead of the pre-clone arm, which never matches it.
				var resumed bool
				if resumed, resumeErr = h.wsvc.ResumePublicationForgeHold(r.Context(), user.ID, run); resumeErr == nil && !resumed {
					resumeErr = pgx.ErrNoRows
				}
				break
			}
			_, resumeErr = h.q.ResumeForgeUnreachableHold(r.Context(), store.ResumeForgeUnreachableHoldParams{
				ID: runID, UserID: user.ID, GlobalTimeoutSeconds: int32(h.cfg.RunTimeout.Seconds()),
			})
		case "codex_idle_stall":
			_, resumeErr = h.q.ResumeCodexIdleStall(r.Context(), store.ResumeCodexIdleStallParams{ID: runID, UserID: user.ID})
		case "worker_memory_pressure":
			// Issue #3639: the resume and the settled-park lift commit together or not at all.
			if h.pool != nil {
				resumeErr = workersvc.ResumeMemoryHold(r.Context(), h.pool, runID, user.ID, int32(h.cfg.RunTimeout.Seconds()))
			} else {
				_, resumeErr = h.q.ResumeMemoryEpisode(r.Context(), store.ResumeMemoryEpisodeParams{
					ID: runID, UserID: user.ID, GlobalTimeoutSeconds: int32(h.cfg.RunTimeout.Seconds()),
				})
			}
		default:
			_, resumeErr = h.q.ResumeWorkerRecoveryEpisode(r.Context(), store.ResumeWorkerRecoveryEpisodeParams{
				ID: runID, UserID: user.ID, GlobalTimeoutSeconds: int32(h.cfg.RunTimeout.Seconds()),
			})
		}
		if resumeErr != nil {
			if errors.Is(resumeErr, pgx.ErrNoRows) {
				httpx.Error(w, http.StatusConflict, "run cannot resume: hold changed or no remaining budget")
				return
			}
			slog.Error("resume recovery episode", "error", resumeErr)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	case "pool_wait":
		// PRD #754 M5: promote a pooled-token hold. Owner+status-scoped; a race that moved the
		// run out of pool_wait between the read and this write yields 0 rows → 409 below.
		rows, err := h.q.PromotePoolWaitRun(r.Context(), store.PromotePoolWaitRunParams{ID: runID, UserID: user.ID})
		if err != nil {
			slog.Error("resume run now (pool_wait)", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if rows == 0 {
			httpx.Error(w, http.StatusConflict, "run is not waiting for a pooled token")
			return
		}
	case "paused":
		// issue #3194: a base-attestation hold is owner-resolved by recovery, export or a
		// successor, never by running this run again (ResumePausedRun refuses it too).
		if workersvc.IsBaseAttestationHeld(run) {
			httpx.Error(w, http.StatusConflict, workersvc.ErrBaseAttestationHeld.Error())
			return
		}
		// PRD #1497 M1 (D6): a completion hold resumes ONLY through its own decision endpoint, never
		// this generic resume — 409 naming it (today this would silently resume the hold).
		if run.HoldReason.Valid && run.HoldReason.String == "completion_blocked" {
			httpx.Error(w, http.StatusConflict, "this run is blocked on a completion decision; resolve it at POST /api/runs/{id}/completion/decision")
			return
		}
		// Resume owner pauses, wall holds and iteration holds through the same accounting.
		// ResumePausedRun banks parked time and keeps started_at; completion holds require
		// their own endpoint, and both exhausted holds require remaining wall budget.
		// A refused write can also mean a competing owner action changed the hold.
		resumeParams := store.ResumePausedRunParams{
			ID:                         runID,
			UserID:                     user.ID,
			AllowCompletionBlockedHold: false,
			GlobalTimeoutSeconds:       int32(h.cfg.RunTimeout.Seconds()),
			StaleCutoff:                workersvc.HeartbeatStaleCutoff(h.clock(), h.cfg.WorkerHeartbeatStale),
		}
		// PRD #3522 M4a (D9): resuming a weekly-reserve pause is an explicit, recorded OVERRIDE until
		// the next observed 7-day reset, so the requeue is not held again at admission and the run
		// does not re-pause at once. The grant is computed from the owner's current reading of the
		// credential the run spent; the hold guard makes a hold that changed since the read above a
		// 409 rather than an override granted under the wrong cause.
		if run.HoldReason.Valid && run.HoldReason.String == "weekly_reserve" {
			resumeParams.RequiredHoldReason = pgtype.Text{String: "weekly_reserve", Valid: true}
			grant, gerr := workersvc.ComputeReserveOverride(r.Context(), h.q, run,
				workersvc.ReserveIntervals{Claude: h.cfg.UsagePollInterval, Codex: h.cfg.CodexUsagePollInterval}, h.clock())
			if gerr != nil {
				slog.Error("resume run now: compute weekly reserve override", "error", gerr)
				httpx.Error(w, http.StatusInternalServerError, "internal error")
				return
			}
			if grant != nil {
				resumeParams.ReserveExemptGrantedAt = pgtype.Timestamptz{Time: grant.GrantedAt, Valid: true}
				resumeParams.ReserveExemptUntil = pgtype.Timestamptz{Time: grant.Until, Valid: true}
				if grant.CredentialID != nil {
					resumeParams.ReserveExemptCredentialID = pgtype.UUID{Bytes: *grant.CredentialID, Valid: true}
				}
				if grant.WindowReset != nil {
					resumeParams.ReserveExemptWindowReset = pgtype.Timestamptz{Time: *grant.WindowReset, Valid: true}
				}
				if grant.PriorPct != nil {
					resumeParams.ReserveExemptPriorPct = pgtype.Int2{Int16: *grant.PriorPct, Valid: true}
				}
				auditBody = pgtype.Text{String: "overrode the weekly usage reserve until " + grant.Until.UTC().Format(time.RFC3339), Valid: true}
			}
		}
		if _, err := h.q.ResumePausedRun(r.Context(), resumeParams); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Issue #3181: a credential_switch_settling hold whose worker is still finishing its own
				// operations for the run is its own refusal, not "no longer paused". Re-read the fence
				// (the same predicate ResumePausedRun refuses on) rather than trusting the earlier
				// read of the hold reason.
				settling, serr := h.q.CredentialSwitchHoldSettling(r.Context(), store.CredentialSwitchHoldSettlingParams{
					RunID: runID, StaleCutoff: workersvc.HeartbeatStaleCutoff(h.clock(), h.cfg.WorkerHeartbeatStale),
				})
				if serr != nil {
					slog.Warn("resume run: re-read hold fence after refused resume", "run", runID, "error", serr)
				} else if settling {
					httpx.Error(w, http.StatusConflict, workersvc.ErrRunHoldSettling.Error())
					return
				}
				// Read the current iteration hold before naming exhaustion: a refused resume
				// can also mean that a competing owner action moved the run.
				if current, readErr := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: user.ID}); readErr == nil &&
					current.Status == "paused" && current.HoldReason.String == "iteration_budget_exhausted" && current.StartedAt.Valid {
					wall := int64(h.cfg.RunTimeout.Seconds())
					if current.BudgetWallSeconds.Valid {
						wall = int64(current.BudgetWallSeconds.Int32)
					}
					total := wall + int64(current.BudgetExtensionSeconds) + int64(current.BudgetFinalizeSeconds)
					used := max(0, int64(current.StatusSince.Time.Sub(current.StartedAt.Time).Round(time.Second).Seconds())) - int64(current.BudgetPausedSeconds)
					if total-used <= 0 {
						httpx.Error(w, http.StatusConflict, fmt.Sprintf("this run is out of time; extend it, then resume: uzi run extend %s --by 2h; uzi run resume %s", runID, runID))
						return
					}
				}
				if run.HoldReason.Valid && run.HoldReason.String == "budget_exhausted" {
					httpx.Error(w, http.StatusConflict, fmt.Sprintf("this run is out of time; extend it to resume: uzi run extend %s --by 2h", runID))
					return
				}
				// PRD #1732 D12: ResumePausedRun refuses every credential_disabled hold
				// (hold_reason IS DISTINCT FROM 'credential_disabled'), so name the fix, not the race text.
				if run.HoldReason.Valid && run.HoldReason.String == "credential_disabled" {
					httpx.Error(w, http.StatusConflict, workersvc.ErrCredentialDisabled.Error())
					return
				}
				httpx.Error(w, http.StatusConflict, "run is no longer paused")
				return
			}
			slog.Error("resume run now (paused)", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
	default:
		// Every other status is either terminal or a park with its own resume (a limit_wait
		// auto-resumes; the gates resume on their own input), so there is nothing to resume here.
		httpx.Error(w, http.StatusConflict, fmt.Sprintf("run is %s", run.Status))
		return
	}

	// Successful transitions write the kind='resume' audit row (PRD #1190 D14): the single
	// resume mechanism, so exactly one writer. Best-effort — the transition already committed,
	// so a failed audit write must not fail the resume. 'resume' is a server-only kind excluded
	// from ConsumeRunInputs, so the worker never drains it.
	if _, err := h.q.CreateRunInput(r.Context(), store.CreateRunInputParams{RunID: runID, Kind: "resume", Body: auditBody}); err != nil {
		slog.Warn("write resume audit row", "run", runID, "error", err)
	}

	h.writeResumedRun(w, r, user.ID, runID, false)
}

// writeResumedRun re-reads the run owner-scoped so the DTO shows the resumed (queued) status.
func (h *Handler) writeResumedRun(w http.ResponseWriter, r *http.Request, userID, runID uuid.UUID, replay bool) {
	run, err := h.q.GetRunByIDForUser(r.Context(), store.GetRunByIDForUserParams{ID: runID, UserID: userID})
	if err != nil {
		httpx.Error(w, http.StatusNotFound, "run not found")
		return
	}
	out := map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock(), h.cfg.RunMaxRequeues)}
	if replay {
		// A repeated identical policy resume (lost response): nothing changed, say so.
		out["replay"] = true
	}
	httpx.JSON(w, http.StatusOK, out)
}

// policyResumeBodyLimit bounds the optional resume body: 16,000 bytes of guidance can
// expand up to sixfold under JSON escaping, plus the fixed fields.
const policyResumeBodyLimit int64 = 128 * 1024

// policyResumeBody is the closed owner request for a compact policy-hold continuation.
type policyResumeBody struct {
	Mode              string `json:"mode"`
	ExpectedEpisodeID string `json:"expected_episode_id"`
	Guidance          string `json:"guidance"`
}

// resumePolicyHold resolves one exact applied policy episode. Every refusal is zero-mutation: the
// service transaction repeats owner, status, hold reason, episode id and lane predicates.
func (h *Handler) resumePolicyHold(w http.ResponseWriter, r *http.Request, userID, runID uuid.UUID, raw []byte) {
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var body policyResumeBody
	if err := httpx.DecodeJSONStrictBounded(r, &body, policyResumeBodyLimit); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	episode, err := uuid.Parse(body.ExpectedEpisodeID)
	if err != nil || episode == uuid.Nil || episode.String() != body.ExpectedEpisodeID {
		httpx.Error(w, http.StatusBadRequest, "expected_episode_id must be a canonical UUID")
		return
	}
	res, err := h.wsvc.ResumePolicyHold(r.Context(), userID, runID, workersvc.PolicyResumeRequest{
		Mode: body.Mode, ExpectedEpisodeID: episode, Guidance: body.Guidance,
	})
	switch {
	case err == nil:
		h.writeResumedRun(w, r, userID, runID, res.Replay)
	case errors.Is(err, workersvc.ErrInvalidState):
		httpx.Error(w, http.StatusBadRequest, "invalid policy resume request")
	case errors.Is(err, workersvc.ErrPolicyFreshUnavailable):
		httpx.Error(w, http.StatusConflict, workersvc.ErrPolicyFreshUnavailable.Error())
	case errors.Is(err, workersvc.ErrPolicyResumeGuidanceMismatch):
		httpx.Error(w, http.StatusConflict, "already resumed with different guidance; the new guidance was not recorded")
	case errors.Is(err, workersvc.ErrPolicyResumeConflict):
		httpx.Error(w, http.StatusConflict, "policy hold changed or does not match this mode; refresh the run and choose again")
	case errors.Is(err, workersvc.ErrRunNotFound):
		httpx.Error(w, http.StatusNotFound, "run not found")
	default:
		slog.Error("resume policy hold", "run", runID, "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
