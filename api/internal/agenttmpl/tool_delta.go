package agenttmpl

// productToolDelta lists the tools a builtin carries beyond its upstream role
// (PRD #1849 D5). The builtin files mirror the upstream role library byte for
// byte, and the library cannot name uzi's worker-only tools, so the grant lives
// here instead of in the file. It is applied wherever a builtin's tools come from
// upstream content: the embedded builtins at init, and an agent-source sync that
// overrides a builtin row (agentsource.planApply). Keyed on the builtin row, never
// on a repo-authored agent's name, so a repo agent that happens to be called
// fact-checker gets nothing from it.
var productToolDelta = map[string][]string{
	// The forge read tools, reached through the worker's in-process forge MCP
	// server (agent/src/agents.ts toDefinition attaches it from the allowlist).
	"fact-checker": {
		"mcp__forge__get_issue",
		"mcp__forge__list_issues",
		"mcp__forge__get_merge_request",
		"mcp__forge__get_pipeline_jobs",
		"mcp__forge__latest_pipeline",
		"mcp__forge__list_issue_label_events",
	},
}

// WithProductTools returns d with its builtin's product-only tools appended to an
// explicit allowlist, deduplicated, in delta order. An empty tools list
// ("inherit all") is returned unchanged: appending would turn it into a narrow
// allowlist. The cost is that an inherit-all role gets no delta tool, and the
// worker attaches the forge MCP server only from an explicit mcp__forge__* grant
// (agent/src/agents.ts toDefinition), so such a role has no forge access.
// TestBuiltinsCarryProductToolDelta requires every delta role's shipped file to
// list its tools. d itself is not modified.
func WithProductTools(d Definition) Definition {
	extra := productToolDelta[d.Name]
	if len(extra) == 0 || len(d.Tools) == 0 {
		return d
	}
	tools := make([]string, len(d.Tools), len(d.Tools)+len(extra))
	copy(tools, d.Tools)
	for _, t := range extra {
		if !containsTool(tools, t) {
			tools = append(tools, t)
		}
	}
	d.Tools = tools
	return d
}

func containsTool(tools []string, t string) bool {
	for _, x := range tools {
		if x == t {
			return true
		}
	}
	return false
}
