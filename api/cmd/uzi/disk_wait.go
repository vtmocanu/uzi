package main

import "github.com/vtmocanu/uzi/api/internal/apitypes"

type diskWaitState uint8

const (
	diskWaitNone diskWaitState = iota
	diskWaitUnknown
	diskWaitTimed
	diskWaitHeld
)

// diskWaitOf uses the existing wire-presence bit: an omitted field is unknown,
// explicit NULL is an owner hold, and a valid stamp stays timed even when overdue.
func diskWaitOf(r apitypes.RunDTO) diskWaitState {
	if !isDiskFullPark(r) {
		return diskWaitNone
	}
	if retry := r.RecoveryRetryNotBefore; retry != nil {
		if retry.IsZero() || retry.Year() < 1 || retry.Year() > 9999 {
			return diskWaitUnknown
		}
		return diskWaitTimed
	}
	if r.RecoveryRetryNotBeforePresent {
		return diskWaitHeld
	}
	return diskWaitUnknown
}

func isDiskOwnerHold(r apitypes.RunDTO) bool {
	return diskWaitOf(r) == diskWaitHeld
}

func diskHoldLines(r apitypes.RunDTO) []string {
	if !isDiskOwnerHold(r) {
		return nil
	}
	id := cellText(r.ID)
	return []string{
		"Owner Resume: uzi run resume " + id + "; Cancel: uzi run cancel " + id,
		"Resume requests one further ordinary attempt; counters persist; admission may refuse.",
		"Local unpublished work is not guaranteed on worker loss.",
	}
}
