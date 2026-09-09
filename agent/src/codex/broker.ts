// PRD #1171 (M3, milestone 2): the worker callback broker — the fail-closed
// authority that OWNS every Codex run-tool callback.
//
// A Codex run does not call our tools directly. The app-server drives a turn and,
// for every model-selected effect (a shell command, a file patch, a delegation, a
// workflow signal, an MCP tool), issues a worker CALLBACK. This broker is the sole
// place that callback is admitted, bound to authority, and executed. It is the
// runtime companion to the pure {@link ExecutionRegistry} (idempotency/poison
// bookkeeping): the registry decides "may this callback run at all"; the broker
// decides "what is this callback ALLOWED to do, and does the effect actually run".
//
// FAIL-CLOSED, and authority is bound to IMMUTABLE per-(thread,turn) grants, NEVER
// to the callback's arguments. The tool NAME + the grants decide the capability;
// the arguments only ever parameterize an ALREADY-authorized effect. This is what
// makes "a plausible argument cannot widen authority" true: a tool absent from the
// grant's allow-list is denied no matter how well-formed its args are.
//
// Every external actor is an INJECTED SEAM, so the whole broker is unit-testable
// with fakes; the real transport/process/harness wiring lands in a later milestone:
//   - `spawnCommand` runs a shell effect as the credential-free COMMAND identity.
//     Production wires it to `commandRootCommand` (agent/src/runner-uid.ts, uid
//     10003) so the effect runs under the setpriv cap-clear; the broker never spawns
//     directly and never reaches for `commandRootCommand` itself.
//   - `fileop` is the openat2 race-safe fileop client (agent/codex/supervisor/fileop):
//     EVERY model-selected file effect goes through it, NEVER through node `fs` on a
//     model path, so the kernel (RESOLVE_BENEATH|NO_SYMLINKS|NO_MAGICLINKS) is the
//     containment and there is no check-then-use window.
//   - `delegate` is the synchronous child-thread runner; the broker AWAITS it so a
//     callback resolves only after its owned child settles. Nested delegation (a
//     child callback delegating again) is denied.
//   - `toolHandlers` are the thin memory/forge/findings/skills pass-throughs (the
//     real factories are wired later).
//
// Screening is NOT reimplemented here: shell commands go through
// `screenBashCommand` and file paths through the `screenToolPath` path-jail, both
// from agent/src/guardrails.ts. Signal parsing reuses `scanSignals` from
// agent/src/signals.ts (the authoritative main-thread-only parser) rather than a
// second copy of the clamping logic.

import path from "node:path";
import { createHash } from "node:crypto";

import { screenBashCommand, screenToolPath } from "../guardrails.js";
import { SIGNAL_SERVER_NAME, scanSignals } from "../signals.js";
import type { ExecutionRegistry } from "./registry.js";

// --- Ingestion bounds (carried forward from the M1 audit) ---------------------
// Every id/name field on an inbound callback is worker/model-influenced untrusted
// input. A field over its byte cap, or a non-string/empty id, is a bounded DENY —
// NOT a poison: an oversize id is a malformed frame, not a replay/forgery, so it
// must not sticky-poison the epoch. (A CHANGED-payload reuse of an already-tracked
// tuple still poisons, but that decision lives in the registry, not here.) Exported
// so the test asserts the exact caps rather than hard-coding a copy of them.
export const MAX_ID_BYTES = 512;
export const MAX_TOOL_NAME_BYTES = 256;

// Output cap. A shell effect's stdout/stderr is bounded (as plain TEXT) before it
// reaches the model so a large effect cannot balloon a callback result. HYGIENE bound,
// not a redaction guarantee — the screener already denies a secret READ, so an ALLOWED
// effect's output is the model's own legitimate result. A file READ has NO broker-level
// cap: the fileop helper already bounds it to its own maxRead (1 MB) and returns valid
// base64, which dispatchFileRead forwards verbatim (re-capping encoded base64 in the
// broker would corrupt it — see dispatchFileRead).
const MAX_SHELL_OUTPUT_BYTES = 64 * 1024;
// A tool/role/skill name echoed into a diagnostic is bounded so a long name cannot
// bloat a denial message; the name already passed the byte cap, this is belt-and-braces.
const MAX_DIAGNOSTIC_CHARS = 120;

// --- Tool vocabulary (adr/1106-codex-harness.md §Hooks and enforcement) -------
// The exact stdin `tool_name`s + their Codex source aliases. `none is safely
// inferred from another`, so the code-mode names are listed explicitly rather than
// derived from the normal/v1 ones.
//
// Documented aliases only: `Write`/`Edit`/`MultiEdit` are the Codex source aliases
// for `apply_patch`; `Agent` is the alias for `spawn_agent`. `collaborationspawn_agent`
// is a DISTINCT code-mode name, not an alias of `spawn_agent`, so it is not collapsed.
const CODEX_TOOL_ALIASES: ReadonlyMap<string, string> = new Map([
  ["Write", "apply_patch"],
  ["Edit", "apply_patch"],
  ["MultiEdit", "apply_patch"],
  ["Agent", "spawn_agent"],
]);

