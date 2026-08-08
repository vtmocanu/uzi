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
//  - Docker secret theft via bind-mount remap is NOT contained by this screener
//    (PRD #83 auditor B4). The docker rule below denies docker ENTIRELY when no
//    daemon is wired; on a WIRED worker it allows docker, and a literal
//    `docker run -v /run/secrets/worker_token:/x` is still caught by the secret-path
//    check — but that is incidental and must NEVER be leaned on: `-v /run:/x` or
//    `-v /:/x` (mount a parent) evade any substring match. Do NOT try to parse `-v`
//    partially — it is unwinnable. The SOLE containment on a wired worker is
//    Decision 3 (the DinD daemon runs in a SEPARATE container whose own mount
//    namespace mounts none of the worker's secret/`/data`/`/nix`, so `-v <anything>:/x`
//    binds the DinD fs, which holds none of it). The guardrail's only docker job is
//    "deny when no daemon is wired".
//  - Docker daemon-redirect (B5) is caught for the INLINE + segment-LOCAL forms only
//    (`DOCKER_HOST=`/`DOCKER_CONTEXT=` prefix, `-H`/`--host`/`-c`/`--context` flags,
//    `docker context use|create|update`, and an `export`/`declare` of DOCKER_HOST/
//    DOCKER_CONTEXT). NOT caught: STATEFUL propagation across statements — `set -a` +
//    a later bare `DOCKER_HOST=…`, or `export DOCKER_HOST=…` in one `;`-segment affecting
//    a `docker` in a later segment of the SAME shell (the analyzer screens each segment
//    independently and does not model exported-var state). This is an intentional residual
//    (both reviewer + auditor rated LOW, no exploit path on the real topology): B5 is
//    defense-in-depth to keep a wired worker on ITS OWN sidecar, and mount-ns (Decision 3)
//    — not this redirect check — is the containment. A modelling of shell env state is out
//    of scope and would risk the tokenizer.

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
 * The teammate-messaging tool. A subagent that addresses the orchestrator by one
 * of the lead's colloquial names (`lead`/`orchestrator`/`team-lead`) gets the
 * SDK's "No agent named '<x>' is reachable" error — the reachable recipient is
 * `main`. buildSendMessageAliasHook rewrites those aliases to `main` (defense in
 * depth for repo-sourced + user-authored agents and future template drift; #210
 * fixed the builtin templates).
 */
export const SEND_MESSAGE_TOOL = "SendMessage";

/**
 * Recipient names that all denote the orchestrator/main thread. Derived floor:
 * LEAD_NAME_RE's spellings in agents.ts (`lead`/`orchestrator`) plus the two
 * measured defect spellings from api/internal/agenttmpl/recipient_test.go
 * (`team-lead`, `team lead`). Matched trim + case-insensitive + EXACT on the whole
 * trimmed value — NEVER a substring, which would clobber legitimate recipients
 * like `team-lead-reviewer` or `co-lead`.
 */
const SEND_MESSAGE_LEAD_ALIASES = new Set(["lead", "orchestrator", "team-lead", "team lead"]);
/** The one reachable orchestrator recipient the aliases above route to. */
const SEND_MESSAGE_MAIN = "main";

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

/**
 * The file tools that WRITE: the write subset of PATH_TOOLS below, which is the
 * authoritative list of tools that carry a path. Exported because agents.ts
 * subtracts exactly this set from every subagent's grant on the PLAN turn (#203),
 * and a hand-written copy of it there would silently go stale the day the SDK
 * gains another write tool. PATH_TOOLS is built from this constant for the same
 * reason.
 *
 * It does NOT follow that no stale copy exists. Two hook `matcher` literals spell
 * the path-tool list out independently, and they are what decides whether the
 * path jail runs at all — see the comment on PATH_TOOLS below, which names them.
 * Adding a tool here is a three-file change, not a one-file change.
 *
 * NOT a filesystem-write guarantee, and must never be described as one: every
 * write-capable role also declares `Bash`, the plan turn runs under
 * `bypassPermissions`, and this file has NO filesystem-write rule of any kind —
 * `echo 'x' > f`, `sed -i`, `tee` and `git apply` are all allowed. Subtracting
 * these removes the ergonomic path and the model's awareness of it, one layer
 * better than a prompt instruction. The integrity property (a `git status` check
 * at the gate, which catches an `Edit` and an `echo >` identically) is issue #212.
 */
