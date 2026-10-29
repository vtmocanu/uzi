import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { TestContext } from "node:test";
import { fileURLToPath } from "node:url";
import type { AuthorityLockOptions, FlockResult } from "../src/authority-lock.js";

// Shared support for the completion-authority tests. The real kernel flock is always used.
// A host whose flock is not util-linux (BusyBox) gets an argument-translating adapter, as
// test/pod-epoch-lock.test.ts does; the production trust check against that host is
// asserted separately by the tests.

const host = spawnSync("/usr/bin/flock", ["--version"], { encoding: "utf8" });
export const utilLinux = host.status === 0 && host.stdout.startsWith("flock from util-linux ");

function adapter(args: string[], fd: number | undefined): Promise<FlockResult> {
  if (args[0] === "--version") return Promise.resolve({ code: 0, output: "flock from util-linux 2.41\n" });
  return new Promise((resolve, reject) => {
    const child = spawn("/usr/bin/flock", ["-x", "-n", "3"], { stdio: ["ignore", "ignore", "ignore", fd!] });
    child.once("error", reject);
    child.once("close", (code) => resolve({ code: code === 1 ? 200 : (code ?? 2), output: "" }));
  });
}

/** Options for a test host: tmp is rarely ext4/XFS, so the statfs check is injected. */
export function testLockOptions(extra: AuthorityLockOptions = {}): AuthorityLockOptions {
  return { supportedFs: async () => true, ...(utilLinux ? {} : { run: adapter }), ...extra };
}

export function makeTmp(t: TestContext, prefix = "authority-"): string {
  const dir = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), prefix)));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

export const agentRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const supportUrl = new URL(import.meta.url).href;
const lockUrl = new URL("../src/authority-lock.ts", import.meta.url).href;

export interface Holder {
  child: ChildProcess;
  /** Resolves with the first stdout line (READY or REFUSED:...). */
  first: Promise<string>;
  exited: Promise<number | null>;
}

/** Spawn a real second node process that tries to take W on `dataDir`, then idles. */
export function spawnWorkerLockHolder(dataDir: string): Holder {
  const code = `(async () => {
    const lock = await import(process.argv[1]);
    const support = await import(process.argv[2]);
    try {
      await lock.acquireWorkerLifetime(process.argv[3], support.testLockOptions());
      process.stdout.write("READY\\n");
      process.stdin.resume();
    } catch (error) {
      process.stdout.write("REFUSED:" + error.message + "\\n");
      process.exit(3);
    }
  })();`;
  const child = spawn(process.execPath, ["--import", "tsx", "-e", code, lockUrl, supportUrl, dataDir], {
    cwd: agentRoot, stdio: ["pipe", "pipe", "inherit"],
  });
  let buffer = "";
  const first = new Promise<string>((resolve, reject) => {
    child.stdout!.on("data", (chunk: Buffer) => {
      buffer += chunk.toString("utf8");
      const newline = buffer.indexOf("\n");
      if (newline >= 0) resolve(buffer.slice(0, newline));
    });
    child.once("error", reject);
    child.once("close", () => reject(new Error("holder exited before reporting")));
  });
  first.catch(() => undefined);
  const exited = new Promise<number | null>((resolve) => child.once("close", (c) => resolve(c)));
  return { child, first, exited };
}
