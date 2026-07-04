// Layered SDK guardrails (PRD #4 §Guardrails, primary directive).
//
// This module is the defense-in-depth layer: the agent already has no push
// credential (the worker holds the PAT and performs every authenticated git
// op), so these hooks/allowlists exist so that even a prompt-injected model
// that *tries* to push, force-mutate the repo, read credentials, or snoop the
// process table is denied at the tool boundary — before GitLab would reject it.
//
// A PreToolUse `permissionDecision: 'deny'` blocks the tool even under
// `bypassPermissions` (a deny from any hook is authoritative), which is exactly
// why the worker runs `bypassPermissions` (allow-by-default, deny-specific) plus
// this deny-hook rather than `default` (which hangs headless) or `dontAsk`
// (deny-by-default, too tight for the coder subagent's broad file/bash needs).
//
// Screening is done by a small shell tokenizer + git-command analyzer rather
// than regex-on-the-raw-string, because a raw-string matcher is trivially
// bypassed: `git -C /repo push`, `git -c x=y push`, and `sh -c 'git push'` all
// hide the real subcommand from a `/git\s+push/` regex. The analyzer splits on
// shell operators, skips git global options to reach the REAL subcommand, and
// recursively unwraps `sh -c` / `bash -c` / `eval` / `env VAR=v` wrappers.
//
// The file tools (Read/Edit/Write/Glob/Grep) get their own PreToolUse matcher
// (buildPathGuardHook): the Bash deny-list would otherwise be sidestepped by
// `Read /proc/<worker_pid>/environ` (which leaks the worker's join token), by an
// absolute path into `/etc`, or by a `..` escape out of the worktree. The path
// guard denies /proc, anything resolving outside the run worktree, and anything
// under `.git/`.
//
// Residual (accepted, documented at merge):
//  - Shell-expansion indirection a static screener cannot see through
//    (`$(printf 'g%sh' it) push`, base64|sh, variable-built commands). Out of
//    reach for any static check; the real guarantee remains that the agent
//    holds no push credential.
//  - Heredoc bodies are tokenized as if they were commands, so a benign
//    `cat <<EOF … git push … EOF` may be over-denied. That degrades SAFE (a
//    denied benign heredoc) and the agent can use Write/Edit instead.

import fs from "node:fs";
import path from "node:path";
import type { HookInput, HookJSONOutput } from "@anthropic-ai/claude-agent-sdk";
import type { Logger } from "./log.js";

/**
 * The subagent-invocation tool. Blocking it on each mapped subagent (see
 * agents.ts) is what enforces "the defined subagents can be invoked by the lead,
 * but no agent can spawn beyond them" — i.e. no nested/unbounded Agent spawning.
 * The lead keeps this tool so it can delegate to coder/reviewer/tester.
 */
export const NESTED_AGENT_TOOL = "Agent";

/**
 * Tools that let an agent DEFER work to a future turn (schedule a wakeup / a
 * session-scoped cron). Disallowed for a uzi run (wired in sdk-executor): a run
 * is a bounded task and the executor tears the agent tree down at every turn
 * boundary, so a deferred wakeup would only ever wake to a killed subagent.
 * Blocking these — together with forcing synchronous in-turn delegation
 * (buildAgentGuardHook) — makes multi-agent delegation actually work without
 * reopening B1 (no subagent survives into the worker's PAT-bearing push). (#34)
 */
export const ASYNC_DEFERRAL_TOOLS = ["ScheduleWakeup", "CronCreate"] as const;

/** Result of screening one Bash command against the deny-list. */
export interface BashScreenResult {
  denied: boolean;
  /** Static (content-free) reason when denied — safe to persist/log. */
  reason?: string;
}

