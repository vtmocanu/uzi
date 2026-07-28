package main

import (
	"encoding/json"
	"time"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// Agent lanes and the per-lane state dot (PRD #112 D5), ported from the web so the
// terminal and the browser cannot drift into two different readings of one run.
//
// The port is of TWO separate pure functions that D5's prose conflates:
// crewStateFor (web/src/components/ActivityFeed.tsx:209-245) produces the DOT and
// takes no frame kind at all, while agentOneLiner (:279-310) produces the one-liner
// TEXT. This file ports the dot; the transcript renders its own text.

// LEAD is the orchestrator's role name and the lane key of last resort.
const laneLead = "lead"

// crewState is the five-value lane dot. D5's PRD text lists four and omits stalled,
// which is the PRD #47 health integration and the only state that means "attention".
type crewState string

const (
	crewWorking crewState = "working"
	crewStalled crewState = "stalled"
	crewWaiting crewState = "waiting"
	crewIdle    crewState = "idle"
	crewDone    crewState = "done"
)

// laneStaleAfter governs ONLY the non-active waiting↔idle split, never the active
// speaker (ActivityFeed.tsx:216, STALE_MS). The server's stall flag defaults to 300s,
// so a healthy tool call lasting 45s-300s must still read `working`; applying this to
// the active lane would flip every long healthy tool call to idle.
const laneStaleAfter = 45 * time.Second

// stalledHealth are the PRD #47 WARN flags that turn an ACTIVE lane amber rather than
// green. A looping agent is spinning without progress and must never read as healthy.
var stalledHealth = map[string]bool{"stalled": true, "slow": true, "looping": true}

// laneStatePriority is the rollup order for a role's single dot: WORST state wins, not
// newest (ActivityFeed.tsx:262-268). One stalled tester must surface over a working one.
var laneStatePriority = map[crewState]int{
	crewStalled: 0, crewWaiting: 1, crewWorking: 2, crewIdle: 3, crewDone: 4,
}

// laneFrame is the minimal shape the lane logic needs, so the classifiers are pure and
// testable without a socket or a DTO pointer dance. Both a replayed MessageDTO and a
// live RunEventDTO convert into it.
//
// Absent pointer fields collapse to "", and that is FAITHFUL rather than lossy here:
// every lane function below uses JavaScript `||` semantics, which treat "" and null
// identically. The web's own comment (ActivityFeed.tsx:75-76) requires exactly this —
// `??` would let an empty-string instance survive as a lane key, which is the bug. So
// "" is the correct Go representation for these three fields, and a future reader
// should not "fix" it into pointers.
type laneFrame struct {
	Seq           int32
	Kind          string
	Agent         string
	AgentInstance string
	AgentLabel    string
	Payload       json.RawMessage
	CreatedAt     time.Time
}

func laneFrameFromMessage(m apitypes.MessageDTO) laneFrame {
	return laneFrame{
		Seq: m.Seq, Kind: m.Kind,
		Agent:         strOrEmpty(m.Agent),
		AgentInstance: strOrEmpty(m.AgentInstance),
		AgentLabel:    strOrEmpty(m.AgentLabel),
		Payload:       m.Payload,
		CreatedAt:     m.CreatedAt,
	}
}

func laneFrameFromEvent(ev apitypes.RunEventDTO) laneFrame {
	f := laneFrame{
		Seq: ev.Seq, Kind: ev.Kind,
		Agent:         strOrEmpty(ev.Agent),
		AgentInstance: strOrEmpty(ev.AgentInstance),
		AgentLabel:    strOrEmpty(ev.AgentLabel),
		Payload:       ev.Payload,
	}
	if ev.CreatedAt != nil {
		f.CreatedAt = *ev.CreatedAt
	}
	return f
}

func strOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// laneKeyOf is the lane identity: the invocation, else the role, else lead
// (ActivityFeed.tsx:77-79). The fallback chain is `||`, never `??` — an empty-string
// instance must fall THROUGH to the role rather than become a key of its own. A NULL
// instance and an empty one therefore coalesce, which is intended: pre-migration rows
// and the orchestrator's own turns both land in one lane per role.
func laneKeyOf(f laneFrame) string {
	if f.AgentInstance != "" {
		return f.AgentInstance
	}
	if f.Agent != "" {
		return f.Agent
	}
	return laneLead
}

// laneRole titles a lane from the first frame naming a NON-lead role, not from the
// lane's first frame (ActivityFeed.tsx:129-133).
//
// Why: an SDK replay frame carries parent_tool_use_id but no subagent_type, and the
// worker's `subagent_type ?? "lead"` fallback stores the literal string "lead" on it —
// so a RESUMED subagent's lane can open with a lead-looking frame and would otherwise
// be titled `lead` while holding a coder's tool rail (PRD #99 Decision 8). Testing for
// an absent agent instead would NOT catch it: the field is already collapsed to "lead"
// upstream in agent/src/sdk-messages.ts, never to null.
//
// The fallback is the lane's FIRST role and may legitimately BE lead: a repo can ship
// .claude/agents/lead.md, which registers as an ordinary invocable subagent, so an
// all-lead lane can be a real invocation. That is why this cannot be "first non-lead
// role, or bust".
//
// NOTE for anyone mutation-testing this, carried over from the web source so nobody
// re-derives it: the final fallback is PROVABLY EQUIVALENT to a bare `return laneLead`,
// so that mutant cannot be killed and its survival is NOT a coverage gap. If control
// reaches it, every frame satisfied `Agent == "" || Agent == laneLead`, so frames[0].Agent
// is "" or "lead" — and hub.PublishMessage omits an empty agent from the frame while the
// REST path stores "" as SQL NULL, so the "" case cannot arrive from the wire. What IS
// pinned is the fallback's VALUE: returning "unknown" or "" instead breaks the roster.
// The expression stays because it states the Decision 8 intent and remains correct if
// the wire ever carries a third case.
func laneRole(frames []laneFrame) string {
	for _, f := range frames {
		if f.Agent != "" && f.Agent != laneLead {
			return f.Agent
		}
	}
	if len(frames) > 0 && frames[0].Agent != "" {
		return frames[0].Agent
	}
	return laneLead
}

// laneLabel is INDEPENDENT of laneRole (PRD #99 Decision 1/3): the label is a separate
// column that can be absent for reasons unrelated to the role, so the first frame
// carrying one wins and a lane with none renders the role alone — no placeholder.
func laneLabel(frames []laneFrame) string {
	for _, f := range frames {
		if f.AgentLabel != "" {
			return f.AgentLabel
		}
	}
	return ""
}

// laneLabelCap is the display clamp for a model-authored label, in RUNES.
//
// It is the SERVER's 80-rune storage cap, not the web's 48 UTF-16 code units. The web's
// own comment concedes its clamp can split an astral pair; that is a defect, not a
// contract, so the Go port does not replicate it. Clamping runes cannot split a
// codepoint at all.
const laneLabelCap = 80

// agentLane is one actor's whole contribution to a run, keyed by invocation.
type agentLane struct {
	Key    string
	Role   string
	Label  string
	Frames []laneFrame
	// LastActivity is the newest CreatedAt in the lane, which drives the non-active
	// waiting↔idle split.
	LastActivity time.Time
}

// buildLanes groups frames into lanes in first-seen order, which keeps the rail stable
// as frames arrive rather than reshuffling on every update.
func buildLanes(frames []laneFrame) []agentLane {
	byKey := map[string][]laneFrame{}
	order := []string{}
	for _, f := range frames {
		k := laneKeyOf(f)
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], f)
	}
	lanes := make([]agentLane, 0, len(order))
	for _, k := range order {
		fs := byKey[k]
		var last time.Time
		for _, f := range fs {
			if f.CreatedAt.After(last) {
				last = f.CreatedAt
			}
		}
		lanes = append(lanes, agentLane{
			Key: k, Role: laneRole(fs), Label: laneLabel(fs),
			Frames: fs, LastActivity: last,
		})
	}
	return lanes
}

