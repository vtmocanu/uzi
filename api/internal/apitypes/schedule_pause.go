package apitypes

import "time"

// SchedulePauseDTO is the user-level pause-all state (PRD #1093 D7): the response of
// GET/PUT/DELETE /api/schedules/pause. Paused is the NORMALIZED live decision (an
// expired Until reads paused:false). Until is the auto-resume instant, or nil for an
// indefinite pause ("until I resume") — nullable, NOT omitempty, so the wire always
// carries the key (the SPA and the contract's zero-marshal rely on it).
type SchedulePauseDTO struct {
	Paused bool       `json:"paused"`
	Until  *time.Time `json:"until"`
}