// Static (content-free) reasons — never echo the attacker-influenced command.
const REASON_PUSH = "denied by guardrail: git push is not permitted (the worker opens MRs; the agent never pushes)";
const REASON_REMOTE = "denied by guardrail: git remote mutation is not permitted";
const REASON_FORCE = "denied by guardrail: forced git operations are not permitted";
const REASON_CONFIG_READ = "denied by guardrail: reading git config values is not permitted";
const REASON_CONFIG_WRITE = "denied by guardrail: modifying remote/core/http/credential git config is not permitted";
const REASON_ENV = "denied by guardrail: reading the process environment is not permitted";
const REASON_PS = "denied by guardrail: inspecting the process table is not permitted";
const REASON_PROC = "denied by guardrail: reading /proc is not permitted";
const REASON_SECRET_FILE = "denied by guardrail: reading the worker credential file is not permitted";
const REASON_DEPTH = "denied by guardrail: command wrapping is nested too deeply to screen safely";
const REASON_OUTSIDE_WORKTREE = "denied by guardrail: file access outside the run worktree is not permitted";
const REASON_DOTGIT = "denied by guardrail: accessing the .git directory is not permitted";
const REASON_UNKNOWN_SUBAGENT = "denied by guardrail: only the run's assembled subagents may be invoked";

const MAX_DEPTH = 6;

// The worker's join-token file is delivered under a Docker/k8s secret mount. A
// read-only mount can't be unlinked, so the file persists and is same-uid
// readable — and the file-tool jail only covers Read/Grep/Glob, not a Bash `cat`.
// Deny any Bash reference to the secret-mount prefix (symmetric with the /proc
// deny). Like the /proc Bash-deny this is a BAR-RAISE, not a complete close: a
// script that reads the file, a shell-indirection primitive, or another read tool
// can still reach a same-uid-readable file; the real close is the k8s uid split
// (docs/proc-hardening.md). The specific UZI_WORKER_TOKEN_FILE path (if outside
// this prefix) is added at the hook via `extraSecretPaths`.
const SECRET_PATH_PREFIXES = ["/run/secrets/"];

