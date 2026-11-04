package workersvc

import (
	"sync"

	"github.com/google/uuid"
)

const (
	// checkpointPublishMaxConcurrent caps the live checkpoint publishes (POST /publish) in flight
	// process-wide. Each buffers up to 64 MiB of pack (io.ReadAll under maxPackBytes), and the push
	// broker adds a spike of roughly 194 MB RSS per publish (measured on this repo; it scales with the
	// target repo's shallow base snapshot, not with pack size; see the api memory limit in
	// deploy/chart/values.yaml). Four concurrent publishes therefore sit at about the 1Gi limit on top
	// of the roughly 280Mi steady state before counting pack bytes; larger packs, larger repos or
	// concurrent held publications (heldPublishMaxConcurrent) can exceed it. The cap bounds that worst
	// case where it was previously unbounded; it does not fit it under the limit.
	checkpointPublishMaxConcurrent = 4

	// checkpointPublishMaxPerWorker is the share one worker (store.Worker.ID) may hold, so one
	// worker token cannot take every slot. Two covers a live publish plus a request the worker has
	// already abandoned whose server-side work still holds its slot: the service call is
	// synchronous, including the detached retention tail inside PublishAttested, which outlives
	// the worker's abort but not the server request. That is enough for the default single-run
	// worker. A multi-run worker exceeding it gets a 429, which the worker retries within its
	// allowance. Keying on the worker (not the owner, whose id every hosted worker of a run carries)
	// gives up fairness between owners: one owner with two busy workers can hold all four
	// process-wide slots, and other owners then get 429s bounded only by the global cap and their
	// retry.
	checkpointPublishMaxPerWorker = 2
)

// checkpointPublishSlots is the admission state of the live checkpoint publish route: the
// process-wide semaphore plus the per-worker held count (the JobFiles.AcquireWrite shape).
type checkpointPublishSlots struct {
	mu    sync.Mutex
	held  map[uuid.UUID]int
	slots chan struct{}
}

func newCheckpointPublishSlots() *checkpointPublishSlots {
	return &checkpointPublishSlots{
		held:  map[uuid.UUID]int{},
		slots: make(chan struct{}, checkpointPublishMaxConcurrent),
	}
}

// AcquireCheckpointPublishSlot takes one live-publish slot for worker without waiting. It is
// refused when the worker already holds its share or every process-wide slot is taken. The caller
// takes it before reading the body and calls release when the request is finished; release is
// idempotent.
func (s *Service) AcquireCheckpointPublishSlot(worker uuid.UUID) (release func(), ok bool) {
	p := s.checkpointPublish
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held[worker] >= checkpointPublishMaxPerWorker {
		return nil, false
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return nil, false
	}
	p.held[worker]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.held[worker]--; p.held[worker] <= 0 {
				delete(p.held, worker)
			}
			<-p.slots
		})
	}, true
}
