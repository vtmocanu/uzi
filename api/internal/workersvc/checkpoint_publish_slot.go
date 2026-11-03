package workersvc

import (
	"sync"

	"github.com/google/uuid"
)

const (
	// checkpointPublishMaxConcurrent caps the live checkpoint publishes (POST /publish) in flight
	// process-wide. Each buffers up to 64 MiB of pack (io.ReadAll under maxPackBytes) and holds its
	// connection for up to the extended write deadline, so the worst case is 4 x 64 MiB = 256 MiB,
	// alongside held publication's own 4 x 64 MiB (heldPublishMaxConcurrent), inside the api pod's
	// 1Gi memory limit (deploy/chart/values.yaml).
	checkpointPublishMaxConcurrent = 4

	// checkpointPublishMaxPerOwner is the share one owner (store.Worker.UserID) may hold, so one
	// owner's workers cannot take every slot. Two covers one live publish plus one orphaned publish
	// per run: a request the worker gave up on keeps its slot through the detached retention tail
	// that outlives the request, while the worker's retry is already admitted.
	checkpointPublishMaxPerOwner = 2
)

// checkpointPublishSlots is the admission state of the live checkpoint publish route: the
// process-wide semaphore plus the per-owner held count (the JobFiles.AcquireWrite shape).
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

// AcquireCheckpointPublishSlot takes one live-publish slot for owner without waiting. It is
// refused when the owner already holds its share or every process-wide slot is taken. The caller
// takes it before reading the body and calls release when the request is finished; release is
// idempotent.
func (s *Service) AcquireCheckpointPublishSlot(owner uuid.UUID) (release func(), ok bool) {
	p := s.checkpointPublish
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.held[owner] >= checkpointPublishMaxPerOwner {
		return nil, false
	}
	select {
	case p.slots <- struct{}{}:
	default:
		return nil, false
	}
	p.held[owner]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.held[owner]--; p.held[owner] <= 0 {
				delete(p.held, owner)
			}
			<-p.slots
		})
	}, true
}
