import os from "node:os";
import type { LogLevel } from "./log.js";

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
  version: string;
  /** `sdk` = Claude Agent SDK (default, product path); `stub` = M2 no-AI stub. */
  executor: ExecutorKind;
  heartbeatIntervalMs: number;
  pollIntervalMs: number;
  /** How long messages accumulate before a batched POST (PRD: 500ms). */
  messageBatchMs: number;
  /** Per-request HTTP timeout for control-plane calls. */
  httpTimeoutMs: number;
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

function isLogLevel(v: string): v is LogLevel {
  return v === "debug" || v === "info" || v === "warn" || v === "error";
}

function parseExecutor(v: string | undefined): ExecutorKind {
  const kind = v?.trim().toLowerCase();
  if (kind === "stub") return "stub";
  if (kind === "sdk" || kind === undefined || kind === "") return "sdk";
  throw new Error(`invalid UZI_EXECUTOR ${JSON.stringify(v)} (expected "sdk" or "stub")`);
}

export function loadConfig(env: NodeJS.ProcessEnv = process.env): Config {
  const apiUrl = required(env, "UZI_API_URL").replace(/\/+$/, "");
  const rawLevel = env.UZI_LOG_LEVEL?.trim().toLowerCase() ?? "info";
  return {
    apiUrl,
    workerToken: required(env, "UZI_WORKER_TOKEN"),
    dataDir: env.UZI_DATA_DIR?.trim() || "/data",
    workerName: env.UZI_WORKER_NAME?.trim() || os.hostname(),
    version: env.UZI_AGENT_VERSION?.trim() || "0.1.0-m3",
    executor: parseExecutor(env.UZI_EXECUTOR),
    heartbeatIntervalMs: duration(env, "WORKER_HEARTBEAT_INTERVAL", "15s"),
    pollIntervalMs: duration(env, "WORKER_POLL_INTERVAL", "3s"),
    messageBatchMs: duration(env, "WORKER_MESSAGE_BATCH_INTERVAL", "500ms"),
    httpTimeoutMs: duration(env, "WORKER_HTTP_TIMEOUT", "30s"),
    logLevel: isLogLevel(rawLevel) ? rawLevel : "info",
  };
}