export const WRITE_PATH_TOOLS = ["Edit", "Write", "MultiEdit", "NotebookEdit"] as const;

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
// PRD #83 M1 (Q3): docker is inert without a daemon sidecar wired (DOCKER_HOST/keystone
// resolver). Content-free — never echo the command.
const REASON_DOCKER_NO_DAEMON = "denied by guardrail: docker requires a daemon sidecar, which this worker has none wired";
// PRD #83 M1 (auditor B5): even a WIRED worker may reach only ITS own sidecar — an
// inline -H/--host, a DOCKER_HOST=… prefix, or `docker context use/create` redirects
// the client to a different daemon and is denied. Defense-in-depth, NOT containment.
const REASON_DOCKER_REDIRECT = "denied by guardrail: redirecting the docker client to a different daemon is not permitted";
const REASON_DEPTH = "denied by guardrail: command wrapping is nested too deeply to screen safely";
const REASON_OUTSIDE_WORKTREE = "denied by guardrail: file access outside the run worktree is not permitted";
const REASON_DOTGIT = "denied by guardrail: accessing the .git directory is not permitted";
const REASON_UNKNOWN_SUBAGENT = "denied by guardrail: only the run's assembled subagents may be invoked";

const MAX_DEPTH = 6;

// The worker's join-token file is delivered under a Docker/k8s secret mount, forced
// to 0400 worker-owned and persisted (not unlinked). Deny any Bash reference to the
// secret-mount prefix (symmetric with the /proc deny). Under the PRD #51 A1 split the
// agent runs as the cap-less `runner` uid and the file is worker-owned, so a `runner`
// read is already denied by the UID BOUNDARY — this Bash deny is now defense-in-depth
// there (and remains the primary bar-raise on a #58 single-uid start, where there is no
// split; see docs/proc-hardening.md). The specific UZI_WORKER_TOKEN_FILE path (if
// outside this prefix) is added at the hook via `extraSecretPaths`.
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
// docker CLI basenames the wired/deny rule keys on (PRD #83 Q3). `docker compose …`
// reduces to the `docker` base; `docker-compose …` (the legacy v1 wrapper) is its own
// basename, so both are listed.
const DOCKER_BASES = new Set(["docker", "docker-compose"]);
// docker GLOBAL flags (before the subcommand) that CONSUME a following value token, so
// the value is never mistaken for the subcommand while scanning for `context` (B5). The
// endpoint-redirecting globals (-H/--host/-c/--context) are denied outright below, so
// they are deliberately NOT here.
const DOCKER_GLOBAL_VALUE_FLAGS = new Set(["-l", "--log-level", "--config", "--tlscacert", "--tlscert", "--tlskey"]);
// Shell builtins that EXPORT a variable into the environment of LATER commands in the same
// shell — the B5 compound redirect form `export DOCKER_HOST=…; docker …`.
const EXPORT_BUILTINS = new Set(["export", "declare", "typeset", "local"]);
// File tools that carry a path and so get the worktree/`/proc`/`.git` guard.
// Built from WRITE_PATH_TOOLS (defined above, exported) plus the read-only ones,
// so THIS set and the plan-turn subtraction in agents.ts cannot drift.
//
// THAT IS TWO OF THREE COPIES, AND THE THIRD IS THE ONE THAT GATES THIS ONE.
// The hook's `matcher` decides whether the guard is invoked at all; the
// `PATH_TOOLS.has(...)` test below is a SECOND filter that only ever sees names
// the matcher already admitted. Two hand-written matcher literals spell the list
// out and derive from nothing:
//
//   agent/src/sdk-executor.ts    matcher: "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep"
//   agent/src/chat-executor.ts   matcher: "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep"
//
// So adding a fifth write tool to WRITE_PATH_TOOLS grows this set and grows the
// plan-turn subtraction, and THE PATH JAIL STILL DOES NOT REACH THE NEW TOOL,
// because neither matcher mentions it. The three lists agree today; nothing
// enforces that. Update all three, or derive the matchers from this constant.
const PATH_TOOLS = new Set<string>(["Read", "Glob", "Grep", ...WRITE_PATH_TOOLS]);
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
  // `filter.<x>.clean/smudge` bodies run as a shell command on checkout/add via a
  // matching .gitattributes — a second code-exec route, so deny writes to it too
  // (M10 audit, defense in depth alongside the worker's core.hooksPath neutralization).
  if (key && /^(remote|core|http|url|credential|include|includeif|alias|filter)\./i.test(key)) return deny(REASON_CONFIG_WRITE);
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

