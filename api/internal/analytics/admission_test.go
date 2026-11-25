package analytics

import (
	"errors"
	"testing"
)

func TestAdmissionCapacity(t *testing.T) {
	cases := map[int32]int{1: 1, 2: 1, 3: 1, 4: 1, 8: 2, 20: 5}
	for maxConns, want := range cases {
		if got := NewAdmission(maxConns).Capacity(); got != want {
			t.Errorf("maxConns %d: capacity %d, want %d", maxConns, got, want)
		}
	}
}

func TestAdmissionSingleFlightPerCallerSection(t *testing.T) {
	a := NewAdmission(20)
	rel, err := a.Acquire("u1", SectionSpend)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Acquire("u1", SectionSpend); !errors.Is(err, ErrBusy) {
		t.Fatalf("duplicate key: got %v, want ErrBusy", err)
	}
	rel2, err := a.Acquire("u1", SectionOptions)
	if err != nil {
		t.Fatalf("different section, same caller: %v", err)
	}
	rel3, err := a.Acquire("u2", SectionSpend)
	if err != nil {
		t.Fatalf("different caller: %v", err)
	}
	rel()
	rel2()
	rel3()
	if n := a.heldCount(); n != 0 {
		t.Fatalf("held map not empty: %d", n)
	}
}

func TestAdmissionFullSemaphore(t *testing.T) {
	a := NewAdmission(4) // capacity 1
	rel, err := a.Acquire("u1", SectionSpend)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Acquire("u2", SectionSpend); !errors.Is(err, ErrBusy) {
		t.Fatalf("full semaphore: got %v, want ErrBusy", err)
	}
	rel()
	rel2, err := a.Acquire("u2", SectionSpend)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	rel2()
}

func TestAdmissionReleaseOnPanic(t *testing.T) {
	a := NewAdmission(4)
	func() {
		defer func() { _ = recover() }()
		rel, err := a.Acquire("u1", SectionSpend)
		if err != nil {
			t.Error(err)
			return
		}
		defer rel()
		panic("boom")
	}()
	rel, err := a.Acquire("u1", SectionSpend)
	if err != nil {
		t.Fatalf("re-acquire after panic: %v", err)
	}
	rel()
	if n := a.heldCount(); n != 0 {
		t.Fatalf("held map not empty: %d", n)
	}
}

func TestAdmissionDoubleReleaseSafe(t *testing.T) {
	a := NewAdmission(8) // capacity 2
	rel, err := a.Acquire("u1", SectionSpend)
	if err != nil {
		t.Fatal(err)
	}
	rel()
	other, err := a.Acquire("u1", SectionSpend)
	if err != nil {
		t.Fatal(err)
	}
	rel() // stale second release must not free the new holder's key
	if _, err := a.Acquire("u1", SectionSpend); !errors.Is(err, ErrBusy) {
		t.Fatalf("stale release freed a live key: %v", err)
	}
	other()
	if n := a.heldCount(); n != 0 {
		t.Fatalf("held map not empty: %d", n)
	}
}
