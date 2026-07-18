package workersvc

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"gitlab.example.com/vtmocanu/uzi/api/internal/agenttmpl"
	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// Per-run agent selection (PRD #37).
//
// Two payloads arrive here from two different directions and NEITHER is trusted:
//
//   - RepoAgent{} rosters come from a WORKER, which parsed them out of a cloned
//     repo's .claude/agents/*.md. The worker caps and validates them too, but a
//     worker is a program holding a join token, not an authority: a compromised or
//     buggy one could report a 10k-entry roster of control characters straight into
//     the DB and the approval UI (security finding F3). So the API re-checks every
//     bound the worker claims to have applied.
//
//   - AgentSelection{} comes from a BROWSER. Its exclusions must name agents that
//     actually exist in the roster the run will use, and they must not exclude
//     every subagent — a run with an empty roster would silently degrade to a lead
//     with nobody to delegate to.
//
// The caps deliberately mirror the worker's (agent/src/repoagents.ts): 16 agents
// per repo, 64-char names, 1024-byte descriptions. They are re-declared rather
// than imported because the two languages cannot share a constant; a drift here
// means the API rejects a roster the worker considered legal, which surfaces as a
// clear 400 rather than as silent truncation. The description cap is measured in
// UTF-8 BYTES on both sides (Go len() here, Buffer.byteLength on the worker) — the
// real payload bound, and equal across the two runtimes.
const (
	// MaxRepoAgents bounds a reported roster (worker cap: REPO_AGENTS_MAX_FILES).
	MaxRepoAgents = 16
	// MaxAgentDescriptionLen bounds one roster entry's description in UTF-8 bytes
	// (worker cap: REPO_AGENT_MAX_DESCRIPTION_LEN, also bytes).
	MaxAgentDescriptionLen = 1024
	// MaxAgentExclusions bounds a selection's exclusion list. Twice MaxRepoAgents:
	// the owner's own template list is not file-capped, so excluding all but one of
	// a large roster must still fit (worker cap: AGENT_EXCLUSIONS_MAX).
	MaxAgentExclusions = 32
)

// Agent sources (runs.agent_source). Kept in lockstep with the column's CHECK.
const (
	AgentSourceRepo = "repo"
	AgentSourceOwn  = "own"
)

// RepoAgent and AgentSelection are defined in apitypes (the stdlib-only leaf the
// uzi CLI links, PRD #64 M1) and re-exported here as type ALIASES. Aliases, not new
// types, so every existing workersvc.RepoAgent / workersvc.AgentSelection reference,
// the validate* free functions below, and DecodeRepoAgents/DecodeExclusions keep
// compiling unchanged. The Max* caps and all validation logic stay in this package;
// apitypes owns only the wire shape. See apitypes/agent.go for the field docs.
type RepoAgent = apitypes.RepoAgent

// AgentSelection — see RepoAgent.
type AgentSelection = apitypes.AgentSelection

// validateRepoAgents enforces every bound on a worker-reported roster: length,
// per-item name shape and length, description length, and the absence of control
// characters (a name or description reaches a run message, the DB, and the gate
// panel — a newline there forges structure). Duplicate names are rejected: the
// worker deduped, so a duplicate means the payload is not what the worker built.
//
// A nil roster is "not reported" and is valid — only a run whose worker reported
// gets a roster at all. An EMPTY roster is meaningful and must survive: it says
// detection ran and found nothing.
func validateRepoAgents(agents []RepoAgent) error {
	if len(agents) > MaxRepoAgents {
		return fmt.Errorf("%w: at most %d repo agents may be reported", ErrInvalidSelection, MaxRepoAgents)
	}
	seen := make(map[string]bool, len(agents))
	for _, a := range agents {
		if !agenttmpl.IsValidName(a.Name) {
			return fmt.Errorf("%w: repo agent name must be kebab-case and at most %d characters", ErrInvalidSelection, agenttmpl.MaxNameLen)
		}
		if seen[a.Name] {
			return fmt.Errorf("%w: repo agent %q was reported twice", ErrInvalidSelection, a.Name)
		}
		seen[a.Name] = true

		if strings.TrimSpace(a.Description) == "" {
			return fmt.Errorf("%w: repo agent %q must have a description", ErrInvalidSelection, a.Name)
		}
		if len(a.Description) > MaxAgentDescriptionLen {
			return fmt.Errorf("%w: repo agent %q description must be at most %d bytes", ErrInvalidSelection, a.Name, MaxAgentDescriptionLen)
		}
		if hasUnsafeChar(a.Description) {
			return fmt.Errorf("%w: repo agent %q description must not contain newlines, control, or bidirectional/format characters", ErrInvalidSelection, a.Name)
		}
	}
	return nil
}