// activeLaneKey is the lane of the newest frame while the run is LIVE, and live here
// includes `claimed`, not just `running` (ActivityFeed.tsx:443).
//
// This is the trap D5 does not mention: the running-only variant exists too
// (activeLane, :449) but drives the tool SPINNER, not the dot. Using running-only here
// makes every lane of a claimed run read `idle`.
func activeLaneKey(runStatus string, frames []laneFrame) string {
	if runStatus != "running" && runStatus != "claimed" {
		return ""
	}
	if len(frames) == 0 {
		return ""
	}
	return laneKeyOf(frames[len(frames)-1])
}

// crewStateFor is the D5 ladder (ActivityFeed.tsx:209-245). Precedence, in order:
// terminal → gate/worker-wait → active speaker (health, NOT recency) → non-active
// recency split.
//
// actor is an OPAQUE identity compared only for equality against activeActor: a lane
// key here, a role name in the rollup. The ladder itself does not care which.
func crewStateFor(runStatus, runHealth, actor, activeActor string, lastActivity, now time.Time) crewState {
	if isTerminalRunStatus(runStatus) {
		return crewDone
	}
	// A gate or a missing worker blocks the WHOLE crew, so it dominates every lane.
	// awaiting_input (PRD #88) is the same shape of block — a human owes the run an
	// action and nothing proceeds until they take it — so it belongs here rather than
	// in atPlanGate, which is specifically the PLAN gate.
	if runStatus == "awaiting_approval" || runStatus == "awaiting_input" || runHealth == "waiting_worker" {
		return crewWaiting
	}
	if activeActor != "" && actor == activeActor {
		// The active speaker trusts run.health and NEVER the recency timer.
		if stalledHealth[runHealth] {
			return crewStalled
		}
		return crewWorking
	}
	// Non-active: recency is the only signal available for "handed off" vs "quiet"
	// (there is no SDK handoff event), and it is a cosmetic split that never reaches
	// the exact states above.
	if now.Sub(lastActivity) < laneStaleAfter {
		return crewWaiting
	}
	return crewIdle
}

// isTerminalRunStatus DELEGATES to uzicli rather than restating the set.
//
// It used to hold its own literal list "so this file's ladder reads against one literal
// list" — which bought local readability and paid for it with a third definition of the
// terminal statuses (run.go's terminalRunStatuses map, uzicli.IsTerminalRunStatus, and
// this) with nothing pinning them equal. That is precisely the drift the package-main
// layout decision made a REVIEW property rather than a compile property, so the answer
// is to call the exported one that is already in scope, not to add a fourth test.
func isTerminalRunStatus(status string) bool {
	return uzicli.IsTerminalRunStatus(status)
}

// worstLaneState is the rollup dot for a group of lanes: the WORST state wins, so one
// stalled lane surfaces over a working one (ActivityFeed.tsx:262-268).
func worstLaneState(states []crewState) crewState {
	if len(states) == 0 {
		return crewIdle
	}
	worst := states[0]
	for _, s := range states[1:] {
		if laneStatePriority[s] < laneStatePriority[worst] {
			worst = s
		}
	}
	return worst
}

// laneDot is the single glyph the rail draws for a state. Kept next to the state table
// so a new state cannot be added without choosing one.
func laneDot(s crewState) string {
	switch s {
	case crewWorking:
		return "●"
	case crewStalled:
		return "▲"
	case crewWaiting:
		return "◐"
	case crewDone:
		return "✓"
	default:
		return "○"
	}
}
