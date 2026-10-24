// Maintainer-run entry for the offline code cross-check replay harness (PRD 3167 M1).
// Subcommands: verify, replay (real model calls, explicitly authorized), score (offline).
// Stdout is JSON lines of fixed codes, ids, counts and pins only. The absolute records-file locator goes to
// stderr (it can carry a username or checkout location, so never merge stderr into published output); raw
// records go to a 0600 file and never to stdout. `score` resolves a relative records or adjudications path
// against the checkout root (not the cwd, which is agent/ under `npm run`).
import path from "node:path";
import fs from "node:fs/promises";
import { execFile } from "node:child_process";
import { fileURLToPath } from "node:url";
import {
  isSafeReplayModel, loadReplayCorpus, materializeCase, parseAdjudications, parseReplayRecords, readReplayFile, REPLAY_EFFORTS,
  ReplayError,
  runReplays, sanitizedAggregate, scoreReplays,
  type ReplayEffort, type ReplayFamily, type ReplayRecord, type RunReplaysDeps,
} from "./code-cross-check-replay.js";

export interface ReplayCliDeps {
  checkout: string;
  env: NodeJS.ProcessEnv;
  write: (line: string) => void;
  /** Local-only diagnostics (the records-file locator); never published. */
  writeErr: (line: string) => void;
  signals: {
    on(name: "SIGINT" | "SIGTERM" | "SIGHUP", handler: () => void): unknown;
    removeListener(name: "SIGINT" | "SIGTERM" | "SIGHUP", handler: () => void): unknown;
  };
  scratchRoot?: string;
  now?: () => number;
  checkers?: RunReplaysDeps["checkers"];
  materialize?: RunReplaysDeps["materialize"];
  manifestSha256?: string;
  uziHead?: () => Promise<string>;
}

interface ReplayArgs {
  family: ReplayFamily;
  model: string;
  effort: ReplayEffort;
  repeats: number;
  timeoutMs: number | undefined;
  authorized: boolean;
}

const EFFORTS: readonly string[] = REPLAY_EFFORTS;
const RECORDS_MAX_BYTES = 64 * 1024 * 1024;
const ADJUDICATIONS_MAX_BYTES = 4 * 1024 * 1024;

/** Reject unsafe identities before anything loads. Never echo an invalid value. */
function parseReplayArgs(args: string[], env: NodeJS.ProcessEnv): ReplayArgs | null {
  const seen = new Map<string, string>();
  let authorized = false;
  for (let i = 0; i < args.length; i++) {
    const flag = args[i]!;
    if (flag === "--authorize-real-model-calls") {
      if (authorized) return null;
      authorized = true;
      continue;
    }
    if (!["--family", "--model", "--effort", "--repeats", "--timeout-ms"].includes(flag) || seen.has(flag)) return null;
    const value = args[++i];
    if (value === undefined) return null;
    seen.set(flag, value);
  }
  const family = seen.get("--family");
  const model = seen.get("--model");
  const effort = seen.get("--effort");
  const repeatsText = seen.get("--repeats");
  const timeoutText = seen.get("--timeout-ms");
  if ((family !== "claude" && family !== "codex") || !model || !effort || !EFFORTS.includes(effort) ||
      repeatsText === undefined || !/^[1-9]\d{0,2}$/.test(repeatsText)) return null;
  const repeats = Number(repeatsText);
  if (repeats > 20) return null;
  let timeoutMs: number | undefined;
  if (timeoutText !== undefined) {
    if (!/^[1-9]\d{0,8}$/.test(timeoutText)) return null;
    timeoutMs = Number(timeoutText);
    if (timeoutMs > 15 * 60_000) return null;
  }
  if (!isSafeReplayModel(model, env)) return null;
  return { family, model, effort: effort as ReplayEffort, repeats, timeoutMs, authorized };
}

function hermeticGit(args: string[], cwd: string): Promise<string> {
  return new Promise((resolve, reject) => {
    execFile("git", args, {
      cwd, timeout: 30_000, maxBuffer: 64 * 1024,
      env: { PATH: process.env.PATH ?? "", GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_CONFIG_NOSYSTEM: "1", LANG: "C" },
    }, (error, stdout) => { if (error) reject(new Error("git_failed")); else resolve(stdout.trim()); });
  });
}

async function agentVersion(checkout: string): Promise<string> {
  const bytes = await readReplayFile(path.join(checkout, "agent/package.json"), 256 * 1024);
  const version: unknown = bytes ? (JSON.parse(bytes.toString("utf8")) as { version?: unknown }).version : undefined;
  if (typeof version !== "string" || !version) throw new Error("agent_version_unavailable");
  return version;
}

async function runVerify(deps: ReplayCliDeps, signal: AbortSignal, scratch: string): Promise<number> {
  const corpus = await loadReplayCorpus(deps.checkout, { manifestSha256: deps.manifestSha256 });
  const materialize = deps.materialize ?? materializeCase;
  for (const c of corpus.cases) {
    signal.throwIfAborted();
    await (await materialize(deps.checkout, c, scratch, signal)).cleanup();
  }
  deps.write(JSON.stringify({ verify: "ok", cases: corpus.cases.length }));
  return 0;
}