const SHELLS = new Set(["sh", "bash", "zsh", "dash", "ksh", "ash"]);
// Wrappers that prefix a real command; skip the wrapper (and its options) and
// analyze what follows. `env` and `eval` are handled specially below.
const GENERIC_WRAPPERS = new Set([
  "command", "builtin", "nohup", "nice", "ionice", "stdbuf", "setsid", "time",
  "xargs", "sudo", "doas", "busybox", "timeout", "chrt", "exec",
]);
const REMOTE_MUTATORS = new Set([
  "set-url", "add", "remove", "rm", "rename", "set-branches", "set-head", "prune", "update",
]);
// Force flags are denied only for subcommands that rewrite refs/history/discard
// work; local file ops (git clean -f, git add -f, …) are allowed. Force-push is
// denied unconditionally by the `push` rule above, not here.
const FORCE_DENY_SUBCOMMANDS = new Set(["checkout", "switch", "restore"]);
// File tools that carry a path and so get the worktree/`/proc`/`.git` guard.
const PATH_TOOLS = new Set(["Read", "Edit", "Write", "MultiEdit", "NotebookEdit", "Glob", "Grep"]);
// git global options that consume the following token as their value.
const GIT_VALUE_OPTS = new Set(["-c", "-C", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env"]);
const GIT_CONFIG_READ_FLAGS = new Set(["--get", "--get-all", "--get-regexp", "--get-urlmatch", "--list", "-l"]);
const GIT_CONFIG_VALUE_OPTS = new Set(["--file", "-f", "--type", "--blob"]);

const deny = (reason: string): BashScreenResult => ({ denied: true, reason });
const ALLOW: BashScreenResult = { denied: false };

/** True if `s` references any configured secret-mount path (the worker token file). */
function hitsSecret(s: string, secretPaths: readonly string[]): boolean {
  return secretPaths.some((p) => p.length > 0 && s.includes(p));
}

// --- tokenizer ---------------------------------------------------------------

type Token = { word: string } | { op: string };

/** Shell-word tokenizer: honors quotes/escapes, emits control operators as
 *  their own tokens. Quotes are stripped (so `sh -c 'git push'` yields the word
 *  `git push`); variable expansion is intentionally NOT performed. */
function tokenize(input: string): Token[] {
  const toks: Token[] = [];
  let cur = "";
  let hasCur = false;
  const flush = (): void => {
    if (hasCur) {
      toks.push({ word: cur });
      cur = "";
      hasCur = false;
    }
  };
  let i = 0;
  const n = input.length;
  while (i < n) {
    const ch = input[i]!;
    if (ch === "'") {
      hasCur = true;
      i++;
      while (i < n && input[i] !== "'") { cur += input[i]; i++; }
      i++;
      continue;
    }
    if (ch === '"') {
      hasCur = true;
      i++;
      while (i < n && input[i] !== '"') {
        if (input[i] === "\\" && i + 1 < n) {
          const nx = input[i + 1]!;
          if (nx === '"' || nx === "\\" || nx === "$" || nx === "`") { cur += nx; i += 2; continue; }
        }
        cur += input[i];
        i++;
      }
      i++;
      continue;
    }
    if (ch === "\\") {
      if (i + 1 < n) {
        const nx = input[i + 1]!;
        if (nx !== "\n") { cur += nx; hasCur = true; }
        i += 2;
        continue;
      }
      i++;
      continue;
    }
    if (ch === " " || ch === "\t" || ch === "\r") { flush(); i++; continue; }
    if (ch === "\n" || ch === ";" || ch === "(" || ch === ")" || ch === "<" || ch === ">") {
      flush();
      toks.push({ op: ch });
      i++;
      continue;
    }
    if (ch === "&") { const two = input[i + 1] === "&"; flush(); toks.push({ op: two ? "&&" : "&" }); i += two ? 2 : 1; continue; }
    if (ch === "|") { const two = input[i + 1] === "|"; flush(); toks.push({ op: two ? "||" : "|" }); i += two ? 2 : 1; continue; }
    cur += ch;
    hasCur = true;
    i++;
  }
  flush();
  return toks;
}

/** Split a token stream into per-simple-command word lists at every operator. */
function splitSegments(toks: Token[]): string[][] {
  const segs: string[][] = [];
  let cur: string[] = [];
  for (const t of toks) {
    if ("op" in t) {
      if (cur.length) { segs.push(cur); cur = []; }
    } else {
      cur.push(t.word);
    }
  }
  if (cur.length) segs.push(cur);
  return segs;
}

function basename(word: string): string {
  const idx = Math.max(word.lastIndexOf("/"), word.lastIndexOf("\\"));
  return idx >= 0 ? word.slice(idx + 1) : word;
}

/** Value of a shell `-c` option (`sh -c STR`, `bash -lc STR`), else undefined. */
function shellDashCArg(words: string[], start: number): string | undefined {
  for (let k = start; k < words.length; k++) {
    const w = words[k]!;
    if (w === "-c" || /^-[a-z]*c$/i.test(w)) return words[k + 1];
    if (w.startsWith("-c") && w.length > 2) return w.slice(2);
    if (!w.startsWith("-")) return undefined; // reached an operand before any -c
  }
  return undefined;
}

// --- analyzer ----------------------------------------------------------------

function isForceFlag(x: string): boolean {
  if (x === "--force" || x === "--force-with-lease" || x === "--force-if-includes") return true;
  // Short flag (single dash) containing a lowercase f: -f, -fd, -rf, ...
  return /^-[a-z]*f[a-z]*$/.test(x);
}

function analyzeGitConfig(rest: string[]): BashScreenResult {
  if (rest.some((x) => GIT_CONFIG_READ_FLAGS.has(x))) return deny(REASON_CONFIG_READ);
  let k = 0;
  while (k < rest.length) {
    const a = rest[k]!;
    if (GIT_CONFIG_VALUE_OPTS.has(a)) { k += 2; continue; }
    if (a.startsWith("-")) { k++; continue; } // --global/--local/--system/--add/--unset/--replace-all/…
    break;
  }
  const key = rest[k];
  // A write/unset to a remote/transport/credential namespace can repoint origin
  // or inject an auth header; an include/includeIf can pull in an attacker
  // config file that does the same; an `alias.<x> = !<shell>` body runs an
  // arbitrary command OUTSIDE the Bash screener the next time any `git <x>` runs,
  // so a write to the alias namespace is denied too (M4 audit item 8). Deny even
  // though the read flags didn't match.
  if (key && /^(remote|core|http|url|credential|include|includeif|alias)\./i.test(key)) return deny(REASON_CONFIG_WRITE);
  return ALLOW;
}

function analyzeGit(args: string[]): BashScreenResult {
  let j = 0;
  while (j < args.length) {
    const a = args[j]!;
    if (GIT_VALUE_OPTS.has(a)) { j += 2; continue; }
    if (/^--(git-dir|work-tree|exec-path|namespace|super-prefix|config-env)=/.test(a)) { j++; continue; }
    if (a.startsWith("-")) { j++; continue; } // -p, --no-pager, --bare, --exec-path, … (flags)
    break;
  }
  if (j >= args.length) return ALLOW; // bare `git`
  const sub = args[j]!.toLowerCase();
  const rest = args.slice(j + 1);
  if (sub === "push") return deny(REASON_PUSH);
  if (sub === "remote") {
    const mut = rest.find((x) => !x.startsWith("-"));
    return mut && REMOTE_MUTATORS.has(mut.toLowerCase()) ? deny(REASON_REMOTE) : ALLOW;
  }
  if (sub === "config") return analyzeGitConfig(rest);
  if (sub === "branch") {
    // Force delete (-D), force move (-M), or explicit --force/-f rewrites a ref.
    if (rest.some((x) => x === "-D" || x === "-M" || isForceFlag(x))) return deny(REASON_FORCE);
    return ALLOW;
  }
  if (FORCE_DENY_SUBCOMMANDS.has(sub) && rest.some(isForceFlag)) return deny(REASON_FORCE);
  return ALLOW;
}

/** Analyze one simple command (operator-free word list) after wrapper peeling. */
function analyzeSimple(cmd: string[], secretPaths: readonly string[]): BashScreenResult {
  if (cmd.length === 0) return ALLOW;
  if (cmd.some((w) => w.includes("/proc/"))) return deny(REASON_PROC);
  if (cmd.some((w) => hitsSecret(w, secretPaths))) return deny(REASON_SECRET_FILE);
  const base = basename(cmd[0]!).toLowerCase();
  if (base === "printenv" || base === "env") return deny(REASON_ENV);
  if (base === "ps" || base === "pgrep") return deny(REASON_PS);
  if (base === "git") return analyzeGit(cmd.slice(1));
  return ALLOW;
}

/** Peel `env`/shell/`eval`/generic wrappers, then screen the real command. */
function analyzeSegment(words: string[], depth: number, secretPaths: readonly string[]): BashScreenResult {
  let i = 0;
  while (i < words.length) {
    const base = basename(words[i]!).toLowerCase();

    if (base === "env") {
      i++;
      while (i < words.length) {
        const a = words[i]!;
        if (a === "-u") { i += 2; continue; }
        if (a === "-i" || a === "-" || a === "--" || a === "-0") { i++; continue; }
        if (a.startsWith("-")) { i++; continue; }
        if (/^[A-Za-z_][A-Za-z0-9_]*=/.test(a)) { i++; continue; } // VAR=value assignment
        break;
      }
      if (i >= words.length) return deny(REASON_ENV); // bare `env` dumps the environment
      continue;
    }

    if (SHELLS.has(base)) {
      const inner = shellDashCArg(words, i + 1);
      if (inner !== undefined) return screenWithDepth(inner, depth + 1, secretPaths);
      return ALLOW; // `bash script.sh` — the script file cannot be inspected statically
    }

    if (base === "eval") {
      return screenWithDepth(words.slice(i + 1).join(" "), depth + 1, secretPaths);
    }

    if (GENERIC_WRAPPERS.has(base)) {
      i++;
      while (i < words.length && words[i]!.startsWith("-")) i++;
      if ((base === "timeout" || base === "nice" || base === "ionice" || base === "chrt") && i < words.length && /^\d/.test(words[i]!)) i++;
      continue;
    }
    break;
  }
  return analyzeSimple(words.slice(i), secretPaths);
}

function screenWithDepth(command: string, depth: number, secretPaths: readonly string[]): BashScreenResult {
  if (depth > MAX_DEPTH) return deny(REASON_DEPTH);
  if (command.includes("/proc/")) return deny(REASON_PROC);
  if (hitsSecret(command, secretPaths)) return deny(REASON_SECRET_FILE);
  for (const seg of splitSegments(tokenize(command))) {
    const r = analyzeSegment(seg, depth, secretPaths);
    if (r.denied) return r;
  }
  return ALLOW;
}

/**
 * Screen a single Bash command string against the deny-list. Pure and
 * synchronous so the guardrail suite can assert it directly with NO live
 * Anthropic session. `extraSecretPaths` are additional worker-credential file
 * paths to deny (the configured UZI_WORKER_TOKEN_FILE), on top of the built-in
 * `/run/secrets/` secret-mount prefix.
 */
export function screenBashCommand(command: string, extraSecretPaths: readonly string[] = []): BashScreenResult {
  return screenWithDepth(command, 0, [...SECRET_PATH_PREFIXES, ...extraSecretPaths]);
}

/** Extract the `command` field from a Bash tool_input, if present. */
function bashCommandOf(toolInput: unknown): string | undefined {
  if (toolInput && typeof toolInput === "object" && "command" in toolInput) {
    const cmd = (toolInput as { command?: unknown }).command;
    if (typeof cmd === "string") return cmd;
  }
  return undefined;
}

/**
 * Build the PreToolUse hook callback. Fires (with `matcher: 'Bash'`) before any
 * Bash tool runs; a matching command is denied with a static reason. Anything
 * that is not a Bash tool call, or a Bash command that passes the deny-list,
 * returns no decision (the tool proceeds under `bypassPermissions`).
 *
 * `extraSecretPaths` are worker-credential file paths (the configured
 * UZI_WORKER_TOKEN_FILE) to deny a Bash read of, on top of the built-in
 * `/run/secrets/` prefix.
 */
export function buildPreToolUseHook(
  log: Logger,
  extraSecretPaths: readonly string[] = [],
): (input: HookInput) => Promise<HookJSONOutput> {
  return async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse" || input.tool_name !== "Bash") {
      return {};
    }
    const command = bashCommandOf(input.tool_input);
    if (command === undefined) return {};

    const screen = screenBashCommand(command, extraSecretPaths);
    if (!screen.denied) return {};

    // Log the denial (reason only — never the command) so an operator can see
    // the guardrail firing without the attacker-influenced text reaching logs.
    log.warn("guardrail denied a Bash command", { reason: screen.reason });
    return {
      hookSpecificOutput: {
        hookEventName: "PreToolUse",
        permissionDecision: "deny",
        permissionDecisionReason: screen.reason ?? "denied by guardrail",
      },
    };
  };
}