// The five workflow signalling tools (agent/src/signals.ts:28-32). Bare names; the
// `mcp__uzi__<name>` qualified forms normalize to these. scanSignals remains the
// authoritative parser — this set is only for recognition/routing.
export const CODEX_SIGNAL_TOOLS: ReadonlySet<string> = new Set([
  "submit_plan",
  "signal_done",
  "ask_user",
  "report_progress",
  "checkpoint",
]);

// The delegation family. `spawn_agent`/`collaborationspawn_agent` START a child;
// `collaborationwait_agent`/`SubagentStart`/`SubagentStop` are the code-mode child
// lifecycle callbacks. ALL are root-only and non-nested, and all route through the
// synchronous `delegate` seam (which carries the tool name so a later milestone can
// distinguish spawn from wait/lifecycle). Recognizing them here is what keeps a
// code-mode name off the "unknown tool" deny path.
export const CODEX_DELEGATE_TOOLS: ReadonlySet<string> = new Set([
  "spawn_agent",
  "collaborationspawn_agent",
  "collaborationwait_agent",
  "SubagentStart",
  "SubagentStop",
]);

/** Stable delegated-child failures the broker permits onto its public result. Any
 *  future child runner must deliberately extend this vocabulary and its tests rather
 *  than forwarding an arbitrary provider/model-controlled code. */
const CHILD_FAILURE_CODES: ReadonlySet<string> = new Set([
  "child_failed",
  "child_aborted",
  "child_timeout",
  "child_denied",
]);

/** Closed helper/client failure vocabulary permitted into model-visible output.
 *  The Go helper owns every `E_*` code except the client's local timeout sentinel. */
const FILEOP_FAILURE_CODES: ReadonlySet<string> = new Set([
  "E_OVERSIZE",
  "E_MALFORMED",
  "E_UNKNOWN_OP",
  "E_DENIED",
  "E_ESCAPE",
  "E_SYMLINK",
  "E_NOT_FOUND",
  "E_EXISTS",
  "E_NOT_DIR",
  "E_IS_DIR",
  "E_NOT_FILE",
  "E_NOT_EMPTY",
  "E_PERM",
  "E_INTERNAL",
  "E_IO",
  "E_NO_MATCH",
  "E_AMBIGUOUS",
  "E_TIMEOUT",
]);

/** The capability the broker binds a callback to, decided from the tool NAME +
 *  grants — never from the arguments. */
type Capability = "shell" | "file_write" | "file_read" | "signal" | "delegate" | "mcp" | "unknown";

/** Canonical callback name shared by the renderer and the enforcing broker. */
export function canonicalizeCodexToolName(name: string): string {
  const alias = CODEX_TOOL_ALIASES.get(name);
  if (alias !== undefined) return alias;
  const prefix = `mcp__${SIGNAL_SERVER_NAME}__`;
  if (name.startsWith(prefix)) {
    const bare = name.slice(prefix.length);
    if (CODEX_SIGNAL_TOOLS.has(bare)) return bare;
  }
  return name;
}

function capabilityOf(canonical: string): Capability {
  if (canonical === "Bash") return "shell";
  if (canonical === "apply_patch") return "file_write";
  if (canonical === "Read") return "file_read";
  if (CODEX_SIGNAL_TOOLS.has(canonical)) return "signal";
  if (CODEX_DELEGATE_TOOLS.has(canonical)) return "delegate";
  if (canonical === "Skill" || canonical.startsWith("mcp__")) return "mcp";
  return "unknown";
}

/** Recognition shared with rendering; execution still requires an immutable grant
 *  and, for MCP, a concrete handler in the broker. */
export function isRecognizedCodexTool(canonical: string): boolean {
  return capabilityOf(canonical) !== "unknown";
}

/** The (thread, turn, call) tuple a callback is keyed by — the same identity the
 *  {@link ExecutionRegistry} reserves against. */
export interface CallbackRuntimeId {
  readonly threadId: string;
  readonly turnId: string;
  readonly callId: string;
}

/** Where the callback originated. Only a `root` (main-thread) origin may latch a
 *  workflow signal or delegate; `child` and `unknown` never can. An unrecognized
 *  value is normalized to `unknown` (fail-closed). */
export type CallbackOrigin = "root" | "child" | "unknown";

/** The neutral, bounded result the broker returns for every callback. It carries a
 *  stable `code` on denial (safe to log/persist) and NEVER a raw secret, command,
 *  or transport frame. */
export type CallbackResult =
  | { readonly ok: true; readonly output: unknown }
  | { readonly ok: false; readonly code: string; readonly message: string };

/** One fileop request, mapped to the NDJSON op protocol
 *  (agent/codex/supervisor/fileop). `path`/`newPath` are worktree-RELATIVE (the
 *  helper anchors them at the root dirfd and rejects an absolute/`..`/`.git`
 *  component itself); `data` is base64 for a write (and the REPLACEMENT text for
 *  `apply`); `old` is base64 of the text `apply` must find-and-replace. The helper
 *  reads through an openat2-pinned fd, then stages and atomically renames the result
 *  within the pinned parent directory.
 *  The `id` correlation is the production client's job, not the broker's. */