async function runReplayCommand(deps: ReplayCliDeps, a: ReplayArgs, signal: AbortSignal, root: string, scratch: string): Promise<number> {
  const corpus = await loadReplayCorpus(deps.checkout, { manifestSha256: deps.manifestSha256 });
  const now = deps.now ?? Date.now;
  const uzi_head = deps.uziHead ? await deps.uziHead() : await hermeticGit(["rev-parse", "HEAD"], deps.checkout);
  const checkerVersion = { uzi_head, agent_version: await agentVersion(deps.checkout) };
  const stamp = new Date(now()).toISOString().replace(/[:.]/g, "-");
  const file = path.join(root, `code-cross-check-replay-${stamp}.jsonl`);
  // Raw findings are model output about repository code: owner-only, created exclusively.
  const handle = await fs.open(file, "wx", 0o600);
  let records: ReplayRecord[];
  const absFile = path.resolve(file);
  try {
    records = await runReplays({
      checkout: deps.checkout, corpus, families: [a.family], model: a.model, effort: a.effort, repeats: a.repeats,
      timeoutMs: a.timeoutMs, scratchDir: scratch, env: deps.env, checkers: deps.checkers,
      materialize: deps.materialize, signal, now: deps.now, checkerVersion,
      writeRecord: async (rec) => { await handle.appendFile(JSON.stringify(rec) + "\n"); },
    });
  } catch (error) {
    deps.writeErr(JSON.stringify({ records_file: absFile }));
    throw error;
  } finally { await handle.close(); }
  deps.writeErr(JSON.stringify({ records_file: absFile }));
  deps.write(sanitizedAggregate(scoreReplays(corpus, records)));
  return records.every((r) => r.outcome === "completed") ? 0 : 1;
}

async function runScore(deps: ReplayCliDeps, args: string[]): Promise<number> {
  const recordsFile = args[0];
  if (!recordsFile || recordsFile.startsWith("--")) { deps.write(JSON.stringify({ error: "invalid_cli" })); return 1; }
  let adjudicationsFile: string | undefined;
  if (args.length === 3 && args[1] === "--adjudications") adjudicationsFile = args[2];
  else if (args.length !== 1) { deps.write(JSON.stringify({ error: "invalid_cli" })); return 1; }
  const corpus = await loadReplayCorpus(deps.checkout, { manifestSha256: deps.manifestSha256 });
  const readInput = async (file: string, max: number, code: "records_invalid" | "adjudication_invalid"): Promise<Buffer> => {
    let bytes: Buffer | null = null;
    try { bytes = await readReplayFile(path.resolve(deps.checkout, file), max); } catch { /* mapped below */ }
    if (!bytes) throw new ReplayError(code);
    return bytes;
  };
  const bytes = await readInput(recordsFile, RECORDS_MAX_BYTES, "records_invalid");
  let adjudications;
  if (adjudicationsFile !== undefined) {
    adjudications = parseAdjudications((await readInput(adjudicationsFile, ADJUDICATIONS_MAX_BYTES, "adjudication_invalid")).toString("utf8"));
  }
  deps.write(sanitizedAggregate(scoreReplays(corpus, parseReplayRecords(bytes.toString("utf8"), deps.env), adjudications)));
  return 0;
}

/** Signal handlers precede startup and stay installed until cleanup has settled. */
export async function runReplayCli(args: string[], deps: ReplayCliDeps): Promise<number> {
  const [command, ...rest] = args;
  const fail = (error: string): number => { deps.write(JSON.stringify({ error })); return 1; };
  let parsed: ReplayArgs | null = null;
  if (command === "replay") {
    parsed = parseReplayArgs(rest, deps.env);
    if (!parsed) return fail("invalid_cli");
    // Authorization precedes loading the corpus or constructing any checker.
    if (!parsed.authorized) return fail("authorization_required");
  } else if (command === "verify") {
    if (rest.length !== 0) return fail("invalid_cli");
  } else if (command !== "score") return fail("invalid_cli");

  const abort = new AbortController();
  const onSignal = (): void => { abort.abort(); };
  const names = ["SIGINT", "SIGTERM", "SIGHUP"] as const;
  for (const name of names) deps.signals.on(name, onSignal);
  let scratch: string | undefined;
  try {
    if (command === "score") return await runScore(deps, rest);
    const root = deps.scratchRoot ?? path.join(deps.checkout, ".uzi/scratch");
    await fs.mkdir(root, { recursive: true });
    scratch = await fs.mkdtemp(path.join(root, "replay-run-"));
    return command === "verify"
      ? await runVerify(deps, abort.signal, scratch)
      : await runReplayCommand(deps, parsed!, abort.signal, root, scratch);
  } catch (error) {
    return fail(error instanceof ReplayError ? error.code : "startup_failed");
  } finally {
    abort.abort();
    for (const name of names) deps.signals.removeListener(name, onSignal);
    if (scratch) await fs.rm(scratch, { recursive: true, force: true });
  }
}

/** Process wiring for the main guard: stdout carries the publishable lines, stderr the local locator. */
export function defaultReplayCliDeps(checkout: string): ReplayCliDeps {
  return {
    checkout, env: process.env, signals: process,
    write: (line) => { process.stdout.write(line + "\n"); },
    writeErr: (line) => { process.stderr.write(line + "\n"); },
  };
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const checkout = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
  runReplayCli(process.argv.slice(2), defaultReplayCliDeps(checkout)).then((code) => { process.exitCode = code; }).catch(() => {
    process.stdout.write(JSON.stringify({ error: "startup_failed" }) + "\n");
    process.exitCode = 1;
  });
}