/** The `subagent_type` an Agent tool_use targets, if present. */
function subagentTypeOf(toolInput: unknown): string | undefined {
  if (toolInput && typeof toolInput === "object" && "subagent_type" in toolInput) {
    const t = (toolInput as { subagent_type?: unknown }).subagent_type;
    if (typeof t === "string" && t.length > 0) return t;
  }
  return undefined;
}

/**
 * Build the PreToolUse hook for the Agent (subagent-invocation) tool. It does two
 * things:
 *
 *  1. hard-fails any invocation whose `subagent_type` is not one of the run's
 *     assembled subagents (M4 audit item 7). The SDK's built-in `general-purpose`
 *     agent AUGMENTS our programmatic `agents` map and is otherwise invokable
 *     unbounded — an allow-list of exactly our subagent names denies it (and any
 *     typo/hallucinated role): defense-in-depth + cost control.
 *
 *  2. for an ALLOWED subagent, rewrites the input to `run_in_background: false`,
 *     forcing SYNCHRONOUS in-turn delegation (#34). The SDK's Agent tool runs
 *     subagents in the BACKGROUND by default, but the executor drives one turn to
 *     its result frame then abort()s + group-kills the whole agent tree (B1) — so
 *     a backgrounded subagent is terminated before it does any work and delegation
 *     silently no-ops (the live run burned iterations on this). Running the
 *     subagent synchronously makes it complete IN-TURN, before the turn-end reap,
 *     so delegation works AND B1 stays closed (no survivor into the PAT push).
 *
 * The lead keeps the Agent tool to delegate to the allowed roles; every subagent
 * already carries `disallowedTools:['Agent']`, so this hook only ever sees the
 * lead's calls.
 */
