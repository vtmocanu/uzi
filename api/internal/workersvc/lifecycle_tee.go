package workersvc

import "github.com/google/uuid"

// lifecycleTee fans one Notify out to several RunLifecycle members.
type lifecycleTee []RunLifecycle

func (t lifecycleTee) Notify(runID uuid.UUID, status string) {
	for _, l := range t {
		l.Notify(runID, status)
	}
}

// TeeLifecycle returns a RunLifecycle that forwards every Notify to each non-nil member, in
// argument order. A nil member (including a nil interface) is skipped, so a caller may pass
// an optional hook unconditionally. With no non-nil member the result is a no-op lifecycle,
// never nil, so SetLifecycle(TeeLifecycle()) behaves like "no automation" without a nil
// check at the call site. Each member's Notify is best-effort by the RunLifecycle contract
// (it must not block or error), so one member cannot stall the others.
func TeeLifecycle(ls ...RunLifecycle) RunLifecycle {
	var out lifecycleTee
	for _, l := range ls {
		if l != nil {
			out = append(out, l)
		}
	}
	return out
}
