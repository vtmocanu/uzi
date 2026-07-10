// PRD #37 M1: detect the cloned repo's own agent roster, `.claude/agents/*.md`.
//
// Trust model. This mirrors the parse-only channel `repo-skills.ts` opened for
// repo skills — the SDK never loads any of it (`settingSources` stays `[]`), the
// worker reads the files itself and converts them to the same `AgentTemplate`
// shape the claim payload delivers. Detection has NO side effects: the roster is
// data until the user (or an autopilot default) selects the repo source at the
// plan gate, which is where M3 assembles it into subagents.
//
// It DIVERGES from repo-skills on one point, deliberately (PRD #37 Decision 2):
// repo skills drop every frontmatter key but name+description, because a skill's
// capability grants are the whole risk. An agent definition without its tools is
// not that agent, so `tools` and `model` survive here — bounded, not trusted:
//
//   - `tools` is filtered through REPO_AGENT_DENIED_TOOLS. A repo agent can never
//     receive `Agent` (no nested spawning), the async-deferral tools (a deferred
//     wakeup would only wake to a killed subagent, #34), or `WebFetch`/`WebSearch`
//     (a repo-authored subagent never gets a first-class network tool). Names we
//     do not recognize are LEFT IN PLACE: an allowlist entry that matches no real
//     tool is silently unavailable, not an error — that is how uzi's own dev-team
//     files, which declare Claude Code team tools (SendMessage, TaskUpdate, …),
//     parse cleanly under a worker SDK that never provides them.
//   - `model` is honored only when it names one of the curated aliases (models.ts).
//     Anything else is ignored and the agent inherits the run default.
//
// A file that is over-cap, unparseable, or duplicate-named is skipped with a
// run-message note. Detection NEVER fails a run.
//
// One trap for M3: a repo file may legitimately be named `lead` (PRD Decision 3
// — it is just another subagent candidate, since the orchestrator always comes
// from the claim payload). `assembleAgents()` routes a `lead`-named template to
// the MAIN THREAD system prompt, so the repo roster must never be fed through
// that lead-detection path. Keep repo agents on the subagent side of the map.

import fs from "node:fs/promises";
import path from "node:path";
import { AGENT_NAME_MAX_LEN, AGENT_NAME_RE, type AgentTemplate, type RepoAgentSummary } from "./protocol.js";
import { ASYNC_DEFERRAL_TOOLS, NESTED_AGENT_TOOL } from "./guardrails.js";
import { isModelAlias } from "./models.js";

/** Tools a repo-authored subagent can never receive, whatever its frontmatter
 *  declares. `Agent` + the deferral tools are already structurally denied for every
 *  subagent (agents.ts, sdk-executor.ts); WebFetch/WebSearch are denied only to
 *  repo agents (PRD #37 Decision 2). Stripping them here is the FIRST layer — M3
 *  keeps `disallowedTools` winning over any allowlist, so a miss here still holds. */
export const REPO_AGENT_DENIED_TOOLS: readonly string[] = [
  NESTED_AGENT_TOOL,
  ...ASYNC_DEFERRAL_TOOLS,
  "WebFetch",
  "WebSearch",
];

/** Caps (PRD #37 Decision 7), mirroring the PRD #16 skills pattern. The API
 *  re-validates the reported roster against the file cap — it does not trust the
 *  worker's payload. */
export const REPO_AGENTS_MAX_FILES = 16;
export const REPO_AGENT_MAX_BYTES = 64 * 1024;
/** Bounds a description before it reaches a run message, the DB, and the gate UI. */
export const REPO_AGENT_MAX_DESCRIPTION_LEN = 1024;
/** Bounds a declared allowlist (and each entry) so a hostile frontmatter cannot
 *  bloat the AgentDefinition. Over-cap entries are dropped, not the agent. */
export const REPO_AGENT_MAX_TOOLS = 64;
export const REPO_AGENT_MAX_TOOL_LEN = 64;

/** The shape a real tool name can take (incl. `mcp__server__tool`). A token that
 *  fails this can match no tool, so it is dropped rather than carried along. */
const TOOL_NAME_RE = /^[A-Za-z][A-Za-z0-9_-]*$/;

