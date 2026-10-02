package apitypes

// RepoAgent is one agent the worker detected in the cloned repo's .claude/agents/ or
// .codex/agents/ folder.
// Names and descriptions only: the prompt bodies stay worker-side, so nothing this
// struct carries is ever executed — it is what the approval gate renders and what
// the run view shows afterwards.
//
// The definition lives here so the run DTOs that embed it stay stdlib-only leaves
// (PRD #64 M1). workersvc.RepoAgent is a type alias of this, keeping every existing
// reference and the workersvc validators compiling unchanged.
type RepoAgent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Folder is the repo folder the worker read this roster from: ".claude/agents" or
	// ".codex/agents". Empty means not reported (an older worker or run), which readers
	// treat as ".claude/agents". Set on repo-agent rosters only, never on own_agents.
	Folder string `json:"folder,omitempty"`
}

// AgentSelection is which roster a run's subagents come from, minus the agents the
// user excluded. Either/or, no mixing (Decision 4). The lead is always uzi's
// builtin and is never selectable or excludable (Decision 3).
//
// workersvc.AgentSelection is a type alias of this (see RepoAgent).
type AgentSelection struct {
	Source     string   `json:"source"`
	Exclusions []string `json:"exclusions"`
}