/** An inline `-H`/`--host` endpoint override (B5). Precise so `docker run --hostname foo`
 *  (a benign flag that shares the `--host` prefix) is NOT caught. */
function isDockerHostFlag(a: string): boolean {
  return a === "-H" || a === "--host" || a.startsWith("--host=") || (a.startsWith("-H") && a.length > 2);
}

/** An inline `-c`/`--context` context override (B5). `--config`/`--configFoo` don't
 *  match the `-c` glued form (they start with `--`), and `docker run -c 512` is safe
 *  because `-c` after the subcommand is never scanned here (see analyzeDocker). */
function isDockerContextFlag(a: string): boolean {
  return a === "-c" || a === "--context" || a.startsWith("--context=") || (a.startsWith("-c") && a.length > 2 && !a.startsWith("--"));
}

/** A leading shell `VAR=value` env-assignment word. */
function isAssignment(word: string): boolean {
  return /^[A-Za-z_][A-Za-z0-9_]*=/.test(word);
}

/** A `DOCKER_HOST=…`/`DOCKER_CONTEXT=…` assignment — either points the docker client at a
 *  different daemon endpoint (B5). */
function redirectsDockerEnv(word: string): boolean {
  return /^(DOCKER_HOST|DOCKER_CONTEXT)=/.test(word);
}

/**
 * Screen a `docker`/`docker-compose` simple command (PRD #83 Q3 + auditor B5).
 * `assignments` are the `VAR=value` env prefixes peeled by analyzeSegment (bare and
 * `env`-wrapper forms, incl. those inherited through an `sh -c`/`eval` wrapper).
 *
 *  - No daemon wired ⇒ deny outright (the guardrail's ONLY docker containment role).
 *  - Wired ⇒ allow, EXCEPT an inline daemon redirect (a `DOCKER_HOST=`/`DOCKER_CONTEXT=`
 *    prefix, an `-H`/`--host` GLOBAL flag, an `-c`/`--context` GLOBAL flag, or
 *    `context use/create/update`) so a wired worker reaches only ITS OWN sidecar. This is
 *    defense-in-depth, never the containment — mount-ns (Decision 3) is (see header, B4).
 */
function analyzeDocker(cmd: string[], assignments: readonly string[], dockerWired: boolean): BashScreenResult {
  if (!dockerWired) return deny(REASON_DOCKER_NO_DAEMON);
  if (assignments.some(redirectsDockerEnv)) return deny(REASON_DOCKER_REDIRECT);
  const args = cmd.slice(1);
  let i = 0;
  while (i < args.length) {
    const a = args[i]!;
    if (isDockerHostFlag(a) || isDockerContextFlag(a)) return deny(REASON_DOCKER_REDIRECT);
    if (!a.startsWith("-")) break; // reached the subcommand
    if (DOCKER_GLOBAL_VALUE_FLAGS.has(a)) { i += 2; continue; } // benign global; skip its value
    i++;
  }
  if (args[i]?.toLowerCase() === "context") {
    const op = args.slice(i + 1).find((x) => !x.startsWith("-"))?.toLowerCase();
    if (op === "use" || op === "create" || op === "update") return deny(REASON_DOCKER_REDIRECT);
  }
  return ALLOW;
}

/** Analyze one simple command (operator-free word list) after wrapper + leading-
 *  assignment peeling. `assignments` are the peeled `VAR=value` prefixes (from
 *  analyzeSegment), inspected only for a DOCKER_HOST= daemon redirect (B5). */
