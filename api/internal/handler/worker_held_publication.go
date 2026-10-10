package handler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Step A of held-work publication (issue #2545): the worker uploads one pack and the API creates
// refs/uzi-held/<run-id>/<generation> with the stored credential.
//
// The base listeners time a request out after 15 s, far too short for a pack upload followed by
// a forge push, so this one route extends its own deadlines through http.NewResponseController
// (the setJobFileDeadlines precedent):
//
//	upload   120 s  read deadline: the body read, outside any lock
//	service  150 s  one context for the pre-verify, tx1, the push and its read-back
//	grace     30 s  writing the answer
//
// and the write deadline is their sum, so the answer to a maximal upload followed by a maximal
// service still fits. The vars are named so a test can scale them, keeping the ratio.
var (
	heldUploadBudget  = 120 * time.Second
	heldServiceBudget = workersvc.HeldServiceBudget
	heldResponseGrace = 30 * time.Second
)

var (
	heldTipRe      = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	heldCoverageRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// heldPublisher is the workersvc surface the route uses; *workersvc.Service satisfies it, and a
// test Handler can set heldSvc to a fake without a database.
type heldPublisher interface {
	AcquireHeldPublishSlot() (release func(), ok bool)
	HeldPublishGate(ctx context.Context, wkr store.Worker, runID uuid.UUID, req workersvc.HeldPublishRequest) (*workersvc.HeldPublishResult, error)
	PublishHeld(ctx context.Context, wkr store.Worker, runID uuid.UUID, req workersvc.HeldPublishRequest, pack []byte) (workersvc.HeldPublishResult, error)
}

func (h *Handler) heldPublisher() heldPublisher {
	if h.heldSvc != nil {
		return h.heldSvc
	}
	if h.wsvc != nil {
		return h.wsvc
	}
	return nil
}

// WorkerRunHeldPublication is step A. Order: authentication (RequireWorker), header validation,
// a concurrency slot, the service's pre-body gate (ownership, scope, capability, switch and the
// reconcile-only short circuit, none of which reads the body), then the deadlines, the body read
// capped at maxPackBytes, and the service call.
func (h *Handler) WorkerRunHeldPublication(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	req := workersvc.HeldPublishRequest{
		Tip:      r.Header.Get("X-Uzi-Held-Tip"),
		Coverage: r.Header.Get("X-Uzi-Held-Coverage"),
	}
	gen, err := strconv.ParseInt(r.Header.Get("X-Uzi-Held-Generation"), 10, 64)
	switch {
	case !heldTipRe.MatchString(req.Tip):
		httpx.Error(w, http.StatusBadRequest, "invalid held tip")
		return
	case err != nil || gen < 1:
		httpx.Error(w, http.StatusBadRequest, "invalid held generation")
		return
	case !heldCoverageRe.MatchString(req.Coverage):
		httpx.Error(w, http.StatusBadRequest, "invalid held coverage")
		return
	}
	req.Generation = gen
	svc := h.heldPublisher()
	if svc == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}

	release, ok := svc.AcquireHeldPublishSlot()
	if !ok {
		w.Header().Set("Retry-After", "5")
		httpx.ErrorReason(w, http.StatusTooManyRequests, "held publication uploads are busy", "busy")
		return
	}
	defer release()

	done, err := svc.HeldPublishGate(r.Context(), wkr, runID, req)
	if err != nil {
		writeHeldError(w, runID, wkr, err)
		return
	}
	if done != nil {
		writeHeldResult(w, *done)
		return
	}

	start := time.Now()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(start.Add(heldUploadBudget))
	_ = rc.SetWriteDeadline(start.Add(heldUploadBudget + heldServiceBudget + heldResponseGrace))
	// Raw octet-stream body capped by MaxBytesReader, so an over-cap pack is a truthful 413
	// rather than a silently truncated, malformed pack. No lock is held here.
	pack, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPackBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		var netErr net.Error
		switch {
		case errors.As(err, &tooLarge):
			httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "held pack too large", workersvc.HeldReasonPackTooLarge)
		case errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
			httpx.Error(w, http.StatusRequestTimeout, "held pack upload timed out")
		default:
			httpx.Error(w, http.StatusBadRequest, "could not read held pack")
		}
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), heldServiceBudget)
	defer cancel()
	res, err := svc.PublishHeld(ctx, wkr, runID, req, pack)
	if err != nil {
		writeHeldError(w, runID, wkr, err)
		return
	}
	writeHeldResult(w, res)
}

func writeHeldResult(w http.ResponseWriter, res workersvc.HeldPublishResult) {
	httpx.JSON(w, http.StatusOK, apitypes.HeldPublicationResponse{
		PublicationID: res.PublicationID.String(), Ref: res.Ref, Tip: res.Tip, State: res.State, Reason: res.Reason,
	})
}

// writeHeldError maps a service error. A refusal carries its reason code; an unrecognised error
// is a logged 500 with no detail.
func writeHeldError(w http.ResponseWriter, runID uuid.UUID, wkr store.Worker, err error) {
	var refusal *workersvc.HeldRefusal
	switch {
	case errors.Is(err, workersvc.ErrRunNotOwned):
		httpx.Error(w, http.StatusNotFound, "run not found for this worker")
	case errors.As(err, &refusal):
		status := http.StatusConflict
		switch refusal.Reason {
		case workersvc.HeldReasonUnsupported, workersvc.HeldReasonPackInvalid, workersvc.HeldReasonTipMissing:
			status = http.StatusUnprocessableEntity
		case workersvc.HeldReasonPackTooLarge:
			status = http.StatusRequestEntityTooLarge
		case workersvc.HeldReasonForgeUnavailable:
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "30")
		}
		httpx.ErrorReason(w, status, "held publication refused", refusal.Reason)
	default:
		slog.Error("worker run held publication",
			"run_id", runID.String(), "worker_id", wkr.ID.String(), "reason", "internal",
			"error", secretscrub.Scrub(err.Error()))
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
