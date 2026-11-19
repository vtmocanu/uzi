package providers

import (
	"sync"
	"time"
)

// Probe admission limits (PRD #4320 section 8): at most one probe in flight per owner, four
// instance-wide, and 30 per owner per sliding hour. The hourly count is keyed by owner id, so
// creating and retiring providers does not reset it. State is in memory: a restart clears it,
// which only ever loosens the limits for one hour.
const (
	probeMaxInFlight        = 4
	probeMaxPerOwnerPerHour = 30
	probeWindow             = time.Hour

	// Retry-After hints for the two in-flight refusals; the hourly refusal computes its own.
	probeBusyRetryAfter = 20 * time.Second
)

// Reasons a probe is refused admission.
const (
	LimitOwnerBusy = "owner_busy"
	LimitBusy      = "instance_busy"
	LimitHourly    = "hourly"
)

// ProbeLimiter admits probes. The zero value is not usable; use NewProbeLimiter.
type ProbeLimiter struct {
	mu       sync.Mutex
	now      func() time.Time
	inflight int
	owners   map[string]*ownerProbeState
}

type ownerProbeState struct {
	running bool
	starts  []time.Time // sliding window of admitted probe starts, oldest first
}

// NewProbeLimiter returns a limiter on the real clock.
func NewProbeLimiter() *ProbeLimiter {
	return &ProbeLimiter{now: time.Now, owners: map[string]*ownerProbeState{}}
}

// Acquire admits one probe for owner. On success release must be called exactly once when the probe
// ends (it is idempotent). On refusal ok is false, reason is one of the Limit* constants and
// retryAfter is a hint for the Retry-After header.
func (l *ProbeLimiter) Acquire(owner string) (release func(), reason string, retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.owners[owner]
	if st == nil {
		st = &ownerProbeState{}
	}
	st.prune(now)
	switch {
	case st.running:
		return nil, LimitOwnerBusy, probeBusyRetryAfter, false
	case l.inflight >= probeMaxInFlight:
		return nil, LimitBusy, probeBusyRetryAfter, false
	case len(st.starts) >= probeMaxPerOwnerPerHour:
		wait := st.starts[0].Add(probeWindow).Sub(now)
		return nil, LimitHourly, max(wait, time.Second), false
	}
	st.running = true
	st.starts = append(st.starts, now)
	l.inflight++
	l.owners[owner] = st
	l.sweep(now)

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.inflight--
			if s := l.owners[owner]; s != nil {
				s.running = false
			}
		})
	}, "", 0, true
}

func (s *ownerProbeState) prune(now time.Time) {
	cut := now.Add(-probeWindow)
	i := 0
	for i < len(s.starts) && !s.starts[i].After(cut) {
		i++
	}
	s.starts = s.starts[i:]
}

// sweep drops owners with nothing running and an empty window, so the map does not grow with every
// owner that ever probed. It is cheap enough to run on every admission at the sizes involved.
func (l *ProbeLimiter) sweep(now time.Time) {
	if len(l.owners) < 256 {
		return
	}
	for k, s := range l.owners {
		s.prune(now)
		if !s.running && len(s.starts) == 0 {
			delete(l.owners, k)
		}
	}
}
