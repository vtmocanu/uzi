package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// The brokered checkpoint publish (WorkerRunPublish) used to run under the base listener
// timeouts (httpx.ServerReadTimeout / httpx.ServerWriteTimeout, 15 s each), although a publish
// legitimately spends longer in its pre-push work and the forge push. The route therefore
// extends its own deadlines through http.NewResponseController, as held publication does:
//
//	upload    60 s  read deadline: the body read, outside any service work
//	service  190 s  workersvc.PublishServiceBudget: one context for the whole service call
//	grace     15 s  writing the answer
//
// and the write deadline is their sum (265 s), so the answer to a maximal upload followed by a
// maximal service still fits. The vars are named so a test can scale them, keeping the ratio.
var (
	publishUploadBudget  = 60 * time.Second
	publishServiceBudget = workersvc.PublishServiceBudget
	publishResponseGrace = 15 * time.Second
)

// errPublishServiceDeadline is what the handler logs when the service budget elapses before the
// publish answered. It is fixed text: the service's own error may name nothing the log needs.
var errPublishServiceDeadline = errors.New("publish: service budget exceeded")

// errPublishPanic is the fixed error a panicking publish goroutine reports.
var errPublishPanic = errors.New("publish: panic")

// checkpointPublisher is the workersvc surface WorkerRunPublish uses; *workersvc.Service
// satisfies it, and a test Handler can set publishSvc to a fake.
type checkpointPublisher interface {
	PublishAttested(ctx context.Context, wkr store.Worker, runID uuid.UUID, tipOid string, attestedSource *string, pack []byte, generation *int64) (workersvc.PublishResult, error)
}

// checkpointPublisher returns the test seam when set, else the real service, else nil.
func (h *Handler) checkpointPublisher() checkpointPublisher {
	if h.publishSvc != nil {
		return h.publishSvc
	}
	if h.wsvc != nil {
		return h.wsvc
	}
	return nil
}

type publishOutcome struct {
	res workersvc.PublishResult
	err error
}

// runPublishWithDeadline runs the publish in its own goroutine under publishServiceBudget and
// waits for the result or the deadline, whichever is first, so a publisher that ignores its
// context cannot hold the response past the service budget. On the deadline it returns
// errPublishServiceDeadline and the goroutine finishes on its own: its result goes to a buffered
// channel with a single send, so it never blocks, and a panic is recovered and logged so a late
// panic cannot take the process down.
//
// Cleanup tail after the deadline. The goroutine can outlive the response by a FINITE tail,
// because the supersession loops (claimCheckpointSlot, freeCheckpointSlot) stop starting new
// detached operations once ctx is cancelled, and every call that is not detached uses the
// cancelled ctx. What can still run:
//   - one in-flight locked operation (supersedeRetainedCheckpoint via withRetentionLock):
//     at most retentionOpTimeout, plus its bookkeeping window retentionRecordTimeout
//     (retentionBookkeepingCtx);
//   - its unlock (releaseRetentionLock) at most retentionUnlockTimeout and, when the unlock
//     fails, destroyRetentionConn at most another retentionUnlockTimeout;
//   - a bounded number of unlocked bookkeeping calls, each at most retentionRecordTimeout.
//
// An upper ESTIMATE (not a guarantee): about 120 + 10 + 10 + 10 plus a few 10 s bookkeeping
// calls, roughly three minutes after the deadline. The cancellation guard stops later
// iterations once it is observed; it is not an atomic guarantee that cancellation cannot race
// entry into one detached operation, and that one operation is part of this tail.
func runPublishWithDeadline(parent context.Context, svc checkpointPublisher, wkr store.Worker, runID uuid.UUID, tip string, attestedSource *string, body []byte, generation *int64) (workersvc.PublishResult, error) {
	ctx, cancel := context.WithTimeout(parent, publishServiceBudget)
	defer cancel()
	done := make(chan publishOutcome, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				slog.Error("worker run publish panic",
					"run_id", runID.String(),
					"worker_id", wkr.ID.String(),
					"panic", secretscrub.Scrub(fmt.Sprint(p)),
					"stack", string(debug.Stack()),
				)
				done <- publishOutcome{err: errPublishPanic}
			}
		}()
		res, err := svc.PublishAttested(ctx, wkr, runID, tip, attestedSource, body, generation)
		done <- publishOutcome{res: res, err: err}
	}()
	select {
	case o := <-done:
		return o.res, o.err
	case <-ctx.Done():
		// A result that raced the deadline still wins: it is already the real answer.
		select {
		case o := <-done:
			return o.res, o.err
		default:
		}
		if parent.Err() != nil {
			return workersvc.PublishResult{}, parent.Err()
		}
		return workersvc.PublishResult{}, errPublishServiceDeadline
	}
}