export interface FileopRequest {
  readonly op: "stat" | "read" | "write" | "apply" | "mkdir" | "rename" | "unlink" | "rmdir" | "list";
  readonly path: string;
  readonly newPath?: string;
  readonly data?: string;
  readonly old?: string;
}

/** One fileop response. `ok:false` carries a bounded error code from the helper's
 *  fixed vocabulary (E_ESCAPE/E_SYMLINK/E_DENIED/E_NOT_FILE/E_OVERSIZE/…), never a
 *  raw errno/path/content. */
export interface FileopResponse {
  readonly ok: boolean;
  readonly code?: string;
  readonly size?: number;
  readonly data?: string;
  readonly exists?: boolean;
  readonly type?: string;
  readonly entries?: ReadonlyArray<{ readonly name: string; readonly type: string }>;
  readonly truncated?: boolean;
}

/** The injected openat2 fileop client. Production talks to the fileop helper over a
 *  command-root process; a test injects a fake. */
export interface FileopClient {
  op(request: FileopRequest): Promise<FileopResponse>;
}

/** Result of running a shell effect as the command identity. */
export interface SpawnCommandResult {
  readonly code: number;
  readonly stdout: string;
  readonly stderr: string;
}

/** Options for a shell effect spawn: the working directory (the broker guarantees it is
 *  inside the worktree before calling) and the SCRUBBED command-identity env the child
 *  runs under. The executor routes its per-run scrubbed env through `env` so an injected
 *  seam records the exact env command spawns use; the default seam falls back to its
 *  closed-over env when `env` is absent. */
export interface SpawnCommandOptions {
  readonly cwd?: string;
  readonly env?: NodeJS.ProcessEnv;
  readonly signal?: AbortSignal;
}

/** The injected "run a shell command as the COMMAND identity" seam. `argv` is the
 *  full argv to run; production wraps it with `commandRootCommand` (uid 10003,
 *  setpriv cap-clear) before spawning. */
export type SpawnCommandSeam = (argv: readonly string[], opts: SpawnCommandOptions) => Promise<SpawnCommandResult>;

/** A thin memory/forge/findings/skills pass-through, keyed by tool name in
 *  {@link CodexCallbackBrokerOptions.toolHandlers}. The real factories wire in a
 *  later milestone; the broker only routes an already-authorized MCP call to it. */
export type ToolHandler = (args: unknown) => Promise<unknown>;

/** The synchronous child-thread delegation seam. The broker AWAITS it so the parent
 *  callback resolves only after the child settles. */
export interface ChildDelegationRequest {
  /** The canonical delegation tool the parent used (spawn_agent / collaboration…). */
  readonly tool: string;
  /** The requested child role (from args, re-validated against the known set). */
  readonly role: string;
  /** The raw model args; the child runner re-parses them (never trusted for authority). */
  readonly args: unknown;
  /** The parent callback's identity, for lineage/audit. */
  readonly parent: CallbackRuntimeId;
}

/** The result the delegation seam returns once the child settles. */
export interface ChildDelegationResult {
  readonly ok: boolean;
  readonly output?: unknown;
  readonly code?: string;
  readonly message?: string;
}

export type DelegateSeam = (request: ChildDelegationRequest) => Promise<ChildDelegationResult>;

/** Extra secret-path/docker inputs threaded into the shell screener, mirroring
 *  {@link screenBashCommand}'s parameters. */
export interface ScreenPolicy {
  readonly extraSecretPaths?: readonly string[];
  readonly dockerWired?: boolean;
}

/** The IMMUTABLE per-(thread,turn) grants the broker binds every effect to. Frozen
 *  for the life of the turn; the broker consults THESE, never the callback args, to
 *  decide authority. */
export interface RunGrants {
  readonly role: string;
  readonly phase: "plan" | "implement";
  /** Canonical tool names (after alias mapping) this role may invoke. */
  readonly allowedTools: ReadonlySet<string>;
  /** Skill names this role may run (checked when a skill tool is invoked). */
  readonly allowedSkills: ReadonlySet<string>;
  /** Whether this thread is the run's root/main thread (root-only authority gate). */
  readonly isRoot: boolean;
}

/** Everything the broker is constructed with: the registry, the injected seams, the
 *  immutable grants, and optional routing/screening config. */
export interface CodexCallbackBrokerOptions {
  readonly registry: ExecutionRegistry;
  readonly spawnCommand: SpawnCommandSeam;
  readonly fileop: FileopClient;
  readonly worktreePath: string;
  readonly grants: RunGrants;
  readonly delegate: DelegateSeam;
  /** memory/forge/findings/skills routing, keyed by tool name. Optional; a missing
   *  handler for an otherwise-allowed MCP tool is a DENY (fail-closed). */
  readonly toolHandlers?: ReadonlyMap<string, ToolHandler>;
  /** The known child roles delegation may target. Fail-closed: unset/empty denies
   *  every delegation as an unknown role. */
  readonly allowedRoles?: ReadonlySet<string>;
  readonly screenPolicy?: ScreenPolicy;
  readonly signal?: AbortSignal;
}

