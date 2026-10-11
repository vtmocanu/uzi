// Package runprogress derives a run's progress estimate (PRD #2602): a percent
// from the frozen milestone list, or a state flag when a percent would mislead.
// It is pure (no I/O): runToDTO feeds it the decoded run row, and the handler
// sets the phase from the run's current_activity role afterwards.
package runprogress

import (
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// State values of apitypes.RunProgress.State (closed set; a consumer must treat
// an unknown value as "none").
const (
	StatePercent  = "percent"
	StateWaiting  = "waiting"
	StateParked   = "parked"
	StateQueued   = "queued"
	StateStalled  = "stalled"
	StatePlanning = "planning"
	StateNone     = "none"
)

// Phase values of apitypes.RunProgress.Phase; empty when no activity is known.
const (
	PhaseImplement = "implement"
	PhaseReview    = "review"
	PhaseValidate  = "validate"
)

// Input is the subset of a run Derive reads. A nil and an empty FrozenIDs both
// mean "no measurable plan" and derive "none".
type Input struct {
	Kind       string
	Status     string
	Health     string
	IsPlanning bool
	// FrozenIDs are the frozen milestone ids in frozen order.
	FrozenIDs  []string
	Completed  []string
	InProgress []string
}

// Derive applies the PRD rules, first match wins. It returns nil for a
// terminal run. Phase is left empty; the caller sets it with Phase.
func Derive(in Input) *apitypes.RunProgress {
	if apitypes.IsTerminalRunStatus(in.Status) {
		return nil
	}
	done, total, active := count(in)
	p := &apitypes.RunProgress{
		MilestoneDone:     done,
		MilestoneTotal:    total,
		ActiveMilestoneID: active,
	}
	switch {
	case in.Status == "awaiting_approval" || in.Status == "awaiting_input" || in.Status == "awaiting_followup":
		p.State = StateWaiting
	case in.Status == "limit_wait" || in.Status == "pool_wait" || in.Status == "recovery_wait" || in.Status == "paused":
		p.State = StateParked
	case in.Status == "queued" || in.Status == "claimed":
		p.State = StateQueued
	case in.Health == "stalled" || in.Health == "looping":
		p.State = StateStalled
	case in.IsPlanning:
		p.State = StatePlanning
	case in.Kind != runkind.Issue || total == 0:
		p.State = StateNone
	case !knownLive(in.Status):
		// A live status this build does not know: show no number rather than guess.
		p.State = StateNone
	default:
		p.State = StatePercent
		pct := min(99, (11*total+89*done)/total)
		p.Pct = &pct
	}
	return p
}

// knownLive reports the non-terminal statuses that reach the percent rule:
// running only, since every other known live status matched an earlier rule.
func knownLive(status string) bool { return status == "running" }

// count mirrors api/cmd/uzi milestoneProgress and milestoneInProgress: done
// counts frozen members present in the completed set (ids outside the frozen
// list are ignored); active is the first frozen id that is in progress and not
// completed.
func count(in Input) (done, total int, active string) {
	total = len(in.FrozenIDs)
	if total == 0 {
		return 0, 0, ""
	}
	completed := make(map[string]bool, len(in.Completed))
	for _, id := range in.Completed {
		completed[id] = true
	}
	inProg := make(map[string]bool, len(in.InProgress))
	for _, id := range in.InProgress {
		inProg[id] = true
	}
	for _, id := range in.FrozenIDs {
		if completed[id] {
			done++
		} else if active == "" && inProg[id] {
			active = id
		}
	}
	return done, total, active
}

// Phase maps a run's current-activity role to a phase. The role is untrusted,
// model-influenced text: it is only compared against these closed sets, never
// echoed.
func Phase(agent string) string {
	switch agent {
	case "":
		return ""
	case "reviewer", "auditor", "fact-checker", "architect", "web-ux", "tui-ux", "dba":
		return PhaseReview
	case "tester":
		return PhaseValidate
	default:
		return PhaseImplement
	}
}
