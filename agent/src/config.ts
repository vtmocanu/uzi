import os from "node:os";
import fs from "node:fs";
import type { LogLevel } from "./log.js";
import { errMessage } from "./util.js";

// Worker configuration, parsed from env (PRD #4 §Configuration).
//
// Interval envs accept either a Go-style duration string ("15s", "3s", "500ms",
// "2h") — matching how the same knobs are expressed server-side and in
// docker-compose — or a bare integer read as milliseconds.

/** Which executor drives a claimed run. */
export type ExecutorKind = "sdk" | "stub";

export interface Config {
  apiUrl: string;
  workerToken: string;
  dataDir: string;
  workerName: string;
  /**
   * The worker template this image was built from (PRD #18), baked in as
   * ENV UZI_WORKER_TEMPLATE by each agent/templates/<name>/Dockerfile (a hardcoded
   * literal per template, not the WORKER_TEMPLATE build arg). Reported at register
   * so the server can badge drift from the declared choice. Defaults to "base"
   * when the env is unset (the minimal image). Observability only: never a trust
   * boundary — the server re-validates it and the join token is the sole authn.
   */
  workerTemplate: string;
  version: string;
  /** `sdk` = Claude Agent SDK (default, product path); `stub` = M2 no-AI stub. */
  executor: ExecutorKind;
  /**
   * When true (UZI_STUB_PLAN_GATE), the `stub` executor drives the M4 plan gate
   * (emit plan → awaiting_approval → await verdict) before committing, so the
   * full plan-approval workflow can be exercised end-to-end with no live SDK.
   * Off by default: the bare M2 stub goes straight to a local commit.
   */
  stubPlanGate: boolean;
  /**
   * The UZI_WORKER_TOKEN_FILE path, if the join token was delivered by file. A
   * read-only secret mount (the shipping compose default) can't be unlinked, so
   * the token file persists and is same-uid readable; the Bash guardrail denies a
   * `cat` of this path (symmetric with its /proc deny). Undefined for env-var
   * delivery.
   */
  workerTokenFile?: string;
  heartbeatIntervalMs: number;
  pollIntervalMs: number;
  /** How long messages accumulate before a batched POST (PRD: 500ms). */
  messageBatchMs: number;
  /** Per-request HTTP timeout for control-plane calls. */
  httpTimeoutMs: number;
  /**
   * Bound on how long a run may sit at the plan-approval gate before it is failed
   * (M4 resolution of the PRD's open "awaiting_approval wall-clock cap"). Generous
   * by default so a human has ample time, but finite so an abandoned plan never
   * wedges the worker (one run at a time).
   */
  planApprovalTimeoutMs: number;
  /**
   * Chat run (PRD #39) lifecycle knobs. Chat rides the run machinery as a third
   * kind but has its own clocks (Decision 3): a per-conversation turn cap, a
   * per-turn wall-clock backstop, an idle window that completes a parked chat, and
   * a faster poll cadence for input pickup (Decision 2, latency floor). The turn
   * cap default matches the server-side ceiling; a claim's own config may push a
   * lower per-run value, which the ChatRunner prefers over this worker default.
   */
  chatMaxTurns: number;
  chatTurnTimeoutMs: number;
  chatIdleTimeoutMs: number;
  chatPollMs: number;
  /** How many chat sessions the worker runs CONCURRENTLY with the run slot
   *  (Decision 4). Default 1 — one live conversation per user-worker; a second chat
   *  queues until the first ends. The run lane is always a separate concurrent slot. */
  chatSessions: number;
  logLevel: LogLevel;
}

const DURATION_RE = /^(\d+)\s*(ms|s|m|h)?$/;

/** Parse "15s" / "500ms" / "2h" / "15000" into milliseconds. */
export function parseDuration(value: string): number {
  const m = DURATION_RE.exec(value.trim());
  if (!m) throw new Error(`invalid duration: ${JSON.stringify(value)}`);
  const n = Number(m[1]);
  switch (m[2]) {
    case "h": return n * 3_600_000;
    case "m": return n * 60_000;
    case "s": return n * 1_000;
    case "ms":
    case undefined: return n; // bare number = milliseconds
    default: throw new Error(`invalid duration unit in ${JSON.stringify(value)}`);
  }
}

function required(env: NodeJS.ProcessEnv, key: string): string {
  const v = env[key];
  if (v === undefined || v.trim() === "") {
    throw new Error(`missing required env ${key}`);
  }
  return v.trim();
}

function duration(env: NodeJS.ProcessEnv, key: string, fallback: string): number {
  return parseDuration(env[key]?.trim() || fallback);
}

/** Parse a positive-integer env (e.g. CHAT_MAX_TURNS) with a fallback; a blank or
 *  non-positive value falls back rather than throwing, matching duration()'s lenient
 *  shape for operator-tunable knobs. */
