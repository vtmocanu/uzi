// Package runactivity derives a run's "now" line (PRD #1064 D3): the run's newest
// tool_use frame folded into an apitypes.RunActivity. It is the ONE copy of the rule,
// linked by the api handler (via a batched DISTINCT ON query) and by the TUI, and
// mirrored in TS (web/src/lib/runActivity.ts) for the run view. The Go and TS copies
// are pinned against each other by fixtures/run-activity/cases.json, asserted from
// both modules.
//
// It is a stdlib-only leaf: it imports apitypes (the wire type it returns) and nothing
// else outside the standard library, so the CLI leaf that links it stays free of the
// server's build graph. The terminal-safety predicate it applies is therefore inlined
// from api/internal/termsafe.Unsafe rather than imported (that package is not
// reachable from a stdlib-only leaf), and TestUnsafeMatchesTermsafe over every rune in
// runactivity_test.go pins the two spellings together.
package runactivity

import (
	"encoding/json"
	"time"
	"unicode"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// detailCapRunes is the rune cap applied to the two model-authored display fields
// (Detail and AgentLabel), matching workersvc.sanitizePlanChangedLine's cap. It is a
// rune count, not a byte count, so a multibyte-heavy label is not shortened harder
// than an ASCII one and the cut never lands inside a rune.
const detailCapRunes = 200

// Frame is one persisted run message, the subset runactivity needs to select and fold
// the newest tool_use. It mirrors the store.RunMessage / apitypes.MessageDTO columns
// the rule reads: the kind, the acting agent and its label (both nullable), the raw
// per-kind payload, the created_at instant and the per-run seq. The handler builds
// these from the batched query; the TUI builds them from its own frames.
type Frame struct {
	Kind       string
	Agent      *string
	AgentLabel *string
	Payload    json.RawMessage
	CreatedAt  time.Time
	Seq        int32
}

// Latest folds the newest tool_use frame in frames via FromFrame, skipping every
// other kind (tool_result, status, text, thinking, …). It returns nil when no
// tool_use frame exists. "Newest" is the greatest Seq — the deterministic tiebreak
// across interleaved subagents (R9) — so the caller need not pre-sort: a frame slice
// in any order yields the same result.
func Latest(frames []Frame) *apitypes.RunActivity {
	var best *Frame
	for i := range frames {
		f := &frames[i]
		if f.Kind != "tool_use" {
			continue
		}
		if best == nil || f.Seq > best.Seq {
			best = f
		}
	}
	if best == nil {
		return nil
	}
	return FromFrame(best.Kind, best.Agent, best.AgentLabel, best.Payload, best.CreatedAt, best.Seq)
}

// unsafeRune reports whether r must not reach a terminal. It is inlined from
// termsafe.Unsafe (unicode.IsControl covers C0/C1/DEL, unicode.Cf covers the format
// characters — bidi overrides, zero-widths, the BOM), kept a stdlib-only spelling
// because this package may not import termsafe. TestUnsafeMatchesTermsafe pins it to
// the canonical predicate over every rune.
func unsafeRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf)
}

// sanitize strips the terminal-unsafe runes from s and caps the result at
// detailCapRunes runes, the same strip-then-cap workersvc.sanitizePlanChangedLine
// applies to model-authored display text. Invalid UTF-8 bytes decode to U+FFFD (the
// range-decode behaviour), which is a strictly better thing to display than a raw
// undecodable byte; the value is stripped, never rejected.
func sanitize(s string) string {
	if s == "" {
		return ""
	}
	out := make([]rune, 0, len(s))
	n := 0
	for _, r := range s {
		// An invalid byte decodes to utf8.RuneError (U+FFFD), which is safe to display
		// and is kept — a hostile byte becomes a visible replacement mark rather than
		// vanishing, matching termsafe.SanitizeTTY.
		if unsafeRune(r) {
			continue
		}
		out = append(out, r)
		n++
		if n >= detailCapRunes {
			break
		}
	}
	return string(out)
}

// deref returns the string a nullable frame column points at, or "" when nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// toolPayload is the tool_use frame payload shape runactivity reads: the tool name and
// its raw input object. Only the fields the rule consumes are decoded.
type toolPayload struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// toolInput is the subset of a tool's input object the rule folds into Detail/Agent.
type toolInput struct {
	FilePath     string `json:"file_path"`
	Description  string `json:"description"`
	SubagentType string `json:"subagent_type"`
}

// FromFrame folds a single frame into a RunActivity per PRD #1064 D3. It is exported
// so the handler can fold the one row its DISTINCT ON query returns per run without
// building a slice. The kind is accepted for symmetry with the persisted shape; the
// fold reads the payload regardless (Latest is what enforces "tool_use only").
//
// Tool is payload.name. For a tool_use named "Agent" (the lead's own subagent
// dispatch, which is often the newest frame on a live run before the subagent has
// written anything), Agent is input.subagent_type (falling back to the frame's own
// agent) and AgentLabel is input.description — the dispatch names the lane that is
// about to work. For every other tool the frame's own agent/agent_label are used
// verbatim. Detail is the repo-relative file_path for Read/Edit/Write/MultiEdit, the
// description for Agent and Bash (NEVER Bash's command), and empty otherwise.
// AgentLabel and Detail are stripped-and-capped; At and Seq come from the frame.
func FromFrame(kind string, agent, agentLabel *string, payload json.RawMessage, createdAt time.Time, seq int32) *apitypes.RunActivity {
	var p toolPayload
	// A malformed payload yields a zero toolPayload rather than an error: the now line
	// is advisory, and a frame the worker persisted is trusted to be a tool_use block,
	// so the worst case is an empty Tool/Detail, never a failed read.
	_ = json.Unmarshal(payload, &p)
	var in toolInput
	if len(p.Input) > 0 {
		_ = json.Unmarshal(p.Input, &in)
	}

	act := &apitypes.RunActivity{
		Agent:      deref(agent),
		AgentLabel: deref(agentLabel),
		Tool:       p.Name,
		At:         createdAt,
		Seq:        seq,
	}

	switch p.Name {
	case "Agent":
		if in.SubagentType != "" {
			act.Agent = in.SubagentType
		}
		act.AgentLabel = in.Description
		act.Detail = in.Description
	case "Bash":
		// The command is deliberately never surfaced (D3); only the description.
		act.Detail = in.Description
	case "Read", "Edit", "Write", "MultiEdit":
		act.Detail = in.FilePath
	default:
		act.Detail = ""
	}

	act.AgentLabel = sanitize(act.AgentLabel)
	act.Detail = sanitize(act.Detail)
	return act
}
