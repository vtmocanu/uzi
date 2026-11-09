// Issue #3644 m3: the parent-side plumbing of the producer-to-consumer regression (runner-independent-restore-producer.test.ts).
// The generations run as REAL separate node processes (restore-producer-child.mts); this module holds the config they are given,
// the child supervisor, and the HTTP bridge that lets a child's forge client reach the fake forge the parent hosts.
import { spawn, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import http from "node:http";
import { fileURLToPath } from "node:url";
import type { ClaimResponse } from "../src/protocol.js";
import type { FetchFn } from "../src/forge.js";

/** What a child needs to build its RunRunner (written to a JSON file; its path is the child's only argument). */
export interface ChildConfig {
  /** P is the producer (parks at the plan gate and is SIGKILLed); S and S2 are the successor and a later restart. */
  role: "P" | "S" | "S2";
  apiUrl: string;
  /** The bearer token FakeApi accepts. */
  apiToken: string;
  /** The worker join token: keys the authority MAC and the boundary record. */
  joinToken: string;
  dataDir: string;
  homeRoot: string;
  procRoot: string;
  hostname: string;
  forgeUrl: string;
  claim: ClaimResponse;
  features: string[];
  /** Set for the Docker-wired row: the endpoint string plus a scripted quiesceRun (no daemon is ever contacted). */
  dockerHost?: string;
  /** The api echoes the owner-hold cause of a recovery_wait park in the state ack (the unsupported-predecessor park needs it). */
  echoRecoveryCause?: boolean;
  logPath: string;
}

export type LogLine = Record<string, unknown> & { msg: string };

export const CHILD_ENTRY = fileURLToPath(new URL("./restore-producer-child.mts", import.meta.url));
const AGENT_ROOT = fileURLToPath(new URL("..", import.meta.url));

export function readLog(file: string): LogLine[] {
  if (!fs.existsSync(file)) return [];
  return fs.readFileSync(file, "utf8").split("\n").filter(Boolean).map(l => JSON.parse(l) as LogLine);
}

export interface Child {
  proc: ChildProcess;
  /** Resolves with `[exit code, signal]`. */
  exited: Promise<[number | null, NodeJS.Signals | null]>;
  stderr(): string;
  log(): LogLine[];
}

/** Starts a child node process (`--import tsx`, as the other test child processes do) on the given config. */
export function startChild(cfg: ChildConfig, cfgFile: string, tmpDir: string): Child {
  fs.writeFileSync(cfgFile, JSON.stringify(cfg));
  // TMPDIR is the per-test dir so a SIGKILLed child's own temp dirs (hermetic procfs, gitleaks shim) go with it. stdin is a pipe the
  // parent holds open: the child exits when the parent dies (see restore-producer-child.mts).
  const proc = spawn(process.execPath, ["--import", "tsx", CHILD_ENTRY, cfgFile], {
    cwd: AGENT_ROOT, stdio: ["pipe", "ignore", "pipe"], env: { ...process.env, TMPDIR: tmpDir },
  });
  let err = "";
  proc.stderr!.on("data", (d: Buffer) => { err += d.toString(); });
  const exited = new Promise<[number | null, NodeJS.Signals | null]>((resolve, reject) => {
    proc.once("exit", (code, signal) => resolve([code, signal]));
    proc.once("error", reject);
  });
  return { proc, exited, stderr: () => err.slice(-4000), log: () => readLog(cfg.logPath) };
}

/** Awaits the child's exit, failing with its stderr tail (and killing it) when it has not exited within `timeoutMs`. */
export async function exitedWithin(child: Child, what: string, timeoutMs = 90_000): Promise<[number | null, NodeJS.Signals | null]> {
  let timer: NodeJS.Timeout | undefined;
  const timeout = new Promise<never>((_, reject) => {
    timer = setTimeout(() => {
      child.proc.kill("SIGKILL");
      reject(new Error(`${what} did not exit within ${timeoutMs}ms: ${child.stderr()}`));
    }, timeoutMs);
  });
  try { return await Promise.race([child.exited, timeout]); } finally { clearTimeout(timer); }
}

/** Polls `cond` until it holds, failing early when the watched child exits first. */
export async function waitFor(what: string, cond: () => boolean, child: Child | undefined, timeoutMs = 90_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!cond()) {
    if (child && child.proc.exitCode !== null) throw new Error(`child exited (${child.proc.exitCode}) while waiting for ${what}: ${child.stderr()}`);
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}: ${child?.stderr() ?? ""}`);
    await new Promise(r => setTimeout(r, 25));
  }
}

/** An HTTP server answering a child's forwarded forge requests with the parent's fake forge transport (`POST /forward`), and recording
 *  the head the child's branch reached (`POST /head`). */
export async function startForgeBridge(fetchFn: FetchFn, onHead: (sha: string) => void): Promise<{ url: string; close(): Promise<void> }> {
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      // The child reports the head it pushed, as the forge would see the branch move.
      if (req.url === "/head") { onHead(Buffer.concat(chunks).toString("utf8")); res.writeHead(204); res.end(); return; }
      void (async () => {
        const call = JSON.parse(Buffer.concat(chunks).toString("utf8")) as { url: string; method: string; headers: Record<string, string>; body?: string };
        const answer = await fetchFn(call.url, { method: call.method, headers: call.headers, ...(call.body === undefined ? {} : { body: call.body }) });
        res.writeHead(200, { "content-type": "application/json" });
        res.end(JSON.stringify({ status: answer.status, body: await answer.text() }));
      })().catch(error => { res.writeHead(500); res.end(String(error)); });
    });
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const { port } = server.address() as { port: number };
  return {
    url: `http://127.0.0.1:${port}`,
    close: () => new Promise<void>(resolve => { server.closeAllConnections(); server.close(() => resolve()); }),
  };
}