export function buildAgentGuardHook(
  allowed: Iterable<string>,
  log: Logger,
): (input: HookInput) => Promise<HookJSONOutput> {
  const allowSet = new Set(allowed);
  return async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse" || input.tool_name !== NESTED_AGENT_TOOL) return {};
    const sub = subagentTypeOf(input.tool_input);
    if (sub === undefined || !allowSet.has(sub)) {
      log.warn("guardrail denied an unexpected subagent", { subagent_type: sub ?? null });
      return {
        hookSpecificOutput: {
          hookEventName: "PreToolUse",
          permissionDecision: "deny",
          permissionDecisionReason: REASON_UNKNOWN_SUBAGENT,
        },
      };
    }
    // Allowed subagent: force synchronous delegation. Already-synchronous calls
    // pass through untouched.
    const original = input.tool_input && typeof input.tool_input === "object"
      ? (input.tool_input as Record<string, unknown>)
      : {};
    if (original["run_in_background"] === false) return {};
    return {
      hookSpecificOutput: {
        hookEventName: "PreToolUse",
        updatedInput: { ...original, run_in_background: false },
      },
    };
  };
}

/** Path-bearing fields across the file tools (file_path / path / notebook_path). */
function extractToolPaths(toolInput: unknown): string[] {
  if (!toolInput || typeof toolInput !== "object") return [];
  const rec = toolInput as Record<string, unknown>;
  const out: string[] = [];
  for (const key of ["file_path", "path", "notebook_path"]) {
    const v = rec[key];
    if (typeof v === "string" && v.length > 0) out.push(v);
  }
  return out;
}