function analyzeSimple(cmd: string[], secretPaths: readonly string[], dockerWired: boolean, assignments: readonly string[]): BashScreenResult {
  if (cmd.length === 0) return ALLOW; // only env assignments, no command word — runs nothing
  if (cmd.some((w) => w.includes("/proc/"))) return deny(REASON_PROC);
  if (cmd.some((w) => hitsSecret(w, secretPaths))) return deny(REASON_SECRET_FILE);
  const base = basename(cmd[0]!).toLowerCase();
  if (base === "printenv" || base === "env") return deny(REASON_ENV);
  if (base === "ps" || base === "pgrep") return deny(REASON_PS);
  if (base === "git") return analyzeGit(cmd.slice(1));
  if (DOCKER_BASES.has(base)) return analyzeDocker(cmd, assignments, dockerWired);
  // B5 compound form: `export DOCKER_HOST=…; docker …` / `declare -x DOCKER_CONTEXT=…`
  // marks a daemon/context redirect for a later docker in the SAME shell. Deny the export
  // itself, but only on a WIRED worker (unwired, docker is already denied outright, so the
  // export is inert). Segment-local: `set -a` + cross-`;` assignment PROPAGATION is a
  // documented residual (see file header), not caught — mount-ns is the containment.
  if (dockerWired && EXPORT_BUILTINS.has(base) && cmd.slice(1).some(redirectsDockerEnv)) {
    return deny(REASON_DOCKER_REDIRECT);
  }
  return ALLOW;
}

/**
 * Peel leading `VAR=value` env-assignments + `env`/shell/`eval`/generic wrappers, then
 * screen the real command. Assignments are peeled at EACH leading position (before AND
 * after a generic wrapper), so they compose with the wrapper peel: `FOO=bar sh -c 'git
 * push'`, `DOCKER_HOST=x sudo docker …`, and `env DOCKER_HOST=x docker …` all reduce
 * correctly. This also closes the pre-existing `FOO=bar git push` evasion (a bare
 * assignment used to leave the base as `foo=bar`, an unknown command that slipped every
 * deny) — a strict tightening, never a loosening. Captured assignments ride down to the
 * docker analyzer for the B5 DOCKER_HOST= redirect check, INCLUDING across an `sh -c`/
 * `eval` wrapper (a prefix assignment is exported to that subshell).
 */
function analyzeSegment(words: string[], depth: number, secretPaths: readonly string[], dockerWired: boolean, inherited: readonly string[]): BashScreenResult {
  const assignments: string[] = [...inherited];
  let i = 0;
  while (i < words.length) {
    if (isAssignment(words[i]!)) { assignments.push(words[i]!); i++; continue; }
    const base = basename(words[i]!).toLowerCase();

    if (base === "env") {
      i++;
      while (i < words.length) {
        const a = words[i]!;
        if (a === "-u") { i += 2; continue; }
        if (a === "-i" || a === "-" || a === "--" || a === "-0") { i++; continue; }
        if (a.startsWith("-")) { i++; continue; }
        if (isAssignment(a)) { assignments.push(a); i++; continue; } // `env DOCKER_HOST=x …` counts for B5
        break;
      }
      if (i >= words.length) return deny(REASON_ENV); // bare `env` dumps the environment
      continue;
    }

    if (SHELLS.has(base)) {
      const inner = shellDashCArg(words, i + 1);
      // A prefix env-assignment is exported to the subshell, so carry it into the inner
      // screen (`DOCKER_HOST=x sh -c 'docker ps'` must still see the redirect).
      if (inner !== undefined) return screenWithDepth(inner, depth + 1, secretPaths, dockerWired, assignments);
      return ALLOW; // `bash script.sh` — the script file cannot be inspected statically
    }

    if (base === "eval") {
      return screenWithDepth(words.slice(i + 1).join(" "), depth + 1, secretPaths, dockerWired, assignments);
    }

    if (GENERIC_WRAPPERS.has(base)) {
      i++;
      while (i < words.length && words[i]!.startsWith("-")) i++;
      if ((base === "timeout" || base === "nice" || base === "ionice" || base === "chrt") && i < words.length && /^\d/.test(words[i]!)) i++;
      continue;
    }
    break;
  }
  return analyzeSimple(words.slice(i), secretPaths, dockerWired, assignments);
}