/** Why a detected file was skipped, or what was changed about it. Codes are
 *  stable; the run-message text is derived (describeRepoAgentNote). */
export type RepoAgentNoteReason =
  /** File bytes exceeded REPO_AGENT_MAX_BYTES — skipped, never read. */
  | "too_large"
  /** Past REPO_AGENTS_MAX_FILES — skipped. */
  | "over_limit"
  /** No frontmatter, bad name, missing/oversized description, or empty body. */
  | "invalid"
  /** An earlier file already claimed this name (first wins). */
  | "duplicate"
  /** Every tool the file declared is denied to repo agents (or unusable), so the
   *  agent is skipped — see the fail-closed note on filterTools(). */
  | "tools_all_denied"
  /** Kept, with denied tools removed from its allowlist. */
  | "tools_filtered"
  /** Kept, with a non-alias `model` ignored (inherits the run default). */
  | "model_ignored";

export interface RepoAgentNote {
  /** The agent (or file) the note is about. Sanitized: it reaches a run message. */
  name: string;
  reason: RepoAgentNoteReason;
  /** For "tools_filtered": which tools were removed. Always a subset of
   *  REPO_AGENT_DENIED_TOOLS — our own constants, never repo-supplied text. */
  tools?: string[];
}

export interface DetectedRepoAgents {
  /** Parsed, capped, denylist-filtered agents, sorted by name. */
  agents: AgentTemplate[];
  /** Every skip/clamp, for the caller to emit as run messages (the worker owns
   *  the gapless seq). */
  notes: RepoAgentNote[];
}

/** The repo's agents directory inside the clone. Nothing else under `.claude/` is
 *  read by this module. */
export function repoAgentsDir(clonePath: string): string {
  return path.join(clonePath, ".claude", "agents");
}

/** Names + descriptions only — the wire form the worker reports (bodies stay
 *  worker-side; the API stores a roster, not untrusted prompts). */
export function repoAgentSummaries(agents: readonly AgentTemplate[]): RepoAgentSummary[] {
  return agents.map((a) => ({ name: a.name, description: a.description }));
}

/** Run-message text for one note. Repo-supplied strings never appear beyond the
 *  (regex-validated, length-capped) name; the tool list is our own constants. */
export function describeRepoAgentNote(note: RepoAgentNote): string {
  switch (note.reason) {
    case "too_large":
      return `repo agent "${note.name}" was skipped: the file exceeds the maximum allowed size`;
    case "over_limit":
      return `repo agent "${note.name}" was skipped: the repo declares more than ${REPO_AGENTS_MAX_FILES} agent files`;
    case "invalid":
      return `repo agent "${note.name}" was skipped: invalid frontmatter, name, description, or empty body`;
    case "duplicate":
      return `repo agent "${note.name}" was skipped: an earlier agent file already declares that name`;
    case "tools_all_denied":
      return `repo agent "${note.name}" was skipped: every tool it declares is denied to repo agents`;
    case "tools_filtered":
      return `repo agent "${note.name}": removed ${(note.tools ?? []).join(", ")} — repo agents never receive these tools`;
    case "model_ignored":
      return `repo agent "${note.name}" declared an unsupported model; it will inherit the run's default model`;
  }
}

/**
 * Enumerate + parse `<clone>/.claude/agents/*.md`.
 *
 * Symlinks are never followed (the agents dir must be a real directory, each
 * `*.md` a real file), so a hostile repo cannot redirect the read outside its own
 * tree. Files are visited in filename order, so the caps and the first-wins
 * dedupe are deterministic; the result is sorted by agent name.
 *
 * A missing directory yields an empty roster and no notes.
 */
