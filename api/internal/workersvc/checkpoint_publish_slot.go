package workersvc

import (
	"sync"

	"github.com/google/uuid"
)

const (
	// checkpointPublishMaxConcurrent caps the live checkpoint publishes (POST /publish) in flight
	// process-wide. Worst case per publish is about 64 MiB of buffered pack (io.ReadAll under
	// maxPackBytes) plus the roughly 194 MB RSS spike of the push broker documented beside the api
	// memory limit in deploy/chart/values.yaml. Four concurrent is about 1 GB on top of the
	// roughly 280Mi steady state, which can exceed the 1Gi limit only when all four carry
	// near-maximal packs at once; typical checkpoint packs are far smaller. The cap makes that
	// worst case bounded where it was previously unbounded. Held publication has its own cap
	// (heldPublishMaxConcurrent).
	checkpointPublishMaxConcurrent = 4

	// checkpointPublishMaxPerWorker is the share one worker (store.Worker.ID) may hold, so one
	// worker token cannot take every slot. Two covers a live publish plus a request the worker has
	// already abandoned whose server-side work still holds its slot: the service call is
	// synchronous, including the detached retention tail inside PublishAttested, which outlives
	// the worker's abort but not the server request. That is enough for the default single-run
	// worker. A multi-run worker exceeding it gets a 429, which the worker retries within its
	// allowance.
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
