package agenttmpl

import "regexp"

// NameRe is the kebab-case constraint on an agent name: the subagent identity —
// the SDK `agents` map key, the PRD #4 routing key, the name a run message and an
// exclusion chip refer to. It lives here, in the neutral dependency-free package,
// because three surfaces must agree on it and a drifting copy would let one
// accept an identity another rejects: the template write path (handler), the
// worker's repo-agent detection (agent/src/protocol.ts AGENT_NAME_RE), and the
// PRD #37 roster/selection validation (workersvc).
var NameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// MaxNameLen bounds a name NameRe would otherwise admit at any length. Mirrors
// the worker's AGENT_NAME_MAX_LEN.
const MaxNameLen = 64

// LeadNameRe matches the orchestrator names. A template whose name matches is
// routed to the SDK main thread as the lead, never registered as an invokable
// subagent (agent/src/agents.ts LEAD_NAME_RE, which this mirrors — case
// insensitive to match it, though NameRe already forces lowercase).
//
// The single legitimate lead is the seeded builtin, so the API refuses to
// create or rename any other template into a lead name, and PRD #37's agent
// selection never offers the lead as an excludable subagent under either source.
var LeadNameRe = regexp.MustCompile(`(?i)^(lead|orchestrator)$`)

// IsValidName reports whether name is a well-formed, length-bounded agent name.
func IsValidName(name string) bool {
	return len(name) <= MaxNameLen && NameRe.MatchString(name)
}

// IsLeadName reports whether name is reserved for the lead orchestrator.
func IsLeadName(name string) bool { return LeadNameRe.MatchString(name) }