export async function detectRepoAgents(clonePath: string): Promise<DetectedRepoAgents> {
  const dir = repoAgentsDir(clonePath);
  const notes: RepoAgentNote[] = [];

  let dirStat;
  try {
    dirStat = await fs.lstat(dir);
  } catch {
    return { agents: [], notes: [] };
  }
  if (!dirStat.isDirectory()) return { agents: [], notes: [] };

  const entries = await fs.readdir(dir, { withFileTypes: true });
  const files = entries
    .filter((e) => e.isFile() && e.name.endsWith(".md"))
    .map((e) => e.name)
    .sort();

  const agents: AgentTemplate[] = [];
  const seen = new Set<string>();

  for (const [index, file] of files.entries()) {
    const slug = safeLabel(file.replace(/\.md$/, ""));
    if (index >= REPO_AGENTS_MAX_FILES) {
      notes.push({ name: slug, reason: "over_limit" });
      continue;
    }

    const full = path.join(dir, file);
    let fileStat;
    try {
      // lstat, not stat: a symlinked *.md is skipped outright rather than read
      // through (`readdir` already reports a symlink as neither file nor dir, so
      // this is belt-and-braces against a future switch to `stat`).
      fileStat = await fs.lstat(full);
    } catch {
      continue;
    }
    if (!fileStat.isFile()) continue;
    // Checked BEFORE the read, so an oversized file is never loaded into memory.
    if (fileStat.size > REPO_AGENT_MAX_BYTES) {
      notes.push({ name: slug, reason: "too_large" });
      continue;
    }

    let raw: string;
    try {
      raw = await fs.readFile(full, "utf8");
    } catch {
      notes.push({ name: slug, reason: "invalid" });
      continue;
    }

    const parsed = parseAgentFile(raw, slug);
    if (!parsed.ok) {
      notes.push({ name: parsed.name, reason: parsed.reason });
      continue;
    }
    if (seen.has(parsed.template.name)) {
      notes.push({ name: parsed.template.name, reason: "duplicate" });
      continue;
    }
    seen.add(parsed.template.name);
    agents.push(parsed.template);
    notes.push(...parsed.notes);
  }

  agents.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return { agents, notes };
}

/** A parsed file: either a template (plus the non-fatal clamps applied to it), or
 *  the reason it is not usable as an agent, under the name to blame in the note. */
type ParsedAgentFile =
  | { ok: true; template: AgentTemplate; notes: RepoAgentNote[] }
  | { ok: false; name: string; reason: "invalid" | "tools_all_denied" };

function parseAgentFile(raw: string, slug: string): ParsedAgentFile {
  const invalid = (name: string): ParsedAgentFile => ({ ok: false, name, reason: "invalid" });

  const fm = parseFrontmatter(raw);
  if (!fm) return invalid(slug);

  // The frontmatter name is the identity when present; the filename slug is the
  // documented fallback. Either way it must pass the kebab-case rule the API
  // holds its own templates to — the name keys the SDK `agents` map, is echoed in
  // run messages, and (M2) round-trips through the API's roster validation.
  const name = fm.fields.name?.trim() || slug;
  if (name.length > AGENT_NAME_MAX_LEN || !AGENT_NAME_RE.test(name)) return invalid(slug);

  const description = fm.fields.description?.trim() ?? "";
  if (description === "" || description.length > REPO_AGENT_MAX_DESCRIPTION_LEN) return invalid(name);

  const body = fm.body;
  if (body.trim() === "") return invalid(name);

  const notes: RepoAgentNote[] = [];
  const template: AgentTemplate = { name, description, prompt_body: body };

  if (fm.fields.tools !== undefined) {
    const filtered = filterTools(fm.fields.tools);
    // An allowlist that survives as `[]` would be read as "inherit all tools"
    // (agents.ts: null/absent/empty ⇒ inherit) — a privilege ESCALATION for an
    // agent that asked for nothing but denied tools. Fail closed: skip the agent.
    if (filtered.kept.length === 0) return { ok: false, name, reason: "tools_all_denied" };
    if (filtered.denied.length > 0) notes.push({ name, reason: "tools_filtered", tools: filtered.denied });
    template.tools = filtered.kept;
  }

  const model = fm.fields.model?.trim();
  if (model) {
    if (isModelAlias(model)) template.model = model;
    else notes.push({ name, reason: "model_ignored" });
  }

  return { ok: true, template, notes };
}

/** Split a declared allowlist into the entries a repo agent keeps and the DENIED
 *  ones that were removed. Unrecognized-but-well-formed names are kept (silently
 *  unavailable); malformed tokens are dropped without a note (they name no tool). */
