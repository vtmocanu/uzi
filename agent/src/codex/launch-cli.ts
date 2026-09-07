// PRD #1156 (M3a) — the packaged internal executable/entrypoint for the isolated
// Codex launch primitive. This is the SAME implementation the future worker adapter
// calls, not a test-only copy: it reads a TRUSTED spec (a JSON file path from argv),
// launches ONE supervisor root via `launchCodexRoot`, and runs a bounded built-in
// flow — await `started`, optionally proxy the app-server transport (fds 0/1/2) to
// the parent so an external driver speaks app-server JSONL RPC, dispose on EOF/signal,
// and print the final evidence JSON to fd 2.
//
// Run directly: `tsx src/codex/launch-cli.ts <spec.json>` (the bottom-of-file guard).
// The spec is TRUSTED input (a worker-written file), never model/repo-controlled; we
// still validate its shape and never let it select the supervisor target argv (that
// stays launcher-fixed inside `launchCodexRoot`).

import { readFileSync } from "node:fs";
import type { Readable, Writable } from "node:stream";
import { pathToFileURL } from "node:url";

import {
  launchCodexRoot,
  type CodexLaunchSpec,
  type CodexRootHandle,
  type LauncherDeps,
} from "./launcher.js";

export interface LaunchCliDeps {
  /** The launch primitive (defaults to the real {@link launchCodexRoot}). */
  readonly launch?: (spec: CodexLaunchSpec, launcherDeps?: LauncherDeps) => Promise<CodexRootHandle>;
  readonly launcherDeps?: LauncherDeps;
  /** Load + validate the trusted spec (defaults to reading the JSON file at argv[2]). */
  readonly loadSpec?: (argv: readonly string[]) => CodexLaunchSpec;
  readonly stdin?: Readable;
  readonly stdout?: Writable;
  readonly stderr?: Writable;
  /** Proxy the app-server transport (0/1/2) to the parent. Default true. */
  readonly proxyStdio?: boolean;
  /** Resolves with the shutdown trigger reason (EOF/signal/failure). Injectable for tests. */
  readonly until?: Promise<string>;
}

function writeLine(stream: Writable, value: unknown): void {
  stream.write(`${JSON.stringify(value)}\n`);
}
function errMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** Validate the trusted spec's SHAPE (defense-in-depth; the file is worker-written). */
function validateSpec(value: unknown): CodexLaunchSpec {
  if (typeof value !== "object" || value === null) throw new Error("spec must be a JSON object");
  const s = value as Record<string, unknown>;
  const str = (k: string): string => {
    const v = s[k];
    if (typeof v !== "string" || v.length === 0) throw new Error(`spec.${k} must be a non-empty string`);
    return v;
  };
  const provider = s.provider;
  if (typeof provider !== "object" || provider === null) throw new Error("spec.provider must be an object");
  const p = provider as Record<string, unknown>;
  const providerStr = (k: string): string => {
    const v = p[k];
    if (typeof v !== "string" || v.length === 0) throw new Error(`spec.provider.${k} must be a non-empty string`);
    return v;
  };
  const kind = s.kind;
  if (kind !== "provider" && kind !== "command") throw new Error('spec.kind must be "provider" or "command"');
  const childArgv = s.childArgv;
  if (!Array.isArray(childArgv) || !childArgv.every((a) => typeof a === "string")) throw new Error("spec.childArgv must be a string[]");
  const credentialValue = p.credentialValue;
  if (credentialValue !== undefined && typeof credentialValue !== "string") throw new Error("spec.provider.credentialValue must be a string when present");
  return {
    ownedDataRoot: str("ownedDataRoot"),
    model: str("model"),
    codexBin: str("codexBin"),
    supervisorBin: str("supervisorBin"),
    cwd: str("cwd"),
    kind,
    childArgv: childArgv as string[],
    provider: {
      name: providerStr("name"),
      baseUrl: providerStr("baseUrl"),
      envKey: providerStr("envKey"),
      ...(credentialValue === undefined ? {} : { credentialValue }),
    },
  };
}

function loadSpecFromArgv(argv: readonly string[]): CodexLaunchSpec {
  const specPath = argv[2];
  if (specPath === undefined || specPath.length === 0) throw new Error("usage: launch-cli <spec.json>");
  return validateSpec(JSON.parse(readFileSync(specPath, "utf8")));
}

/** The default shutdown race: parent stdin EOF, a termination signal, or a root failure. */
function defaultShutdown(deps: LaunchCliDeps, handle: CodexRootHandle): Promise<string> {
  return new Promise<string>((resolve) => {
    const stdin = deps.stdin ?? process.stdin;
    stdin.once("end", () => resolve("eof"));
    stdin.once("close", () => resolve("eof"));
    process.once("SIGINT", () => resolve("signal:SIGINT"));
    process.once("SIGTERM", () => resolve("signal:SIGTERM"));
    handle.whenFailed.then(() => resolve("failed")).catch(() => { /* failure already surfaced by dispose */ });
  });
}

/**
 * Read the trusted spec, launch one root, run the bounded built-in flow, and return an
 * exit code: 0 on a confirmed clean disposal, non-zero otherwise.
 */
export async function runLaunchCli(argv: readonly string[], deps: LaunchCliDeps = {}): Promise<number> {
  const stderr = deps.stderr ?? process.stderr;
  const launch = deps.launch ?? launchCodexRoot;
  const loadSpec = deps.loadSpec ?? loadSpecFromArgv;

  let spec: CodexLaunchSpec;
  try {
    spec = loadSpec(argv);
  } catch (error) {
    writeLine(stderr, { event: "spec-error", reason: errMessage(error) });
    return 2;
  }

  let handle: CodexRootHandle;
  try {
    handle = await launch(spec, deps.launcherDeps);
  } catch (error) {
    writeLine(stderr, { event: "launch-failed", reason: errMessage(error) });
    return 2;
  }

  writeLine(stderr, { event: "ready", supervisorPid: handle.supervisorPid ?? null, started: handle.started });

  if (deps.proxyStdio ?? true) {
    const stdin = deps.stdin ?? process.stdin;
    if (handle.transport.stdin) stdin.pipe(handle.transport.stdin);
    if (handle.transport.stdout) handle.transport.stdout.pipe(deps.stdout ?? process.stdout);
    if (handle.transport.stderr) handle.transport.stderr.pipe(stderr);
  }

  const trigger = await (deps.until ?? defaultShutdown(deps, handle));
  const outcome = await handle.dispose();
  writeLine(stderr, {
    event: "final",
    trigger,
    clean: outcome.clean,
    reason: outcome.clean ? null : outcome.reason,
    dispose: outcome.event ?? null,
  });
  return outcome.clean ? 0 : 1;
}

// Thin guard: run the CLI when this module is executed directly (tsx launch-cli.ts …).
// Importing the module in a test does NOT trigger this (argv[1] is the test runner).
const invokedDirectly = process.argv[1] !== undefined && import.meta.url === pathToFileURL(process.argv[1]).href;
if (invokedDirectly) {
  void runLaunchCli(process.argv).then((code) => { process.exitCode = code; });
}
