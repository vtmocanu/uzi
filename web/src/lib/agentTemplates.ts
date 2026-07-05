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

// splitToolInput turns raw tag-editor input into clean tool names, splitting on
// commas and whitespace and dropping blanks. This keeps commas, spaces, and
// newlines (the frontmatter-injection vectors) out of every stored tag.
export function splitToolInput(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

// hasControlChar reports whether s contains a character the server's
// unicode.IsControl-based check rejects: a C0 control (incl. newline/CR/tab),
// DEL, a C1 control (0x80-0x9f), or the Unicode replacement char (malformed
// UTF-8). Kept in lockstep with the server so a user gets a clear UI warning
// instead of a bare 400. Implemented by code point so no control chars appear
// in the source.
function hasControlChar(s: string): boolean {
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x20 || (c >= 0x7f && c <= 0x9f) || c === 0xfffd) return true;
  }
  return false;
}

// MAX_MODEL_LEN caps a model alias / full ID. Real IDs (aliases through full
// bedrock IDs) sit well under this; the cap just bounds a pasted blob. Kept in
// lockstep with the server's maxModelLen (PRD #17 Decision 4).
export const MAX_MODEL_LEN = 100;

// modelFieldWarning mirrors the server's validateModel (PRD #17 Decision 4): a
// blank value is inherit (fine); otherwise the trimmed value must be a single
// whitespace-free token, control-char-free, and at most MAX_MODEL_LEN chars.
// The value is trimmed first, exactly as the server does, so a stray trailing
// space does not warn on something the server would accept. "" means clean.
export function modelFieldWarning(model: string): string {
  const m = model.trim();
  if (m === "") return "";
  if (m.length > MAX_MODEL_LEN) {
    return `Model is too long (max ${MAX_MODEL_LEN} characters); use a shorter alias or model ID.`;
  }
  if (hasControlChar(m)) {
    return "Model contains a newline or control character; remove it before saving.";
  }
  if (/\s/.test(m)) {
    return "Model must be a single token with no spaces.";
  }
  return "";
}

// frontmatterFieldWarning mirrors the server rejection of newline/control
// characters (and commas in tool names) in the single-line frontmatter fields,
// plus the tightened model rules (modelFieldWarning). The server is
// authoritative; this blocks submit early with a clear message (the usual
// vector is a paste into the description). "" means clean.
export function frontmatterFieldWarning(fields: {
  description: string;
  model: string;
  tools: string[];
}): string {
  if (hasControlChar(fields.description)) {
    return "Description contains a newline or control character; remove it before saving.";
  }
  const modelWarning = modelFieldWarning(fields.model);
  if (modelWarning) return modelWarning;
  for (const t of fields.tools) {
    if (hasControlChar(t) || t.includes(",")) {
      return `Tool name "${t}" contains a comma, newline, or control character.`;
    }
  }
  return "";
}

// isLeadTemplateName mirrors the worker's LEAD_NAME_RE (agent/src/agents.ts): a
// template with this name is the orchestrator routed to the main thread, not an
// invokable subagent. Used to badge it distinctly in the Agents list.
export function isLeadTemplateName(name: string): boolean {
  return /^(lead|orchestrator)$/i.test(name);
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
