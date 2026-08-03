// PRD #37 M1: detect the cloned repo's own agent roster, `.claude/agents/*.md`.
//
// Trust model. This mirrors the parse-only channel `repo-skills.ts` opened for
// repo skills — the SDK never loads any of it (`settingSources` stays `[]`), the
// worker reads the files itself and converts them to the same `AgentTemplate`
// shape the claim payload delivers. Detection has NO side effects: the roster is
// data until the user (or an autopilot default) selects the repo source at the
// plan gate, which is where M3 assembles it into subagents.
//
// It DIVERGES from repo-skills on one point, deliberately (PRD #37 Decision 2, as
// ratified 2026-07-10): repo skills drop every frontmatter key but
// name+description, because a skill's capability grants are the whole risk. An
// agent definition without its tools is not that agent, so `tools` and `model`
// survive here — the declared tools AND model are honored, bounded only by:
//
//   - `tools`: a NARROW denylist repo agents can never receive — `Agent` (no
//     nested spawning) and the async-deferral tools (a deferred wakeup would only
//     wake to a killed subagent, #34). `Task` is the SDK's canonical alias for
//     `Agent`, so it is denied too. `WebFetch`/`WebSearch` are NOT denied: a repo
//     agent that declares `Bash` can exfiltrate the OAuth token via `curl`
//     regardless, so denying the first-class network tool stops nothing an
//     attacker would do while breaking honest agents (fact-checker + user
//     decision). The real close is the agent-container egress restriction, a
//     separate follow-up PRD. Unrecognized-but-well-formed names are LEFT IN
//     PLACE: an allowlist entry that matches no real tool is silently unavailable
//     (the SDK resolver is case-sensitive + fail-closed — `foo` grants nothing),
//     which is how a dev-team file declaring a tool this SDK does not ship parses
//     cleanly rather than failing.
//     CORRECTED 2026-08-03 (issue #210): this passage used to name SendMessage
//     and TaskUpdate as the worked example, "under a worker SDK that never
//     provides them". THE EXAMPLE WAS WRONG — the worker SDK DOES provide
//     SendMessage (26 tool_use entries across runs 71d83432 / 84b6a933 /
//     c13cff61, 18 of them successful) and TaskList (3). The structural claim on
//     the two lines above is untouched and still true; only the instance was
//     wrong. The consequence is what matters here and it INVERTS: a repo agent
//     declaring SendMessage is not granted an inert name, it is granted a WORKING
//     tool that can message the run's main thread — and the assertion in
//     test/repoagents.test.ts shows that state is reachable, not theoretical.
//     Benign (main is the intended recipient anyway), but it is a real capability
//     and this comment used to tell the reader it did not exist.
//   - `model`: honored for any value passing the API's `ValidateModel` shape check
//     (models.ts) — a full id like `claude-opus-4-8`, not only a short alias.
//     Only a string that could never be a model id (control chars, whitespace,
//     over-length) is ignored, and the agent inherits the run default.
//
// The Agent/deferral denial here is DEFENCE-IN-DEPTH (layer 1). The load-bearing
// guarantee is structural and lives in the assembly path M3 routes repo agents
// through: `agents.ts` sets `disallowedTools:[Agent]` on EVERY subagent (whether
// or not it declared `tools`), and `sdk-executor.ts` disallows the deferral tools
// globally — `disallowedTools` is applied first and wins over any declared
// allowlist. So an agent that omits `tools:` (inherit-all) still cannot get
// `Agent`. This parser strip only spares the SDK from ever seeing the denied name.
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
import { isValidModel } from "./models.js";

/** Tools a repo-authored subagent can never receive, whatever its frontmatter
 *  declares (PRD #37 Decision 2). NARROW by design: only `Agent` and the async-
 *  deferral tools, all of which are ALSO structurally denied for every subagent in
 *  the assembly path (agents.ts, sdk-executor.ts). WebFetch/WebSearch are
 *  deliberately absent — Bash egress makes denying them theatre (see the header).
 *  Stripping here is layer-1 defence-in-depth; the real guarantee is
 *  `disallowedTools` winning in M3. Compared on CANONICAL names (see canonicalTool). */
export const REPO_AGENT_DENIED_TOOLS: readonly string[] = [NESTED_AGENT_TOOL, ...ASYNC_DEFERRAL_TOOLS];

/** The SDK canonicalizes a handful of tool aliases before it resolves them (e.g.
 *  `Task` → `Agent`). A repo declaring `tools: [Task]` would slip an exact-string
 *  denylist but resolve to the denied `Agent`, so the strip canonicalizes first.
 *  Only aliases that map ONTO a denied tool need an entry here. */
const TOOL_CANONICAL: Readonly<Record<string, string>> = { Task: NESTED_AGENT_TOOL };