/**
 * Screen a single file-tool path against the run worktree. Relative paths resolve
 * against `cwd` (the tool's working dir), and containment is checked against the
 * worktree root — so a `..` escape, an absolute path into `/etc` or `/proc`, or
 * a path under `.git/` is denied.
 *
 * The M3 jail was purely lexical (path.resolve), so an in-worktree symlink to
 * /proc or outside the worktree slipped the check: `link -> /proc`, then
 * `Read link/1/environ` resolves lexically inside the worktree. M4 audit item 6
 * closes this for the file tools by ALSO resolving symlinks on the existing
 * portion of the path (realpath-when-exists) and re-checking. Non-existent paths
 * (a Write target being created) keep the lexical check, so the function stays
 * pure for those and the unit suite can assert it without touching disk.
 *
 * Residual (documented, k8s-phase fix): this only guards the FILE tools. A Bash
 * `cat symlink/1/environ` still bypasses both this and the Bash `/proc/` string
 * guard, because worker and agent share a uid and /proc/<pid>/environ is
 * owner-readable. Two things are readable that way:
 *   - the WORKER's own environ, which holds the join token (redacted from every
 *     message; the PAT is never in the worker's persistent env); and
 *   - during the worker's git push/MR, the PAT lives in a git CHILD's env — but
 *     the executor group-kills the agent's subprocess tree BEFORE the push (B1,
 *     see sdk-executor.killAgentTree), so a normal survivor is already dead. A
 *     survivor that escaped into its OWN session (`setsid`) is not reached by
 *     killing the CLI's group and could still race that window.
 * The real structural close (different uid for the agent vs the worker's git
 * ops / userns / hidepid=2 / gVisor) belongs to the remote-worker phase; see the
 * header of docker-compose's agent service.
 */
export function screenToolPath(candidate: string, worktreeRoot: string, cwd: string): BashScreenResult {
  const root = path.resolve(worktreeRoot);
  const lexical = path.resolve(cwd || root, candidate);
  const lexicalResult = classifyResolvedPath(candidate, lexical, root);
  if (lexicalResult.denied) return lexicalResult;
  // Resolve symlinks on the existing prefix and re-check, so a symlink that
  // lexically stays in-worktree but points at /proc or outside is caught. Both
  // sides must be realpath'd (N2): comparing a realpath'd candidate against a
  // lexical root over-DENIES every real file when the worktree root itself sits
  // under a symlink ancestor (e.g. macOS /var → /private/var, or a symlinked
  // data volume) — a fail-closed asymmetry, not a security hole, but fragile.
  const real = realpathExisting(lexical);
  if (real === lexical) return ALLOW;
  return classifyResolvedPath(candidate, real, realpathExisting(root));
}