function positiveInt(env: NodeJS.ProcessEnv, key: string, fallback: number): number {
  const raw = env[key]?.trim();
  if (!raw) return fallback;
  const n = Number(raw);
  return Number.isInteger(n) && n > 0 ? n : fallback;
}

function isLogLevel(v: string): v is LogLevel {
  return v === "debug" || v === "info" || v === "warn" || v === "error";
}

function parseExecutor(v: string | undefined): ExecutorKind {
  const kind = v?.trim().toLowerCase();
  if (kind === "stub") return "stub";
  if (kind === "sdk" || kind === undefined || kind === "") return "sdk";
  throw new Error(`invalid UZI_EXECUTOR ${JSON.stringify(v)} (expected "sdk" or "stub")`);
}

function parseBool(v: string | undefined): boolean {
  const s = v?.trim().toLowerCase();
  return s === "1" || s === "true" || s === "yes";
}

/**
 * Resolve the worker join token. UZI_WORKER_TOKEN_FILE (a path) is preferred and
 * is the STRUCTURAL /proc hardening (M6): delivering the token via a file rather
 * than an env var keeps it out of the worker's `/proc/<pid>/environ`, where a
 * same-uid agent subprocess (`cat <symlink>/self/environ`) could otherwise read
 * it. After reading, the file is unlinked so it does not rest on disk for the
 * agent's Bash to `cat` either — best-effort, since a read-only secret mount
 * (k8s Secret, `--read-only`) cannot be unlinked; the environ win holds
 * regardless. Falls back to UZI_WORKER_TOKEN (env) for backward compatibility.
 */
function resolveWorkerToken(env: NodeJS.ProcessEnv): string {
  const file = env.UZI_WORKER_TOKEN_FILE?.trim();
  if (file) {
    let raw: string;
    try {
      raw = fs.readFileSync(file, "utf8");
    } catch (err) {
      throw new Error(`cannot read UZI_WORKER_TOKEN_FILE ${file}: ${errMessage(err)}`);
    }
    const token = raw.trim();
    if (token === "") throw new Error(`UZI_WORKER_TOKEN_FILE ${file} is empty`);
    try {
      fs.unlinkSync(file);
    } catch {
      // Read-only mount or already gone: the token is no longer in environ, which
      // is the primary win; the on-disk residue is only a defense-in-depth extra.
    }
    return token;
  }
  return required(env, "UZI_WORKER_TOKEN");
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): Config {
  const apiUrl = required(env, "UZI_API_URL").replace(/\/+$/, "");
  const rawLevel = env.UZI_LOG_LEVEL?.trim().toLowerCase() ?? "info";
  return {
    apiUrl,
    workerToken: resolveWorkerToken(env),
    dataDir: env.UZI_DATA_DIR?.trim() || "/data",
    workerName: env.UZI_WORKER_NAME?.trim() || os.hostname(),
    workerTemplate: env.UZI_WORKER_TEMPLATE?.trim() || "base",
    version: env.UZI_AGENT_VERSION?.trim() || "0.1.0-m4",
    executor: parseExecutor(env.UZI_EXECUTOR),
    stubPlanGate: parseBool(env.UZI_STUB_PLAN_GATE),
    workerTokenFile: env.UZI_WORKER_TOKEN_FILE?.trim() || undefined,
    heartbeatIntervalMs: duration(env, "WORKER_HEARTBEAT_INTERVAL", "15s"),
    pollIntervalMs: duration(env, "WORKER_POLL_INTERVAL", "3s"),
    messageBatchMs: duration(env, "WORKER_MESSAGE_BATCH_INTERVAL", "500ms"),
    httpTimeoutMs: duration(env, "WORKER_HTTP_TIMEOUT", "30s"),
    planApprovalTimeoutMs: duration(env, "WORKER_PLAN_APPROVAL_TIMEOUT", "24h"),
    // Chat lifecycle (PRD #39 Decisions 2/3). Defaults raised from the earlier
    // 15/20m draft because idle-death discards the conversation; poll is faster
    // than the run lane so a turn starts within ~1s of a user message.
    chatMaxTurns: positiveInt(env, "CHAT_MAX_TURNS", 50),
    chatTurnTimeoutMs: duration(env, "WORKER_CHAT_TURN_TIMEOUT", "10m"),
    chatIdleTimeoutMs: duration(env, "WORKER_CHAT_IDLE_TIMEOUT", "60m"),
    chatPollMs: duration(env, "WORKER_CHAT_POLL_MS", "1000"),
    chatSessions: positiveInt(env, "WORKER_CHAT_SESSIONS", 1),
    logLevel: isLogLevel(rawLevel) ? rawLevel : "info",
  };
}