// --- small pure helpers -------------------------------------------------------

function asObject(v: unknown): Record<string, unknown> | undefined {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

/** A non-empty string field of `args`, else undefined. */
function strField(args: unknown, key: string): string | undefined {
  const v = asObject(args)?.[key];
  return typeof v === "string" && v.length > 0 ? v : undefined;
}

/** The first present non-empty string among `keys`, else undefined. */
function firstStrField(args: unknown, keys: readonly string[]): string | undefined {
  for (const k of keys) {
    const v = strField(args, k);
    if (v !== undefined) return v;
  }
  return undefined;
}

/** A string field of `args` that MAY be the empty string (e.g. an Edit `new_string`
 *  that deletes text), distinct from {@link strField}'s non-empty contract. Returns
 *  undefined only when the key is absent or not a string. */
function anyStrField(args: unknown, key: string): string | undefined {
  const v = asObject(args)?.[key];
  return typeof v === "string" ? v : undefined;
}

/** One parsed Claude-vocabulary edit: a non-empty `oldStr` to find and a (possibly
 *  empty) `newStr` to replace it with. This is the `apply_patch`/`Edit`/`MultiEdit`
 *  argument shape the renderer maps, normalized for the atomic staged `apply` op. */
interface ParsedEdit {
  readonly oldStr: string;
  readonly newStr: string;
}

/** Parse a single `{ old_string, new_string }` edit object (the Edit tool shape and
 *  each MultiEdit entry). Returns undefined when the shape is not a valid edit so the
 *  caller can fail closed with `bad_args`. `old_string` must be a non-empty string (an
 *  empty old is the create/overwrite case, which is the `content` write path, never an
 *  apply); `new_string` must be a string but MAY be empty (a deletion). */
function parseEdit(value: unknown): ParsedEdit | undefined {
  const oldStr = strField(value, "old_string");
  if (oldStr === undefined) return undefined;
  const newStr = anyStrField(value, "new_string");
  if (newStr === undefined) return undefined;
  return { oldStr, newStr };
}

function boundedString(s: string, maxBytes: number): string {
  // Enforce the byte cap in ONE shot (O(n)): encode once, and only if the encoding
  // exceeds the cap slice the RAW bytes to the cap and decode. A trailing partial
  // multi-byte sequence dropped by the slice is fine (toString renders it as the
  // replacement char). The per-char shrink loop this replaced was O(n^2) — it
  // recomputed Buffer.byteLength over the whole shrinking string every iteration,
  // and on model-controlled shell/read output that is an event-loop DoS. This helper
  // is for plain-TEXT (shell stdout/stderr) callers only; NEVER run it on base64 (a
  // byte-boundary cut there yields non-decodable text — see dispatchFileRead).
  const buf = Buffer.from(s, "utf8");
  if (buf.length <= maxBytes) return s;
  const out = buf.subarray(0, maxBytes).toString("utf8");
  return `${out}…[truncated]`;
}

/** True for a code point that must not reach a human `message`: a C0 control
 *  (0x00-0x1F, incl. TAB/LF/CR/ESC), DEL (0x7F), a C1 control (0x80-0x9F), or a
 *  Unicode bidi/format control (zero-width, line/paragraph separators, bidi
 *  embeddings/overrides/isolates, word joiner, BOM). */
function isUnsafeIdentifierChar(cp: number): boolean {
  if (cp <= 0x1f) return true; // C0 controls incl. TAB/LF/CR/ESC
  if (cp === 0x7f) return true; // DEL
  if (cp >= 0x80 && cp <= 0x9f) return true; // C1 controls
  if (cp >= 0x200b && cp <= 0x200f) return true; // ZWSP..RLM
  if (cp === 0x2028 || cp === 0x2029) return true; // line / paragraph separators
  if (cp >= 0x202a && cp <= 0x202e) return true; // bidi embeddings / overrides
  if (cp >= 0x2060 && cp <= 0x2064) return true; // word joiner..invisible separator
  if (cp >= 0x2066 && cp <= 0x206f) return true; // bidi isolates + deprecated format
  return cp === 0xfeff; // BOM / ZWNBSP
}

/** Sanitize an untrusted identifier (a model/app-server-controlled tool/role/skill
 *  name) BEFORE it is embedded in a returned human `message`: replace every control
 *  and bidi/format code point (see {@link isUnsafeIdentifierChar}) with the visible
 *  replacement char so a name carrying ESC, CR/LF, or a bidi override cannot forge or
 *  terminal-rewrite a downstream log/report line, then length-bound via `diag`. Pure
 *  and allocation-bounded (one code-point pass + one slice). The stable machine `code`
 *  is NEVER routed through this — only the human message. */
function safeId(s: string): string {
  let out = "";
  for (const ch of s) {
    const cp = ch.codePointAt(0) ?? 0;
    out += isUnsafeIdentifierChar(cp) ? "�" : ch;
  }
  return diag(out);
}

function diag(s: string): string {
  return s.length > MAX_DIAGNOSTIC_CHARS ? `${s.slice(0, MAX_DIAGNOSTIC_CHARS)}…` : s;
}

/** Case-INSENSITIVE `.git` component check. Defense-in-depth beside the path-jail's
 *  case-sensitive `.git` deny — a `.GIT`/`.Git` component on a case-insensitive
 *  filesystem still reaches the git dir. */
function referencesDotGit(relOrPath: string): boolean {
  return relOrPath.split(/[\\/]/).some((c) => c.toLowerCase() === ".git");
}

/** Recursively sort object keys so a fingerprint is independent of key order. */
function sortValue(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(sortValue);
  const o = asObject(v);
  if (!o) return v;
  const out: Record<string, unknown> = {};
  for (const k of Object.keys(o).sort()) out[k] = sortValue(o[k]);
  return out;
}

const deny = (code: string, message: string): CallbackResult => ({ ok: false, code, message });

/**
 * The worker callback broker. One instance per bound (thread, turn); its `grants`
 * are immutable for that scope. Every callback flows through {@link handleToolCall}.
 */
export class CodexCallbackBroker {
  private readonly registry: ExecutionRegistry;
  private readonly spawnCommand: SpawnCommandSeam;
  private readonly fileop: FileopClient;
  private readonly worktreePath: string;
  private readonly grants: RunGrants;
  private readonly delegateSeam: DelegateSeam;
  private readonly toolHandlers?: ReadonlyMap<string, ToolHandler>;
  private readonly allowedRoles: ReadonlySet<string>;
  private readonly extraSecretPaths: readonly string[];
  private readonly dockerWired: boolean;
  private readonly signal: AbortSignal | undefined;

  constructor(opts: CodexCallbackBrokerOptions) {
    this.registry = opts.registry;
    this.spawnCommand = opts.spawnCommand;
    this.fileop = opts.fileop;
    this.worktreePath = path.resolve(opts.worktreePath);
    this.grants = opts.grants;
    this.delegateSeam = opts.delegate;
    this.toolHandlers = opts.toolHandlers;
    this.allowedRoles = opts.allowedRoles ?? new Set<string>();
    this.extraSecretPaths = opts.screenPolicy?.extraSecretPaths ?? [];
    this.dockerWired = opts.screenPolicy?.dockerWired ?? false;
    this.signal = opts.signal;
  }

  /**
   * Admit, authorize, and execute one model-selected tool callback. Resolves only
   * AFTER the effect (or owned child) settles.
   *
   * The order is fixed and fail-closed:
   *  1. ingestion bounds (a bounded DENY, never a poison),
   *  2. admission through the registry (idempotency/poison — a fresh `ok` proceeds,
   *     a replay returns the cached terminal with NO re-execution, everything else
   *     denies),
   *  3. authority from the tool NAME + grants (args cannot widen it),
   *  4. dispatch by capability,
   * then the admitted reservation is settled with the terminal outcome.
   */
  async handleToolCall(
    rt: CallbackRuntimeId,
    name: unknown,
    args: unknown,
    origin: CallbackOrigin,
  ): Promise<CallbackResult> {
    // 1. Ingestion bounds. A malformed/oversize frame is a bounded deny; it must not
    // poison the epoch (that is reserved for a changed-payload reuse, decided by the
    // registry).
    const bounds = this.checkIngestion(rt, name);
    if (bounds) return bounds;
    const toolName = name as string; // narrowed by checkIngestion
    const org: CallbackOrigin = origin === "root" || origin === "child" ? origin : "unknown";

    // 2. Admission. The fingerprint binds (name + canonical args + origin); the same
    // tuple with a DIFFERENT fingerprint is a replay/forgery the registry poisons on.
    const fingerprint = this.fingerprint(toolName, args, org);
    const admission = this.registry.reserveCallback({
      threadId: rt.threadId,
      turnId: rt.turnId,
      callId: rt.callId,
      fingerprint,
    });
    if (admission.kind === "replay") {
      // Idempotent: return the cached terminal class, run NO second effect.
      return admission.marker.outcome === "ok"
        ? { ok: true, output: { replay: true } }
        : deny("replayed_error", "idempotent replay of a previously-settled failed callback");
    }
    if (admission.kind === "denied") {
      // changed_reuse / reservation_ceiling already poisoned in the registry;
      // admission_closed / in_flight_duplicate are plain denials. No settle: nothing
      // was admitted.
      return deny(admission.reason, `callback denied at admission (${admission.reason})`);
    }

    // 3 + 4. A fresh reservation: authorize + dispatch, then ALWAYS settle it (an
    // admitted reservation left unsettled would poison the epoch at quiesce).
    const token = admission.token;
    let result: CallbackResult;
    try {
      result = await this.authorizeAndDispatch(rt, toolName, args, org);
    } catch {
      result = deny("broker_error", "the callback failed inside the broker");
    }
    this.registry.settleCallback(token, result.ok ? "ok" : "error");
    return result;
  }

  // --- step 1: ingestion ------------------------------------------------------

  private checkIngestion(rt: CallbackRuntimeId, name: unknown): CallbackResult | undefined {
    for (const [field, value] of [
      ["threadId", rt.threadId],
      ["turnId", rt.turnId],
      ["callId", rt.callId],
    ] as const) {
      if (typeof value !== "string" || value.length === 0) {
        return deny("ingestion_rejected", `${field} must be a non-empty string`);
      }
      if (Buffer.byteLength(value, "utf8") > MAX_ID_BYTES) {
        return deny("ingestion_rejected", `${field} exceeds the ${MAX_ID_BYTES}-byte cap`);
      }
    }
    if (typeof name !== "string" || name.length === 0) {
      return deny("ingestion_rejected", "tool name must be a non-empty string");
    }
    if (Buffer.byteLength(name, "utf8") > MAX_TOOL_NAME_BYTES) {
      return deny("ingestion_rejected", `tool name exceeds the ${MAX_TOOL_NAME_BYTES}-byte cap`);
    }
    return undefined;
  }

  // --- step 2 helper: fingerprint --------------------------------------------

  private fingerprint(name: string, args: unknown, origin: CallbackOrigin): string {
    let payload: string;
    try {
      payload = JSON.stringify({ name, origin, args: sortValue(args) });
    } catch {
      // A non-serializable payload cannot be replay-distinguished; fold it to a
      // stable marker so it fails CLOSED (re-runs are treated as duplicates, never
      // re-executed) rather than throwing.
      payload = `unserializable:${name}:${origin}`;
    }
    return createHash("sha256").update(payload).digest("hex");
  }

  // --- step 3 + 4: authority and dispatch ------------------------------------

  private async authorizeAndDispatch(
    rt: CallbackRuntimeId,
    name: string,
    args: unknown,
    origin: CallbackOrigin,
  ): Promise<CallbackResult> {
    const canonical = canonicalizeCodexToolName(name);
    const cap = capabilityOf(canonical);

    // An unrecognized tool never executes: deny with a diagnostic naming it.
    if (cap === "unknown") {
      return deny("unknown_tool", `unrecognized tool "${safeId(name)}" was stripped`);
    }
    // Authority is the grant, not the args: a known tool absent from the allow-list
    // is denied even with perfectly plausible arguments.
    if (!this.grants.allowedTools.has(canonical)) {
      return deny("denied_tool", `tool "${safeId(canonical)}" is not granted to role "${safeId(this.grants.role)}"`);
    }

    switch (cap) {
      case "shell":
        return this.dispatchShell(args);
      case "file_write":
        return this.dispatchFileWrite(args);
      case "file_read":
        return this.dispatchFileRead(args);
      case "signal":
        return this.dispatchSignal(canonical, args, origin);
      case "delegate":
        return this.dispatchDelegate(rt, canonical, args, origin);
      case "mcp":
        return this.dispatchMcp(name, canonical, args);
      // `unknown` handled above.
    }
  }

  // --- shell ------------------------------------------------------------------

  private async dispatchShell(args: unknown): Promise<CallbackResult> {
    const command = strField(args, "command");
    if (command === undefined) {
      return deny("bad_args", "Bash requires a non-empty string 'command'");
    }
    const cwd = strField(args, "cwd");
    let spawnCwd = this.worktreePath;
    if (cwd !== undefined) {
      if (referencesDotGit(cwd)) return deny("path_denied", "cwd references the .git directory");
      const jail = screenToolPath(cwd, this.worktreePath, this.worktreePath, this.extraSecretPaths);
      if (jail.denied) return deny("path_denied", jail.reason ?? "cwd is outside the worktree");
      spawnCwd = path.resolve(this.worktreePath, cwd);
    }
    // Screen the command through the shared guardrail — NEVER a second copy of it.
    let screen: ReturnType<typeof screenBashCommand>;
    try {
      screen = screenBashCommand(command, this.extraSecretPaths, this.dockerWired);
    } catch {
      return deny("screen_error", "shell screening raised; denied fail-closed");
    }
    if (screen.denied) return deny("shell_denied", screen.reason ?? "denied by guardrail");

    const spawned = await this.spawnCommand(["/bin/sh", "-c", command], { cwd: spawnCwd, signal: this.signal });
    return {
      ok: true,
      output: {
        code: spawned.code,
        stdout: boundedString(spawned.stdout, MAX_SHELL_OUTPUT_BYTES),
        stderr: boundedString(spawned.stderr, MAX_SHELL_OUTPUT_BYTES),
      },
    };
  }

  // --- file effects (through the openat2 fileop client ONLY) ------------------

  /** Screen a model path with the path-jail (proc/secret/outside-worktree/.git) plus
   *  the case-insensitive `.git` defense, then relativize it for the fileop helper.
   *  Returns a DENY result on any rejection, else the worktree-relative path. */
  private screenAndRelativize(candidate: string): { rel: string } | CallbackResult {
    if (referencesDotGit(candidate)) return deny("path_denied", "path references the .git directory");
    const jail = screenToolPath(candidate, this.worktreePath, this.worktreePath, this.extraSecretPaths);
    if (jail.denied) return deny("path_denied", jail.reason ?? "path denied by the path-jail");
    const abs = path.resolve(this.worktreePath, candidate);
    const rel = path.relative(this.worktreePath, abs);
    // A case-insensitive `.git` component could survive relativization; re-check.
    if (rel === "" || rel.startsWith("..") || referencesDotGit(rel)) {
      return deny("path_denied", "path does not resolve to a file inside the worktree");
    }
    return { rel };
  }

  private async dispatchFileWrite(args: unknown): Promise<CallbackResult> {
    // A write mutates the tree; the plan phase must not (mirrors agents.ts
    // subtracting WRITE_PATH_TOOLS from the plan-turn grants).
    if (this.grants.phase === "plan") {
      return deny("write_denied_in_plan", "file writes are not permitted during the plan phase");
    }
    const candidate = firstStrField(args, ["path", "file_path"]);
    if (candidate === undefined) return deny("bad_args", "a file write requires a string 'path'");

    // The `apply_patch` capability covers the whole Claude write vocabulary the renderer
    // maps (Write/Edit/MultiEdit → apply_patch). Which EFFECT it is comes from the args,
    // but the args NEVER widen authority — the capability was already granted. Precedence:
    //   1. a full-file `content` body           → the `write` op (create-or-truncate);
    //   2. a `edits` array (MultiEdit)           → a SEQUENCE of atomic staged `apply` ops;
    //   3. a single `old_string`/`new_string`    → ONE atomic staged `apply` op.
    // EVERY branch goes through the openat2 no-symlink fileop helper — a model-selected
    // file effect never falls back to pathname check-then-use node `fs`. The `content`
    // branch stays byte-for-byte the M2 behaviour so its existing control is unaffected.
    const content = firstStrField(args, ["content", "data", "text"]);
    if (content !== undefined) {
      const screened = this.screenAndRelativize(candidate);
      if ("ok" in screened) return screened;
      const res = await this.fileop.op({
        op: "write",
        path: screened.rel,
        data: Buffer.from(content, "utf8").toString("base64"),
      });
      if (!res.ok) return this.mapFileopError(res);
      return { ok: true, output: { written: true, size: res.size } };
    }

    const edits = this.parseEdits(args);
    if (edits === undefined) {
      return deny("bad_args", "a file write requires 'content', an 'edits' array, or an 'old_string'/'new_string' pair");
    }

    const screened = this.screenAndRelativize(candidate);
    if ("ok" in screened) return screened;

    // Apply each edit as its OWN atomic staged replacement. A multi-edit is a sequence
    // of atomic edits, not one atomic transaction. The first failure stops the batch,
    // and the bounded denial explicitly reports how many earlier edits remain applied.
    let applied = 0;
    for (const edit of edits) {
      const res = await this.fileop.op({
        op: "apply",
        path: screened.rel,
        old: Buffer.from(edit.oldStr, "utf8").toString("base64"),
        data: Buffer.from(edit.newStr, "utf8").toString("base64"),
      });
      if (!res.ok) return this.mapFileopError(res, applied);
      applied += 1;
    }
    return { ok: true, output: { applied } };
  }

  /** Parse the Claude Edit/MultiEdit argument vocabulary into an ordered list of
   *  {@link ParsedEdit}s, or undefined when the args carry no valid edit. `edits` (the
   *  MultiEdit array) takes precedence over a bare `old_string`/`new_string` pair; an
   *  `edits` value that is present but not a non-empty array of valid edits is a
   *  fail-closed undefined (never silently treated as zero edits). */
  private parseEdits(args: unknown): readonly ParsedEdit[] | undefined {
    const raw = asObject(args)?.edits;
    if (raw !== undefined) {
      if (!Array.isArray(raw) || raw.length === 0) return undefined;
      const parsed: ParsedEdit[] = [];
      for (const entry of raw) {
        const edit = parseEdit(entry);
        if (edit === undefined) return undefined; // one malformed entry fails the whole batch closed
        parsed.push(edit);
      }
      return parsed;
    }
    const single = parseEdit(args);
    return single === undefined ? undefined : [single];
  }

  private async dispatchFileRead(args: unknown): Promise<CallbackResult> {
    const candidate = firstStrField(args, ["path", "file_path"]);
    if (candidate === undefined) return deny("bad_args", "a file read requires a string 'path'");

    const screened = this.screenAndRelativize(candidate);
    if ("ok" in screened) return screened;

    const res = await this.fileop.op({ op: "read", path: screened.rel });
    if (!res.ok) return this.mapFileopError(res);
    // FORWARD the helper's base64 body VERBATIM and PROPAGATE its `truncated` flag.
    // Decision (forward-verbatim, no broker-level re-cap): the fileop helper already
    // bounds a read to its own maxRead (1 MB) and returns VALID base64, so a second
    // broker cap buys nothing. Running boundedString on the ENCODED text would cut it
    // at a raw-byte boundary that is not a 4-char base64 quantum, producing
    // non-decodable garbage and silently dropping the helper's truncation signal — the
    // exact corruption this replaces. A smaller broker cap, were one ever needed, would
    // have to DECODE -> slice the raw bytes -> RE-ENCODE (never truncate the text); it
    // is not needed here, so the helper's bound stands.
    return {
      ok: true,
      output: {
        size: res.size,
        contentBase64: typeof res.data === "string" ? res.data : undefined,
        truncated: res.truncated === true,
      },
    };
  }

  /** Map a fileop response to a bounded neutral denial. Only the closed helper/client
   *  code vocabulary is echoed; a raw code, errno, path, or content never reaches it. */
  private mapFileopError(res: FileopResponse, appliedBeforeFailure?: number): CallbackResult {
    const code =
      typeof res.code === "string" && res.code.length <= 16 && FILEOP_FAILURE_CODES.has(res.code) ? res.code : "E_IO";
    const partial =
      appliedBeforeFailure === undefined
        ? ""
        : `; ${appliedBeforeFailure} edit${appliedBeforeFailure === 1 ? "" : "s"} applied before failure`;
    // `appliedBeforeFailure` is bounded by a JavaScript array length, so its decimal
    // representation is at most ten digits. No path, edit body, or raw helper error
    // crosses this neutral model-visible result.
    return deny("fileop_denied", `file operation denied (${code})${partial}`);
  }

  // --- signals (ROOT-ONLY) ----------------------------------------------------

  private dispatchSignal(canonical: string, args: unknown, origin: CallbackOrigin): CallbackResult {
    // Mirror isSubagentFrame/scanSignals authority: only the root/main origin may
    // latch a signal. A child or unknown origin can NEVER move the run's workflow.
    if (origin !== "root" || !this.grants.isRoot) {
      return deny("signal_root_only", `signal "${safeId(canonical)}" may be latched only by the root origin`);
    }
    // Reuse the AUTHORITATIVE main-thread parser rather than a second copy of the
    // clamping logic: synthesize the same assistant/tool_use frame scanSignals reads.
    const scanned = scanSignals({
      type: "assistant",
      message: {
        content: [
          {
            type: "tool_use",
            name: `mcp__${SIGNAL_SERVER_NAME}__${canonical}`,
            input: asObject(args) ?? {},
          },
        ],
      },
    });
    if (Object.keys(scanned).length === 0) {
      return deny("invalid_signal", `signal "${safeId(canonical)}" carried no valid payload`);
    }
    return { ok: true, output: scanned };
  }

  // --- delegation (ROOT-ONLY, non-nested) -------------------------------------

  private async dispatchDelegate(
    rt: CallbackRuntimeId,
    canonical: string,
    args: unknown,
    origin: CallbackOrigin,
  ): Promise<CallbackResult> {
    // Only the root may delegate; a `child` origin delegating IS the nested-
    // delegation case and is denied. `unknown` is denied for the same reason.
    if (origin !== "root" || !this.grants.isRoot) {
      return deny("delegate_root_only", "delegation may originate only from the root; nested delegation is denied");
    }
    const role = firstStrField(args, ["subagent_type", "role", "agent_type"]);
    if (role === undefined) return deny("bad_args", "delegation requires a target role");
    if (!this.allowedRoles.has(role)) {
      return deny("unknown_role", `role "${safeId(role)}" is not a known delegation target`);
    }
    // Await the child SYNCHRONOUSLY: the parent callback resolves only after it settles.
    const child = await this.delegateSeam({ tool: canonical, role, args, parent: rt });
    if (child.ok) return { ok: true, output: child.output };
    const code = child.code !== undefined && CHILD_FAILURE_CODES.has(child.code)
      ? child.code
      : "child_failed";
    const message = child.message === undefined
      ? "the delegated child failed"
      : safeId(child.message);
    return deny(code, message);
  }

  // --- memory/forge/findings/skills pass-through ------------------------------

  private async dispatchMcp(name: string, canonical: string, args: unknown): Promise<CallbackResult> {
    // A skill invocation additionally binds to the skills grant: the requested skill
    // (from args) must be in allowedSkills. This binds the effect to the grant, not
    // to the args — the args only NAME which granted skill to run.
    const isSkill = canonical === "Skill" || canonical.startsWith("mcp__skills__");
    if (isSkill) {
      const skill = firstStrField(args, ["skill", "name"]);
      if (skill === undefined) return deny("bad_args", "a skill invocation requires a skill name");
      if (!this.grants.allowedSkills.has(skill)) {
        return deny("denied_skill", `skill "${safeId(skill)}" is not granted to role "${safeId(this.grants.role)}"`);
      }
    }
    const handler = this.toolHandlers?.get(name);
    if (handler === undefined) {
      return deny("denied_tool", `no handler is wired for "${safeId(name)}"`);
    }
    try {
      const output = await handler(args);
      return { ok: true, output };
    } catch {
      return deny("handler_error", `the "${safeId(name)}" handler failed`);
    }
  }
}