function filterTools(declared: readonly string[]): { kept: string[]; denied: string[] } {
  const kept: string[] = [];
  const denied: string[] = [];
  for (const tool of declared.slice(0, REPO_AGENT_MAX_TOOLS)) {
    if (tool.length > REPO_AGENT_MAX_TOOL_LEN || !TOOL_NAME_RE.test(tool)) continue;
    if (REPO_AGENT_DENIED_TOOLS.includes(tool)) {
      if (!denied.includes(tool)) denied.push(tool);
      continue;
    }
    if (!kept.includes(tool)) kept.push(tool);
  }
  return { kept, denied };
}

interface Frontmatter {
  fields: { name?: string; description?: string; tools?: string[]; model?: string };
  body: string;
}

/**
 * Split leading YAML frontmatter, keeping only the four keys an AgentTemplate has.
 * Every other key is ignored — `hooks`, `settings`, and anything else a repo
 * invents can never reach the SDK through this path.
 *
 * Deliberately a line parser, not a YAML engine (same call as repo-skills.ts): the
 * surface is four scalar keys plus a list. `tools` accepts the three forms the
 * ecosystem writes — `a, b`, `[a, b]`, and a `-` block sequence. A value that is
 * anything else (a folded `>`/`|` block, nested maps) yields a field that fails
 * validation upstream, i.e. the file is skipped with a note. CRLF is normalized so
 * a Windows repo parses. First occurrence of a key wins.
 */
function parseFrontmatter(raw: string): Frontmatter | null {
  const lines = raw.replace(/\r\n/g, "\n").replace(/\r/g, "\n").split("\n");
  if (lines[0] !== "---") return null;
  const close = lines.indexOf("---", 1);
  if (close < 0) return null;

  const fields: Frontmatter["fields"] = {};
  // Set while a `tools:` key with an empty inline value is collecting `- item`
  // continuation lines; any other key line ends the sequence.
  let toolsBlock: string[] | undefined;

  for (const line of lines.slice(1, close)) {
    const item = /^\s+-\s+(.+)$/.exec(line);
    if (item && toolsBlock) {
      toolsBlock.push(stripQuotes(item[1]!.trim()));
      continue;
    }
    const idx = line.indexOf(":");
    if (idx < 0) continue;
    const key = line.slice(0, idx).trim();
    toolsBlock = undefined;
    const value = stripQuotes(line.slice(idx + 1).trim());

    if (key === "name" && fields.name === undefined) fields.name = value;
    else if (key === "description" && fields.description === undefined) fields.description = value;
    else if (key === "model" && fields.model === undefined) fields.model = value;
    else if (key === "tools" && fields.tools === undefined) {
      if (value === "") {
        // `tools:` with nothing on the line opens a block sequence. Note it as a
        // DECLARED (currently empty) allowlist either way: treating it as "no
        // tools key" would hand the agent the inherit-all default.
        toolsBlock = [];
        fields.tools = toolsBlock;
      } else {
        fields.tools = parseInlineToolList(value);
      }
    }
  }

  let bodyLines = lines.slice(close + 1);
  if (bodyLines[0] === "") bodyLines = bodyLines.slice(1); // drop one blank separator
  return { fields, body: bodyLines.join("\n") };
}

/** `Bash, Read` or `[Bash, Read]` → ["Bash","Read"]. */
function parseInlineToolList(value: string): string[] {
  let s = value.trim();
  if (s.startsWith("[") && s.endsWith("]")) s = s.slice(1, -1);
  return s
    .split(",")
    .map((t) => stripQuotes(t.trim()))
    .filter((t) => t !== "");
}

/** Strip one layer of matching surrounding quotes. */
function stripQuotes(v: string): string {
  if (v.length >= 2 && ((v[0] === '"' && v.at(-1) === '"') || (v[0] === "'" && v.at(-1) === "'"))) {
    return v.slice(1, -1);
  }
  return v;
}

/** A filename made safe to put in a run message: control characters stripped,
 *  length bounded. Used only for notes about files that never became agents (a
 *  real agent's name has already passed AGENT_NAME_RE). */
function safeLabel(value: string): string {
  return value.replace(/[\u0000-\u001f\u007f]/g, "").slice(0, AGENT_NAME_MAX_LEN);
}