// validateSelection checks a selection against the roster it names. `roster` is the
// chosen source's selectable subagent names (the lead already removed). Exclusions
// must be well-formed and must all exist in the roster.
//
// An empty roster is NOT rejected for the `own` source: a user may disable every
// subagent template (or have only the lead allocated, which ownSubagentNames strips),
// and that is an established, legal configuration — the lead runs alone against its
// hardcoded guardrail prompt (ARCHITECTURE.md "The lead is a normal template … a
// user may disable it"; agent/src/prompt.ts renders "No subagents are available; do
// the work yourself."). Turning that into a 400 would regress a run that approves
// fine today. Only the `repo` source requires a non-empty roster — that is the real
// ordering trap: choosing repo agents when none were detected would activate an
// empty agent map, a broken run rather than a deliberate lead-only one.
func validateSelection(sel AgentSelection, roster []string) error {
	if sel.Source != AgentSourceRepo && sel.Source != AgentSourceOwn {
		return fmt.Errorf("%w: source must be 'repo' or 'own'", ErrInvalidSelection)
	}
	if len(sel.Exclusions) > MaxAgentExclusions {
		return fmt.Errorf("%w: at most %d exclusions", ErrInvalidSelection, MaxAgentExclusions)
	}
	if len(roster) == 0 && sel.Source == AgentSourceRepo {
		return fmt.Errorf("%w: this run detected no repo agents", ErrInvalidSelection)
	}

	known := make(map[string]bool, len(roster))
	for _, n := range roster {
		known[n] = true
	}
	excluded := make(map[string]bool, len(sel.Exclusions))
	for _, name := range sel.Exclusions {
		if !agenttmpl.IsValidName(name) {
			return fmt.Errorf("%w: exclusion must be kebab-case and at most %d characters", ErrInvalidSelection, agenttmpl.MaxNameLen)
		}
		// An exclusion naming an agent the roster does not contain is always an
		// error, INCLUDING when the roster is empty (an empty `own` roster with an
		// exclusion is a confused request, not a lead-only run).
		if !known[name] {
			return fmt.Errorf("%w: %q is not one of this run's %s agents", ErrInvalidSelection, name, sel.Source)
		}
		excluded[name] = true
	}
	// Gated on a non-empty roster: `0 >= 0` must NOT read as "everything excluded"
	// for a legitimately subagent-less own run (which reaches here with no exclusions).
	if len(known) > 0 && len(excluded) >= len(known) {
		return fmt.Errorf("%w: at least one subagent must remain selected", ErrInvalidSelection)
	}
	return nil
}

// ownSubagentNames drops the lead from the run owner's delivered TEMPLATE names:
// that lead is uzi's builtin orchestrator, which runs on the main thread and is
// never an invokable subagent, so it can be neither excluded nor counted as a
// survivor.
//
// It is applied to the `own` source ONLY. A repo file named `lead` is deliberately
// just another subagent candidate (Decision 3) — the orchestrator always comes from
// the claim payload, so a repo roster has no lead to protect and filtering one out
// would silently drop an agent the user can see in the gate panel.
func ownSubagentNames(templates []string) []string {
	out := make([]string, 0, len(templates))
	for _, n := range templates {
		if agenttmpl.IsLeadName(n) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// repoAgentNames is the roster a run's persisted repo_agents column names. A NULL
// or malformed column yields none — a malformed one cannot be a source, and
// validateSelection turns that into "this run detected no repo agents" rather than
// a 500: the column is data the worker wrote, not an invariant of this request.
func repoAgentNames(raw []byte) []string {
	agents, err := DecodeRepoAgents(raw)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name)
	}
	return names
}

// DecodeRepoAgents reads the runs.repo_agents jsonb column. A NULL/empty column
// yields a NIL slice, which the run DTO renders as JSON null — "no worker ever
// reported", as distinct from a reported-but-empty `[]`.
func DecodeRepoAgents(raw []byte) ([]RepoAgent, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []RepoAgent
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// DecodeExclusions reads the runs.agent_exclusions jsonb column, with the same
// NULL-vs-`[]` distinction: null = no selection recorded, `[]` = a selection that
// excluded nothing.
func DecodeExclusions(raw []byte) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// encodeJSONArray marshals a slice for a jsonb column, never emitting the literal
// `null` a nil slice would: the columns distinguish NULL (never reported) from
// `[]` (reported empty), and that distinction dies if a nil slice serializes to
// JSON null inside a non-NULL jsonb value.
func encodeJSONArray[T any](items []T) ([]byte, error) {
	if items == nil {
		items = []T{}
	}
	return json.Marshal(items)
}

// hasUnsafeChar reports whether s carries a control character (newline, CR, an
// ANSI-escape ESC, …) OR a format/bidirectional character (Unicode category Cf:
// the RLO/LRO overrides U+202A–202E, the isolates U+2066–2069, zero-width joiners,
// the BOM). uzi holds its OWN templates to the control-char rule via IsControl; a
// repo agent's description is UNTRUSTED and gets the stricter one, because these
// fields render straight into the plan-approval panel and a run message: a bidi
// override can visually reorder "reviewer" into something else in an approval
// dialog, and IsControl alone (Cc category) never catches U+202E (Cf). No
// legitimate one-line agent description needs a format character, so rejecting the
// whole category costs nothing. The worker will scrub these too in a follow-up; the
// API does not take that on trust.
func hasUnsafeChar(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return true
		}
	}
	return false
}
