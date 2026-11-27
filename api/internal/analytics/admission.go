// Package analytics holds the pure (stdlib-only) pieces of the scoped usage
// analytics API (#4684): the admission gate, the Go-authoritative calendar
// (decision D-A) and timezone validation. It must not import api/internal/store.
package analytics

import (
	"errors"
	"sync"
)

// Section names used as the second half of the admission key. The owner and
// admin variants of one section share a key (#4684 plan, admission.go).
const (
	SectionOptions     = "options"
	SectionSpend       = "spend"
	SectionReliability = "reliability"
	SectionCapacity    = "capacity"
	SectionQuality     = "quality"
	SectionFlow        = "flow"
)

// ErrBusy is returned by Acquire when the (caller, section) key is already
// held or every admission slot is taken.
var ErrBusy = errors.New("analytics: busy")

type admissionKey struct {
	caller  string
	section string
}

// Admission is a non-blocking semaphore with single-flight per
// (caller, section). Its capacity is max(1, maxConns/4). The held-key map
// doubles as the semaphore, so no idle entries can remain after release.
type Admission struct {
	mu   sync.Mutex
	cap  int
	held map[admissionKey]struct{}
}

// NewAdmission builds an Admission for a pool of maxConns connections.
func NewAdmission(maxConns int32) *Admission {
	c := int(maxConns / 4)
	if c < 1 {
		c = 1
	}
	return &Admission{cap: c, held: make(map[admissionKey]struct{})}
}

// Capacity reports the maximum number of concurrent holders.
func (a *Admission) Capacity() int { return a.cap }

// Acquire never blocks. It returns a release func or ErrBusy. The release
// func is idempotent; callers must defer it so a panic still releases.
func (a *Admission) Acquire(caller, section string) (release func(), err error) {
	k := admissionKey{caller: caller, section: section}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.held[k]; ok {
		return nil, ErrBusy
	}
	if len(a.held) >= a.cap {
		return nil, ErrBusy
	}
	a.held[k] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.held, k)
			a.mu.Unlock()
		})
	}, nil
}

// heldCount reports the number of keys currently held (tests).
func (a *Admission) heldCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.held)
}