function screenWithDepth(
  command: string,
  depth: number,
  secretPaths: readonly string[],
  dockerWired: boolean,
  assignments: readonly string[] = [],
): BashScreenResult {
  if (depth > MAX_DEPTH) return deny(REASON_DEPTH);
  if (command.includes("/proc/")) return deny(REASON_PROC);
  if (hitsSecret(command, secretPaths)) return deny(REASON_SECRET_FILE);
  for (const seg of splitSegments(tokenize(command))) {
    const r = analyzeSegment(seg, depth, secretPaths, dockerWired, assignments);
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
 *
 * `dockerWired` (PRD #83 Q3) is a PARAMETER, never read from `process.env` inside
 * this analyzer (auditor B2 — the screener stays pure): the worker resolves docker
 * wiring ONCE at startup (docker-wiring.ts) and passes the resolved boolean here.
 * false (the default) DENIES docker entirely; true allows it (minus daemon redirects).
 */
export function screenBashCommand(
  command: string,
  extraSecretPaths: readonly string[] = [],
  dockerWired = false,
): BashScreenResult {
  return screenWithDepth(command, 0, [...SECRET_PATH_PREFIXES, ...extraSecretPaths], dockerWired);
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
 *
 * `dockerWired` (PRD #83 Q3) is resolved ONCE at worker startup (docker-wiring.ts) and
 * frozen into the hook here — the screener never reads env (auditor B2). false ⇒ docker
 * is denied; true ⇒ allowed (minus daemon redirects).
 */
export function buildPreToolUseHook(
  log: Logger,
  extraSecretPaths: readonly string[] = [],
  dockerWired = false,
): (input: HookInput) => Promise<HookJSONOutput> {
  return async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse" || input.tool_name !== "Bash") {
      return {};
    }
    const command = bashCommandOf(input.tool_input);
    if (command === undefined) return {};

    const screen = screenBashCommand(command, extraSecretPaths, dockerWired);
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

/** The recipient (`to`) an SDK SendMessage tool_use targets, if present. */
function messageRecipientOf(toolInput: unknown): string | undefined {
  if (toolInput && typeof toolInput === "object" && "to" in toolInput) {
    const t = (toolInput as { to?: unknown }).to;
    if (typeof t === "string" && t.length > 0) return t;
  }
  return undefined;
}

/**
 * Build the PreToolUse hook that aliases a SendMessage addressed to the lead's
 * colloquial names (`lead`/`orchestrator`/`team-lead`/`team lead`) onto the one
 * reachable orchestrator recipient, `main`. Repo-sourced and user-authored agents
 * are the unprotected surface this covers — the builtin templates already say
 * "SendMessage to `main`" (#210, guarded by recipient_test.go), so this is pure
 * defense in depth for those.
 *
 * CLOBBER-GUARD: the rewrite fires ONLY when no registered subagent literally owns
 * the aliased name (case-insensitive). The repo-source path (subagentsFromTemplates,
 * agents.ts) can register a REAL invokable subagent named `lead`/`orchestrator`/
 * `team-lead` (recipient_test.go); an unconditional rewrite would silently misroute
 * a legitimate call to it. `allowedSubagents` is the registered-name set, threaded
 * in by the executor exactly like buildAgentGuardHook's `allowed`.
 *
 * Like every hook here the matcher is only a pre-filter, so the callback re-checks
 * the event + tool name and returns `{}` (no decision) for anything else. It only
 * ever rewrites the `to` field; it never denies.
 */
export function buildSendMessageAliasHook(
  allowedSubagents: Iterable<string>,
  log: Logger,
): (input: HookInput) => Promise<HookJSONOutput> {
  const registered = new Set([...allowedSubagents].map((n) => n.trim().toLowerCase()));
  return async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse" || input.tool_name !== SEND_MESSAGE_TOOL) return {};
    const to = messageRecipientOf(input.tool_input);
    if (to === undefined) return {};
    const norm = to.trim().toLowerCase();
    // Only a lead alias, and only when no registered subagent owns that exact name.
    if (!SEND_MESSAGE_LEAD_ALIASES.has(norm) || registered.has(norm)) return {};
    const original = input.tool_input && typeof input.tool_input === "object"
      ? (input.tool_input as Record<string, unknown>)
      : {};
    log.debug("guardrail aliased a SendMessage recipient to main", { alias: norm });
    return {
      hookSpecificOutput: {
        hookEventName: "PreToolUse",
        updatedInput: { ...original, to: SEND_MESSAGE_MAIN },
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
 * Layering (PRD #51): this guards the FILE tools; a Bash `cat symlink/1/environ`
 * would bypass both it and the Bash `/proc/` string guard. Under the A1 split that no
 * longer reaches a secret — the agent runs as the cap-less `runner` uid, so the
 * WORKER's own /proc/environ (join token) and, during the worker's git push/MR, the
 * git CHILD's /proc/environ (PAT) — both 0400 worker-owned — are UNREADABLE to the
 * runner. So this path guard and the Bash /proc deny are now defense-in-depth on the
 * A1 path (they stay the primary bar-raise on a #58 single-uid start, where there is no
 * split). B1 (the executor reaps the agent tree via a setpriv-to-runner kill BEFORE the
 * push, sdk-executor.killAgentTree) remains a second layer against a `setsid`-escaped
 * survivor. The cross-container k8s form (shareProcessNamespace:false / userns / gVisor)
 * is mapped in docs/proc-hardening.md.
 */
export function screenToolPath(
  candidate: string,
  worktreeRoot: string,
  cwd: string,
  secretPaths: readonly string[] = [],
): BashScreenResult {
  const root = path.resolve(worktreeRoot);
  const lexical = path.resolve(cwd || root, candidate);
  const lexicalResult = classifyResolvedPath(candidate, lexical, root, secretPaths);
  if (lexicalResult.denied) return lexicalResult;
  // Resolve symlinks on the existing prefix and re-check, so a symlink that
  // lexically stays in-worktree but points at /proc or outside is caught. Both
  // sides must be realpath'd (N2): comparing a realpath'd candidate against a
  // lexical root over-DENIES every real file when the worktree root itself sits
  // under a symlink ancestor (e.g. macOS /var → /private/var, or a symlinked
  // data volume) — a fail-closed asymmetry, not a security hole, but fragile.
  const real = realpathExisting(lexical);
  if (real === lexical) return ALLOW;
  return classifyResolvedPath(candidate, real, realpathExisting(root), secretPaths);
}

/**
 * Deny a resolved absolute path that reads /proc, hits a configured secret file,
 * escapes the worktree, or touches `.git/`. The secret-file check runs on the
 * RESOLVED path (so a relative or symlinked reference to the secret is caught too)
 * and BEFORE the containment check, so a secret file that happens to sit inside the
 * root is still denied — that is what makes `extraSecretPaths` meaningful for a chat
 * session rooted at /opt/uzi-src (PRD #39 Decision 6). For a run, the worker's join
 * token lives OUTSIDE the worktree, so the outside-root deny below is already the
 * load-bearing block; the secret check is additive defense-in-depth. The /proc deny
 * stays first and unchanged — it is the load-bearing non-Bash egress block.
 */
function classifyResolvedPath(
  candidate: string,
  resolved: string,
  root: string,
  secretPaths: readonly string[] = [],
): BashScreenResult {
  if (candidate.includes("/proc/") || resolved === "/proc" || resolved.startsWith("/proc/")) return deny(REASON_PROC);
  if (hitsSecret(resolved, secretPaths)) return deny(REASON_SECRET_FILE);
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
 *
 * `extraSecretPaths` are worker-credential file paths (the configured
 * UZI_WORKER_TOKEN_FILE) to deny a Read/Grep/Glob of, honored inside
 * classifyResolvedPath (on the resolved path). For a RUN the join token sits
 * outside the worktree, so the containment check already denies it and this is
 * additive; for a CHAT session rooted at the baked source `/opt/uzi-src` (PRD #39
 * Decision 6/S2) it is the same outside-root deny that carries the load, with this
 * as defense-in-depth — a compromised chat must never Read the join token and
 * escalate to the worker protocol whose *run* claims carry the PAT.
 */
export function buildPathGuardHook(
  worktreeRoot: string,
  log: Logger,
  extraSecretPaths: readonly string[] = [],
): (input: HookInput) => Promise<HookJSONOutput> {
  return async (input: HookInput): Promise<HookJSONOutput> => {
    if (input.hook_event_name !== "PreToolUse" || !PATH_TOOLS.has(input.tool_name)) return {};
    const cwd = typeof input.cwd === "string" && input.cwd ? input.cwd : worktreeRoot;
    for (const candidate of extractToolPaths(input.tool_input)) {
      const screen = screenToolPath(candidate, worktreeRoot, cwd, extraSecretPaths);
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