function canonicalTool(name: string): string {
  return TOOL_CANONICAL[name] ?? name;
}

/** Caps (PRD #37 Decision 7), mirroring the PRD #16 skills pattern. The API
 *  re-validates the reported roster against the file cap — it does not trust the
 *  worker's payload. */
export const REPO_AGENTS_MAX_FILES = 16;
export const REPO_AGENT_MAX_BYTES = 64 * 1024;
/** Bounds a description (in UTF-8 BYTES, not UTF-16 units) before it reaches a run
 *  message, the DB, and the gate UI. Bytes, to match the API's Go len() basis and
 *  the neighbouring byte caps (REPO_AGENT_MAX_BYTES): UTF-8 bytes >= UTF-16 units,
 *  so a UTF-16-unit check here would accept payloads the API then 400s. */
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
  /** One or more files past REPO_AGENTS_MAX_FILES were ignored. Emitted ONCE,
   *  aggregated with a `count`, never per file (a 10k-file repo must not flood the
   *  run stream with 10k rows). */
  | "over_limit"
  /** No frontmatter, bad name, missing/oversized/unsafe description, or empty body. */
  | "invalid"
  /** An earlier file already claimed this name (first wins). */
  | "duplicate"
  /** Every tool the file declared is denied to repo agents (or unusable), so the
   *  agent is skipped — see the fail-closed note on filterTools(). */
  | "tools_all_denied"
  /** Kept, with denied tools removed from its allowlist. */
  | "tools_filtered"
  /** Kept, with an unusable `model` string ignored (inherits the run default). */
  | "model_ignored";

export interface RepoAgentNote {
  /** The agent (or file) the note is about. Sanitized: it reaches a run message.
   *  Empty for the aggregated "over_limit" note, which names no single file. */
  name: string;
  reason: RepoAgentNoteReason;
  /** For "tools_filtered": which tools were removed. Always a subset of
   *  REPO_AGENT_DENIED_TOOLS — our own constants, never repo-supplied text. */
  tools?: string[];
  /** For "over_limit": how many files past the cap were ignored. */
  count?: number;
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
      return `${note.count ?? 0} agent file(s) past the cap of ${REPO_AGENTS_MAX_FILES} were ignored`;
    case "invalid":
      return `repo agent "${note.name}" was skipped: invalid frontmatter, name, description, or empty body`;
    case "duplicate":
      return `repo agent "${note.name}" was skipped: an earlier agent file already declares that name`;
    case "tools_all_denied":
      return `repo agent "${note.name}" was skipped: every tool it declares is denied to repo agents`;
    case "tools_filtered":
      return `repo agent "${note.name}": removed ${(note.tools ?? []).join(", ")} — repo agents never receive these tools`;
    case "model_ignored":
      return `repo agent "${note.name}" declared an unusable model string; it will inherit the run's default model`;
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
  const allFiles = entries
    .filter((e) => e.isFile() && e.name.endsWith(".md"))
    .map((e) => e.name)
    .sort();

  // Only the first REPO_AGENTS_MAX_FILES (by name) are considered; the rest are
  // ignored with a SINGLE aggregated note. Slicing here also means the parse loop
  // never runs past the cap, so a hostile repo with 10k agent files costs one
  // readdir + one note, not 10k run_messages (auditor F3).
  const overCap = allFiles.length - REPO_AGENTS_MAX_FILES;
  const files = allFiles.slice(0, REPO_AGENTS_MAX_FILES);
  if (overCap > 0) notes.push({ name: "", reason: "over_limit", count: overCap });

  const agents: AgentTemplate[] = [];
  const seen = new Set<string>();