/** Deny a resolved absolute path that reads /proc, escapes the worktree, or hits .git/. */
function classifyResolvedPath(candidate: string, resolved: string, root: string): BashScreenResult {
  if (candidate.includes("/proc/") || resolved === "/proc" || resolved.startsWith("/proc/")) return deny(REASON_PROC);
  const inRoot = resolved === root || resolved.startsWith(root + path.sep);
  if (!inRoot) return deny(REASON_OUTSIDE_WORKTREE);
  const gitDir = path.join(root, ".git");
  if (resolved === gitDir || resolved.startsWith(gitDir + path.sep)) return deny(REASON_DOTGIT);
  return ALLOW;
}

/**
 * Resolve symlinks on the existing portion of `p`, re-appending any non-existent
 * remainder, so a symlink whose target does not (yet) exist is still followed —
 * e.g. `link -> /proc` resolves even on a host without /proc. Done manually
 * (deepest existing ancestor → readlink one hop → repeat) rather than via
 * `fs.realpathSync`, which throws on a dangling link and would leave the escape
 * unresolved. Bounded hops guard against a symlink cycle; any fs error falls back
 * to what is resolved so far (the lexical check already ran).
 */
function realpathExisting(p: string): string {
  let current = path.resolve(p);
  for (let hops = 0; hops < 64; hops++) {
    const found = deepestExisting(current);
    if (!found) return current;
    let st: fs.Stats;
    try {
      st = fs.lstatSync(found.path);
    } catch {
      return current;
    }
    if (st.isSymbolicLink()) {
      let target: string;
      try {
        target = fs.readlinkSync(found.path);
      } catch {
        return current;
      }
      const abs = path.resolve(path.dirname(found.path), target);
      current = found.tail.length ? path.join(abs, ...found.tail) : abs;
      continue; // the target may itself contain further symlinks
    }
    // Deepest existing component is a real file/dir: realpath it (resolves any
    // intermediate symlinks in the prefix) and re-append the non-existent tail.
    try {
      const real = fs.realpathSync.native(found.path);
      return found.tail.length ? path.join(real, ...found.tail) : real;
    } catch {
      return current;
    }
  }
  return current;
}

/** The deepest lstat-existing ancestor of `p` plus the non-existent tail below it. */
function deepestExisting(p: string): { path: string; tail: string[] } | undefined {
  let prefix = p;
  const tail: string[] = [];
  for (;;) {
    try {
      fs.lstatSync(prefix);
      return { path: prefix, tail };
    } catch {
      const parent = path.dirname(prefix);
      if (parent === prefix) return undefined;
      tail.unshift(path.basename(prefix));
      prefix = parent;
    }
  }
}

/**
 * Build the PreToolUse hook for the path-bearing file tools. Denies any path
 * that reads /proc, escapes the run worktree, or touches `.git/`, closing the
 * sibling-tool bypass of the Bash `/proc` deny. Non-path tools and in-worktree
 * paths return no decision (the tool proceeds under `bypassPermissions`).
 */
export function buildPathGuardHook(
  worktreeRoot: string,
  log: Logger,
): (input: HookInput) => Promise<HookJSONOutput> {
  return async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse" || !PATH_TOOLS.has(input.tool_name)) return {};
    const cwd = typeof input.cwd === "string" && input.cwd ? input.cwd : worktreeRoot;
    for (const candidate of extractToolPaths(input.tool_input)) {
      const screen = screenToolPath(candidate, worktreeRoot, cwd);
      if (screen.denied) {
        log.warn("guardrail denied a file-tool path", { tool: input.tool_name, reason: screen.reason });
        return {
          hookSpecificOutput: {
            hookEventName: "PreToolUse",
            permissionDecision: "deny",
            permissionDecisionReason: screen.reason ?? "denied by guardrail",
          },
        };
      }
    }
    return {};
  };
}
