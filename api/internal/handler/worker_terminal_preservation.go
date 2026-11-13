package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Terminal preservation (issue #4007): POST /api/worker/runs/{id}/terminal-preservation. The worker
// of a terminal run uploads its final work and the API creates or fast-forwards
// refs/uzi-terminal/<run-id> with the stored credential. It shares held publication's step-A
// deadlines (heldUploadBudget, heldServiceBudget, heldResponseGrace), its process-wide slot pool
// (AcquireHeldPublishSlot) and its pack cap (maxPackBytes, here applied to the base64 text).

// terminalBodyOverhead is what the JSON body may carry beyond the base64 pack: the manifest (the
// store caps its canonical form at 256 KiB) and the scalar fields.
const terminalBodyOverhead = 1 << 20

// terminalMaxBodyBytes caps the whole JSON body.
var terminalMaxBodyBytes = int64(base64.StdEncoding.EncodedLen(maxPackBytes)) + terminalBodyOverhead

var terminalTipRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// terminalPublisher is the workersvc surface the route uses; *workersvc.Service satisfies it, and a
// test Handler can set terminalSvc to a fake without a database.
type terminalPublisher interface {
	AcquireHeldPublishSlot() (release func(), ok bool)
	TerminalPreservationAdmit(wkr store.Worker) error
	PublishTerminal(ctx context.Context, wkr store.Worker, runID uuid.UUID, req workersvc.TerminalPublishRequest) (workersvc.TerminalPublishResult, error)
}

func (h *Handler) terminalPublisher() terminalPublisher {
	if h.terminalSvc != nil {
		return h.terminalSvc
	}
	if h.wsvc != nil {
		return h.wsvc
	}
	return nil
}

// WorkerRunTerminalPreservation is the route. Order: authentication (RequireWorker), a concurrency
// slot, the pre-body capability check (no body read, no write), the deadlines, the body read capped
// at terminalMaxBodyBytes, a strict decode, field validation, and the service call.
func (h *Handler) WorkerRunTerminalPreservation(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	svc := h.terminalPublisher()
	if svc == nil {
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	release, ok := svc.AcquireHeldPublishSlot()
	if !ok {
		w.Header().Set("Retry-After", "5")
		httpx.ErrorReason(w, http.StatusTooManyRequests, "terminal preservation uploads are busy", "busy")
		return
	}
	defer release()

	if err := svc.TerminalPreservationAdmit(wkr); err != nil {
		writeTerminalError(w, runID, wkr, err)
		return
	}

	start := time.Now()
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(start.Add(heldUploadBudget))
	_ = rc.SetWriteDeadline(start.Add(heldUploadBudget + heldServiceBudget + heldResponseGrace))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, terminalMaxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		var netErr net.Error
		switch {
		case errors.As(err, &tooLarge):
			httpx.ErrorReason(w, http.StatusRequestEntityTooLarge, "terminal preservation body too large", apitypes.TerminalReasonPackTooLarge)
		case errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
			httpx.Error(w, http.StatusRequestTimeout, "terminal preservation upload timed out")
		default:
			httpx.Error(w, http.StatusBadRequest, "could not read terminal preservation body")
		}
		return
	}
	req, err := decodeTerminalRequest(body)
	if err != nil {
		httpx.ErrorReason(w, http.StatusBadRequest, err.Error(), "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), heldServiceBudget)
	defer cancel()
	res, err := svc.PublishTerminal(ctx, wkr, runID, req)
	if err != nil {
		writeTerminalError(w, runID, wkr, err)
		return
	}
	httpx.JSON(w, http.StatusOK, apitypes.TerminalPreservationResponse{
		PreservationID: res.PreservationID.String(), ContributionID: res.ContributionID.String(), Ref: res.Ref, Tip: res.Tip,
		State: res.State, PreservationState: res.PreservationState, Disposition: res.Disposition, Reason: res.Reason,
	})
}

// decodeTerminalRequest strictly decodes and validates the body: unknown fields (at any depth) and
// trailing data are refused, the generation is required, the tips are 40-hex, the disposition is
// preserve, the manifest entry counts are within their caps and the pack is standard base64. The
// manifest's own run, generation and worker are checked by the service against the authenticated
// values.
func decodeTerminalRequest(body []byte) (workersvc.TerminalPublishRequest, error) {
	var in apitypes.TerminalPreservationRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return workersvc.TerminalPublishRequest{}, errors.New("invalid terminal preservation body")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return workersvc.TerminalPublishRequest{}, errors.New("invalid terminal preservation body")
	}
	switch {
	case in.Generation < 1:
		return workersvc.TerminalPublishRequest{}, errors.New("invalid terminal generation")
	case !terminalTipRe.MatchString(in.Tip):
		return workersvc.TerminalPublishRequest{}, errors.New("invalid terminal tip")
	case in.PriorTip != "" && !terminalTipRe.MatchString(in.PriorTip):
		return workersvc.TerminalPublishRequest{}, errors.New("invalid terminal prior tip")
	case in.Disposition != apitypes.TerminalDispositionPreserve:
		return workersvc.TerminalPublishRequest{}, errors.New("invalid terminal disposition")
	case len(in.Manifest.Roots) > apitypes.TerminalManifestMaxRoots || len(in.Manifest.Abandoned) > apitypes.TerminalManifestMaxAbandoned:
		return workersvc.TerminalPublishRequest{}, errors.New("terminal manifest has too many entries")
	}
	var pack []byte
	if in.Pack != "" {
		var err error
		if pack, err = base64.StdEncoding.DecodeString(in.Pack); err != nil {
			return workersvc.TerminalPublishRequest{}, errors.New("invalid terminal pack encoding")
		}
		if len(pack) > maxPackBytes {
			return workersvc.TerminalPublishRequest{}, errors.New("terminal pack too large")
		}
	}
	return workersvc.TerminalPublishRequest{
		Generation: in.Generation, Tip: in.Tip, PriorTip: in.PriorTip, Manifest: in.Manifest, Pack: pack,
	}, nil
}

// writeTerminalError maps a service error. A refusal carries its reason code; an unrecognised error
// is a logged 500 with no detail.
func writeTerminalError(w http.ResponseWriter, runID uuid.UUID, wkr store.Worker, err error) {
	var refusal *workersvc.TerminalRefusal
	switch {
	case errors.Is(err, workersvc.ErrRunNotOwned):
		httpx.Error(w, http.StatusNotFound, "run not found for this worker")
	case errors.Is(err, workersvc.ErrStaleClaim):
		httpx.ErrorReason(w, http.StatusConflict, "terminal preservation claim is stale", "stale_claim")
	case errors.Is(err, workersvc.ErrMissingClaimGeneration):
		httpx.ErrorReason(w, http.StatusConflict, "terminal preservation requires a claim generation", "missing_claim_generation")
	case errors.As(err, &refusal):
		status := http.StatusConflict
		switch refusal.Reason {
		case apitypes.TerminalReasonUnsupported, apitypes.TerminalReasonManifestInvalid, apitypes.TerminalReasonPackInvalid,
			apitypes.TerminalReasonTipMissing, apitypes.TerminalReasonNotDescendant, apitypes.TerminalReasonRootUnreachable,
			apitypes.TerminalReasonTreeMismatch, apitypes.TerminalReasonWalkTooLarge:
			status = http.StatusUnprocessableEntity
		case apitypes.TerminalReasonPackTooLarge:
			status = http.StatusRequestEntityTooLarge
		case apitypes.TerminalReasonForgeUnavailable:
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "30")
		case apitypes.TerminalReasonPublicationInFlight:
			w.Header().Set("Retry-After", "30")
		}
		httpx.ErrorReason(w, status, "terminal preservation refused", refusal.Reason)
	default:
		slog.Error("worker run terminal preservation",
			"run_id", runID.String(), "worker_id", wkr.ID.String(), "reason", "internal",
			"error", secretscrub.Scrub(err.Error()))
		httpx.Error(w, http.StatusInternalServerError, "internal error")
	}
}
