package workersvc

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHeldPublishSlotsWorkerAdmission(t *testing.T) {
	svc := New(nil, nil, Params{})
	first := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	second := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	granted := 0
	// Keep accepted reservations outstanding, as while a body is still arriving.
	for i := 0; i < heldPublishMaxConcurrent; i++ {
		release, ok := svc.AcquireHeldPublishSlot(first.ID)
		if ok {
			granted++
			t.Cleanup(release)
		}
	}
	if granted != 1 {
		t.Errorf("first worker received %d reservations, want 1", granted)
	}
	release, ok := svc.AcquireHeldPublishSlot(second.ID)
	if !ok {
		t.Fatal("another worker has no remaining upload capacity")
	}
	t.Cleanup(release)
}

func TestHeldPublishSlotsCapacityRetry(t *testing.T) {
	svc := New(nil, nil, Params{})
	releases := make([]func(), 0, heldPublishMaxConcurrent)
	for i := 0; i < heldPublishMaxConcurrent; i++ {
		release, ok := svc.AcquireHeldPublishSlot(uuid.New())
		if !ok {
			t.Fatalf("slot %d refused below the global cap", i)
		}
		releases = append(releases, release)
		t.Cleanup(release)
	}
	waiting := uuid.New()
	if release, ok := svc.AcquireHeldPublishSlot(waiting); ok {
		release()
		t.Fatal("slot granted over the global cap")
	}
	releases[0]()
	release, ok := svc.AcquireHeldPublishSlot(waiting)
	if !ok {
		t.Fatal("a capacity refusal retained the worker reservation")
	}
	t.Cleanup(release)
}

func TestHeldPublishSlotsReleaseIdempotent(t *testing.T) {
	svc := New(nil, nil, Params{})
	worker := uuid.New()
	release, ok := svc.AcquireHeldPublishSlot(worker)
	if !ok {
		t.Fatal("initial slot refused")
	}
	var group sync.WaitGroup
	for i := 0; i < 32; i++ {
		group.Add(1)
		go func() { defer group.Done(); release() }()
	}
	group.Wait()
	current, ok := svc.AcquireHeldPublishSlot(worker)
	if !ok {
		t.Fatal("worker reservation not released")
	}
	t.Cleanup(current)
	release() // A late release must not remove the worker's new reservation.
	if unexpected, ok := svc.AcquireHeldPublishSlot(worker); ok {
		unexpected()
		t.Fatal("late release removed the current reservation")
	}
}

func TestHeldPublishSlotsConcurrentWorker(t *testing.T) {
	svc := New(nil, nil, Params{})
	worker := uuid.New()
	start, finish := make(chan struct{}), make(chan struct{})
	done := sync.OnceFunc(func() { close(finish) })
	t.Cleanup(done)
	var probes, completed sync.WaitGroup
	var accepted atomic.Int32
	for i := 0; i < 32; i++ {
		probes.Add(1)
		completed.Add(1)
		go func() {
			defer completed.Done()
			<-start
			release, ok := svc.AcquireHeldPublishSlot(worker)
			if ok {
				accepted.Add(1)
			}
			probes.Done()
			if ok {
				<-finish
				release()
			}
		}()
	}
	close(start)
	probes.Wait()
	if accepted.Load() != 1 {
		t.Errorf("concurrent requests received %d reservations, want 1", accepted.Load())
	}
	other, ok := svc.AcquireHeldPublishSlot(uuid.New())
	if !ok {
		t.Error("another worker cannot reserve available capacity")
	} else {
		other()
	}
	done()
	completed.Wait()
	release, ok := svc.AcquireHeldPublishSlot(worker)
	if !ok {
		t.Fatal("worker reservation remains after all requests finished")
	}
	release()
}
