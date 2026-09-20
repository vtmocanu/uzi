package healthsvc

import (
	"sync"
	"time"
)

// BeatRegistry is the in-process record of "when did each background loop last tick"
// (PRD #1484 M2, D8). It is a LEAF: no loop package imports healthsvc — main.go builds
// ONE registry, Registers each loop it starts with that loop's interval, and hands each
// loop a plain `func(){ reg.Beat("<name>") }` callback. The `loops` health check reads
// Snapshot() to classify each registered loop against its interval.
//
// A loop that is NOT started on this deployment (the conditional scheduler) is simply
// never Registered, so it is absent from the snapshot and left out of the loops evidence
// entirely (D15) — never counted as missing.
//
// It is correct for a single api replica (the chart default, api.replicaCount: 1). With
// several replicas the check describes the answering process only; persisting beats is a
// follow-up if that ever matters (D8).
type BeatRegistry struct {
	now func() time.Time

	mu    sync.Mutex
	loops map[string]*loopEntry
	order []string // registration order, so Snapshot is stable
}

// loopEntry is one loop's mutable beat record.
type loopEntry struct {
	interval     time.Duration
	registeredAt time.Time
	lastBeat     time.Time // zero until the loop's first Beat
}

// LoopBeat is a read-only snapshot of one registered loop's beat record. The loops check
// classifies each against its interval: danger past 10 intervals, warn past 3, unknown
// for a loop that has not beaten since registration and is younger than 3 intervals.
type LoopBeat struct {
	Name         string
	Interval     time.Duration
	LastBeat     time.Time // zero when the loop has not beaten since registration
	RegisteredAt time.Time
}

// NewBeatRegistry builds an empty registry. now is the clock seam (nil defaults to
// time.Now) shared with the Service so RegisteredAt/LastBeat and the loops check read one
// clock.
func NewBeatRegistry(now func() time.Time) *BeatRegistry {
	if now == nil {
		now = time.Now
	}
	return &BeatRegistry{now: now, loops: make(map[string]*loopEntry)}
}

// Register records a loop under name with its tick interval, stamping RegisteredAt from
// the clock. Registering the same name again is idempotent (it keeps the first
// registration), so a double-wire cannot reset the baseline.
func (r *BeatRegistry) Register(name string, interval time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.loops[name]; ok {
		return
	}
	r.loops[name] = &loopEntry{interval: interval, registeredAt: r.now()}
	r.order = append(r.order, name)
}

// Beat records that the named loop just ticked. Nil-safe and unknown-name-safe: a Beat
// for a loop that was never Registered is dropped, so a mis-wired callback cannot panic.
func (r *BeatRegistry) Beat(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.loops[name]; ok {
		e.lastBeat = r.now()
	}
}

// Snapshot returns the registered loops in registration order. The returned slice is a
// copy, safe to read after the lock is released.
func (r *BeatRegistry) Snapshot() []LoopBeat {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]LoopBeat, 0, len(r.order))
	for _, name := range r.order {
		e := r.loops[name]
		out = append(out, LoopBeat{
			Name:         name,
			Interval:     e.interval,
			LastBeat:     e.lastBeat,
			RegisteredAt: e.registeredAt,
		})
	}
	return out
}