  for (const file of files) {
    const slug = safeLabel(file.replace(/\.md$/, ""));
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

  // The description reaches a run message, the DB, and the plan-gate panel (as
  // plain text). It is UNTRUSTED repo-supplied text, so it is held to a STRICTER
  // rule than uzi's own single-line frontmatter fields: no control characters AND
  // no bidirectional/format characters (Unicode Cf). A right-to-left override
  // (U+202E) is category Cf, which a control-char check alone misses, and can
  // visually reorder the rendered text in an approval dialog. Rejected, not
  // scrubbed — a description that needs a format character is not a real one.
  const description = fm.fields.description?.trim() ?? "";
  if (description === "" || Buffer.byteLength(description, "utf8") > REPO_AGENT_MAX_DESCRIPTION_LEN || hasUnsafeChar(description)) {
    return invalid(name);
  }

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

  // The declared model is HONORED for any well-formed token (a full id, not only
  // a short alias — PRD #37 Decision 2 as ratified). Only a string that could
  // never be a model id is ignored, and the agent inherits the run default.
  const model = fm.fields.model?.trim();
  if (model) {
    if (isValidModel(model)) template.model = model;
    else notes.push({ name, reason: "model_ignored" });
  }

  return { ok: true, template, notes };
}

/** Split a declared allowlist into the entries a repo agent keeps and the DENIED
 *  ones that were removed. The denial check is on the CANONICAL name (so `Task` is
 *  caught as `Agent`); the DECLARED name is what appears in `denied`/`kept`, so the
 *  note shows what the author actually wrote and the SDK does its own canonicalizing
 *  on what survives. Unrecognized-but-well-formed names are kept (silently
 *  unavailable); malformed tokens are dropped without a note (they name no tool). */
function filterTools(declared: readonly string[]): { kept: string[]; denied: string[] } {
  const kept: string[] = [];
  const denied: string[] = [];
  for (const tool of declared.slice(0, REPO_AGENT_MAX_TOOLS)) {
    if (tool.length > REPO_AGENT_MAX_TOOL_LEN || !TOOL_NAME_RE.test(tool)) continue;
    if (REPO_AGENT_DENIED_TOOLS.includes(canonicalTool(tool))) {
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
 * ecosystem writes — `a, b`, `[a, b]`, and a `-` block sequence (at ANY indent,
 * including column 0, which prettier/yamlfmt produce). A scalar value that OPENS a
 * folded/literal block (`>`, `|`, `|-`, …) is dropped: the real content would be on
 * following indented lines this parser does not gather, so the indicator char must
 * never be mistaken for the value (it would otherwise reach the gate picker as a
 * one-character description). A leading UTF-8 BOM and CRLF are normalized so a
 * Windows-authored repo parses. First occurrence of a key wins.
 */
function parseFrontmatter(raw: string): Frontmatter | null {
  const lines = raw.replace(/^\uFEFF/, "").replace(/\r\n/g, "\n").replace(/\r/g, "\n").split("\n");
  if (lines[0] !== "---") return null;
  const close = lines.indexOf("---", 1);
  if (close < 0) return null;

  const fields: Frontmatter["fields"] = {};
  // Set while a `tools:` key with an empty inline value is collecting `- item`
  // continuation lines; any other key line ends the sequence.
  let toolsBlock: string[] | undefined;

  for (const line of lines.slice(1, close)) {
    // `\s*` (not `\s+`): a block-sequence item may sit at column 0, which is what
    // prettier/yamlfmt normalize `tools:\n- Bash` to. Requiring indentation dropped
    // such a legit agent with a FALSE "tools_all_denied" reason (reviewer). Safe
    // because `toolsBlock` is only active right after a bare `tools:` and is cleared
    // by the next `key:` line below.
    const item = /^\s*-\s+(.+)$/.exec(line);
    if (item && toolsBlock) {
      toolsBlock.push(stripQuotes(item[1]!.trim()));
      continue;
    }
    const idx = line.indexOf(":");
    if (idx < 0) continue;
    const key = line.slice(0, idx).trim();
    toolsBlock = undefined;
    const value = stripQuotes(line.slice(idx + 1).trim());
    // A folded/literal block-scalar opener (`>`, `|`, `>-`, `|+`, `|2`, …) is not a
    // value — drop the field. A block-scalar DESCRIPTION then fails the non-empty
    // check and the file is skipped, instead of `">"` reaching the picker.
    if (BLOCK_SCALAR_OPENER_RE.test(value)) continue;

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

/** A YAML block-scalar indicator standing alone as a value: `>`/`|` with an
 *  optional indentation digit and/or chomping (`+`/`-`). The block's real content
 *  lives on following indented lines this line-parser does not gather. */
const BLOCK_SCALAR_OPENER_RE = /^[|>][0-9]*[+-]?$/;

/** True when `s` carries a control character (Cc — newline, CR, ESC) or a
 *  bidirectional/format character (Cf — U+202E and friends). Mirrors the API's
 *  `hasUnsafeChar` (workersvc): untrusted repo text is held to the strict rule so
 *  it cannot forge structure or visually reorder itself in a run message / the
 *  approval panel. */
function hasUnsafeChar(s: string): boolean {
  return /[\p{Cc}\p{Cf}]/u.test(s);
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

/** A filename reduced to a safe label for a run message: everything outside
 *  `[A-Za-z0-9._-]` is dropped (not just C0/DEL — a filename can carry bidi/format
 *  chars that would otherwise reach the stream), then length-bounded. Used only for
 *  notes about files that never became agents; a real agent's name has already
 *  passed AGENT_NAME_RE. */
function safeLabel(value: string): string {
  return value.replace(/[^A-Za-z0-9._-]/g, "").slice(0, AGENT_NAME_MAX_LEN);
}
