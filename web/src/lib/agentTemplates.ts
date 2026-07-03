// Client-side helpers for the Agents UI: the subagent-file preview, the known
// Claude Code tool list for the tag editor, and the non-blocking guardrail
// warnings. The server is authoritative for both the export format (its
// /rendered endpoint) and the hard secret guardrail; these mirror that behavior
// for live UI feedback only.

import type { AgentTemplate } from "./api";

// KNOWN_TOOLS is the set the tag editor recognises. Unknown names are allowed
// (MCP tool names are legitimate) but flagged so a typo in a core tool is
// visible. Keep in sync with Claude Code's built-in tools plus the team-plumbing
// tools the builtin agents use.
export const KNOWN_TOOLS: string[] = [
  "Bash",
  "Read",
  "Write",
  "Edit",
  "Grep",
  "Glob",
  "WebFetch",
  "WebSearch",
  "NotebookEdit",
  "Task",
  "TodoWrite",
  "SendMessage",
  "TaskCreate",
  "TaskUpdate",
  "TaskList",
  "TaskGet",
  "TaskStop",
];

// renderSubagent mirrors the server renderer (fixed field order, inline
// comma-separated tools, model/tools omitted when empty). Used for the live
// preview; the server's /rendered output is the authoritative export.
export function renderSubagent(t: {
  name: string;
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
}): string {
  let s = "---\n";
  s += `name: ${t.name}\n`;
  s += `description: ${t.description}\n`;
  if (t.tools && t.tools.length > 0) s += `tools: ${t.tools.join(", ")}\n`;
  if (t.model) s += `model: ${t.model}\n`;
  s += "---\n\n";
  s += t.prompt_body;
  return s;
}

// unknownTools returns the entries not in KNOWN_TOOLS (for a non-blocking hint).
export function unknownTools(tools: string[]): string[] {
  return tools.filter((t) => !KNOWN_TOOLS.includes(t));
}

// looseSecretWarning returns a warning string if the text looks like it might
// carry an Anthropic credential, or "" otherwise. This is deliberately looser
// than the server guardrail (which rejects only a full token): it nudges on
// anything credential-ish without blocking, so prompts that merely mention the
// format still save.
export function looseSecretWarning(text: string): string {
  if (/sk-ant-/i.test(text) || /ANTHROPIC_API_KEY|ANTHROPIC_AUTH_TOKEN/.test(text)) {
    return "This text mentions an Anthropic credential pattern. Make sure you are not pasting a real token — tokens belong in Settings, never in a template.";
  }
  return "";
}

// summarizeTools renders the allowlist for the list view: "all" for inherit,
// else a compact join.
export function summarizeTools(t: AgentTemplate): string {
  if (!t.tools) return "all";
  if (t.tools.length === 0) return "none";
  return t.tools.join(", ");
}
