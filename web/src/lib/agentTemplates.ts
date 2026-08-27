// Client-side helpers for the Agents UI: the subagent-file preview, the known
// Claude Code tool list for the tag editor, and the non-blocking guardrail
// warnings. The server is authoritative for both the export format (its
// /rendered endpoint) and the hard secret guardrail; these mirror that behavior
// for live UI feedback only.

import type { AgentTemplate, AgentTemplateScope } from "./api";

// TEMPLATE_SCOPE_LABEL / templateScopeBadgeTone mirror the skills scope helpers
// (lib/skills.ts) so agent templates and skills badge scope identically (PRD #18
// M7). "Mine" is the owner-facing label for a user-scope template.
export const TEMPLATE_SCOPE_LABEL: Record<AgentTemplateScope, string> = {
  builtin: "Builtin",
  global: "Global",
  user: "Mine",
};

export function templateScopeBadgeTone(scope: AgentTemplateScope): "brand" | "info" | "neutral" {
  switch (scope) {
    case "builtin":
      return "brand";
    case "global":
      return "info";
    default:
      return "neutral";
  }
}

// ── Source-aware provenance badge (PRD #602 M5) ──────────────────────────────
// A row carries at most ONE provenance chip, chosen from `origin` with
// `differs_from_builtin` as the drift signal for the non-synced cases. The three
// origins and what each shows:
//   - "synced" → the SYNCED chip (its body came from the configured agent-source
//     repo). Shown INSTEAD of the drift chip, whatever differs_from_builtin says:
//     a synced body legitimately differs from the embedded default without being
//     an accidental edit, so "differs from shipped" would misdescribe it. A synced
//     row keeps whatever reset/delete affordance its SCOPE gives it — an overridden
//     builtin (scope='builtin') still resets to its embedded default; a synced-only
//     global (scope='global') has no embedded twin and is deletable, never reset.
//   - "admin"/"embedded"/null → the existing "differs from shipped" chip when the
//     row drifts from its shipped definition, and nothing when it does not.
// The tone/label live here beside the scope-badge helpers so the list, the detail
// header, and any future surface badge provenance identically.
export type TemplateOrigin = "embedded" | "synced" | "admin";

// templateOrigin resolves the effective origin, defaulting an absent value the way
// the server backfill does: a builtin with no origin is a pristine "embedded"
// default; anything else with no origin has no provenance (null).
export function templateOrigin(t: Pick<AgentTemplate, "origin" | "is_builtin">): TemplateOrigin | null {
  if (t.origin) return t.origin;
  return t.is_builtin ? "embedded" : null;
}

// The "synced" chip's tone and copy. A distinct tone ("plan", the violet used
// nowhere else in the Agents table) reads as "arrived through the source pipeline",
// keeping it visually separate from the blue "info" drift chip and the brand
// orchestrator chip.
export const SYNCED_BADGE_LABEL = "synced";
export const SYNCED_BADGE_TONE = "plan" as const;
export const SYNCED_BADGE_HINT =
  "This template's body came from the configured agent-source repo, applied by an admin.";

// provenanceBadgeKind picks which chip a row shows: "synced" for a synced-origin
// row (shown instead of the drift chip), "drift" when a non-synced row differs from
// its shipped definition, or null when there is nothing to flag.
export function provenanceBadgeKind(
  t: Pick<AgentTemplate, "origin" | "is_builtin" | "differs_from_builtin">,
): "synced" | "drift" | null {
  if (templateOrigin(t) === "synced") return "synced";
  return t.differs_from_builtin ? "drift" : null;
}

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
// whitespace-free token, at most MAX_MODEL_LEN chars, and free of control chars
// AND Unicode Cf format chars (bidi overrides, zero-width joiners/spaces). The
// Cf rejection is scoped here only — it matches the server's tightened
// ValidateModel (base a8234ce9) and is NOT applied to the description/tools
// fields, which the server does not reject Cf on. The value is trimmed first,
// exactly as the server does, so a stray trailing space does not warn on
// something the server would accept. "" means clean.
export function modelFieldWarning(model: string): string {
  const m = model.trim();
  if (m === "") return "";
  if (m.length > MAX_MODEL_LEN) {
    return `Model is too long (max ${MAX_MODEL_LEN} characters); use a shorter alias or model ID.`;
  }
  if (hasControlChar(m) || /\p{Cf}/u.test(m)) {
    return "Model contains a newline, control, or format character; remove it before saving.";
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

// TemplateContent is the four mutable columns the drift comparison reads —
// exactly the fields agenttmpl.Definition carries, minus the name, which is the
// lookup key rather than content.
export interface TemplateContent {
  description: string;
  model: string | null;
  tools: string[] | null;
  prompt_body: string;
}

// driftedColumns names which of the four columns differ between the shipped
// definition and a template's current values, in display order.
//
// IT IS THE ONLY CLIENT-SIDE COPY OF THESE RULES, and it lives here rather than
// inside the editor so the diff panel and the Reset confirmation cannot name
// different columns — a confirmation that undersells what it is about to discard
// is worse than none. It applies the same rules as the server's
// agenttmpl.SameContent, each for the reason stated there: tools compared
// order-SENSITIVELY (the order is rendered), null and [] both meaning
// inherit-all, and neither free-text column trimmed.
export function driftedColumns(shipped: TemplateContent, current: TemplateContent): string[] {
  const out: string[] = [];
  if (shipped.description !== current.description) out.push("description");
  if ((shipped.model ?? "") !== (current.model ?? "")) out.push("model");
  const a = shipped.tools ?? [];
  const b = current.tools ?? [];
  if (a.length !== b.length || a.some((t, i) => t !== b[i])) out.push("tools");
  if (shipped.prompt_body !== current.prompt_body) out.push("prompt body");
  return out;
}

// toolsSummary renders one side of the tools comparison. Kept beside
// driftedColumns because "inherit all" is the same null-means-inherit convention
// the comparison folds, and summarizeTools below answers a different question
// (it takes a whole row, and says "none" for an empty list).
export function toolsSummary(tools: string[] | null): string {
  return tools && tools.length > 0 ? tools.join(", ") : "inherit all";
}

// summarizeTools renders the allowlist for the list view: "all" for inherit,
// else a compact join.
export function summarizeTools(t: AgentTemplate): string {
  if (!t.tools) return "all";
  if (t.tools.length === 0) return "none";
  return t.tools.join(", ");
}
