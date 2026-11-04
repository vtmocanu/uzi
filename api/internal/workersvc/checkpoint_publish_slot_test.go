package workersvc

import (
	"testing"

	"github.com/google/uuid"
)

func newSlotService() *Service { return New(nil, nil, Params{}) }

func TestCheckpointPublishSlotGlobalCap(t *testing.T) {
	s := newSlotService()
	var rels []func()
	for i := 0; i < 4; i++ { // distinct workers, so only the global cap can refuse
		rel, ok := s.AcquireCheckpointPublishSlot(uuid.New())
		if !ok {
			t.Fatalf("slot %d refused below the global cap", i)
		}
		rels = append(rels, rel)
	}
	if _, ok := s.AcquireCheckpointPublishSlot(uuid.New()); ok {
		t.Fatal("a fifth slot was granted past the global cap")
	}
	rels[0]()
	rel, ok := s.AcquireCheckpointPublishSlot(uuid.New())
	if !ok {
		t.Fatal("a released slot was not reusable")
	}
	rel()
	for _, r := range rels[1:] {
		r()
	}
}

func TestCheckpointPublishSlotPerWorkerCap(t *testing.T) {
	s := newSlotService()
	worker := uuid.New()
	r1, ok1 := s.AcquireCheckpointPublishSlot(worker)
	r2, ok2 := s.AcquireCheckpointPublishSlot(worker)
	if !ok1 || !ok2 {
		t.Fatal("worker refused below its share")
	}
	if _, ok := s.AcquireCheckpointPublishSlot(worker); ok {
		t.Fatal("worker got a third slot past its share")
	}
	// The global cap still has room, so another worker is admitted.
	other, ok := s.AcquireCheckpointPublishSlot(uuid.New())
	if !ok {
		t.Fatal("a second worker was refused while the first sat at its share")
	}
	other()
	r1()
	r3, ok := s.AcquireCheckpointPublishSlot(worker)
	if !ok {
		t.Fatal("worker not admitted after releasing one slot")
	}
	r3()
	r2()
}

func TestCheckpointPublishSlotReleaseIdempotent(t *testing.T) {
	s := newSlotService()
	worker := uuid.New()
	rel, ok := s.AcquireCheckpointPublishSlot(worker)
	if !ok {
		t.Fatal("refused")
	}
	keep, ok := s.AcquireCheckpointPublishSlot(worker)
	if !ok {
		t.Fatal("refused")
	}
	rel()
	rel()
	rel() // a double release must not free keep's slot
	again, ok := s.AcquireCheckpointPublishSlot(worker)
	if !ok {
		t.Fatal("worker not admitted after one release")
	}
	if _, ok := s.AcquireCheckpointPublishSlot(worker); ok {
		t.Fatal("double release gave the worker more than its share")
	}
	again()
	keep()
}

func TestCheckpointPublishSlotMapEntryCleared(t *testing.T) {
	s := newSlotService()
	worker := uuid.New()
	rel, _ := s.AcquireCheckpointPublishSlot(worker)
	rel2, _ := s.AcquireCheckpointPublishSlot(worker)
	rel()
	rel2()
	p := s.checkpointPublish
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.held) != 0 || len(p.slots) != 0 {
		t.Fatalf("state left behind: held=%v slots=%d", p.held, len(p.slots))
	}
}
