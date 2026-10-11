import nativeFs from "node:fs/promises";
import { spawn, execFile, type ChildProcess } from "node:child_process";
import { promisify } from "node:util";
import os from "node:os";
import path from "node:path";
import { runnerCommand, runnerPath, runnerTmpdir, uidSplitActive } from "../src/runner-uid.js";
import { gitEnv } from "../src/git.js";

const operations = ["access", "realpath", "mkdtemp", "mkdir", "writeFile", "readFile", "readdir",
  "stat", "lstat", "rm", "cp", "copyFile", "unlink", "symlink", "chmod", "utimes"] as const;
type Operation = typeof operations[number];
const predicates = ["isFile", "isDirectory", "isSymbolicLink", "isFIFO", "isSocket", "isBlockDevice", "isCharacterDevice"];
export function planningEnv(): NodeJS.ProcessEnv {
  return { PATH: runnerPath(), TMPDIR: runnerTmpdir() ?? os.tmpdir() };
}
export function planningGitEnv(): NodeJS.ProcessEnv {
  return { ...gitEnv(), ...planningEnv(), GIT_AUTHOR_NAME: "Fixture", GIT_AUTHOR_EMAIL: "fixture@example.invalid",
    GIT_COMMITTER_NAME: "Fixture", GIT_COMMITTER_EMAIL: "fixture@example.invalid" };
}
const nativeExec = promisify(execFile);
// Preserve execFile's overloads and .child (hash-object writes to stdin).
export const planningExec: typeof nativeExec = ((command: string, args: string[], options: object) => {
  const wrapped = runnerCommand(command, args);
  return nativeExec(wrapped.command, wrapped.args, { ...options, env: planningGitEnv() });
}) as typeof nativeExec;

// Advanced IPC preserves Buffer paths/data, bigint stat fields and Dates.
// Stats/Dirent prototypes are represented by their seven native predicate results.
const childProgram = `
const fs = require("node:fs/promises");
const operations = new Set(${JSON.stringify(operations)});
const predicates = ${JSON.stringify(predicates)};
function encode(value) {
  if (Array.isArray(value)) return value.map(encode);
  if (value && typeof value.isFile === "function") {
    return { fields: {...value, atime: value.atime, mtime: value.mtime, ctime: value.ctime, birthtime: value.birthtime}, predicates: Object.fromEntries(predicates.map(p => [p, value[p]()])) };
  }
  return value;
}
process.on("message", async ({id, operation, args}) => {
  if (operation === "close") { process.disconnect(); return; }
  if (operation === "hold") { process.send({id, started: true}); return; }
  if (operation === "exit") { process.exit(23); }
  if (operation === "disconnect") { process.disconnect(); return; }
  try {
    if (!operations.has(operation)) throw Error("unsupported fixture operation");
    const value = await fs[operation](...args);
    process.send({id, value: encode(value)});
  } catch (error) {
    process.send({id, error: {message: error.message, name: error.name, code: error.code,
      errno: error.errno, syscall: error.syscall, path: error.path, dest: error.dest}});
  }
});
process.on("disconnect", () => process.exit(0));
`;
function decode(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(decode);
  if (value && typeof value === "object" && "predicates" in value && "fields" in value) {
    const record = value as { fields: object; predicates: Record<string, boolean> };
    return Object.assign(record.fields, Object.fromEntries(predicates.map(p => [p, () => record.predicates[p]])));
  }
  return value;
}
type Pending = { resolve: (value: unknown) => void; reject: (error: Error) => void; started?: () => void };

/** Suite-owned test IO: native single UID, one persistent runner child on split.
 * Requests have no retries; a transport failure rejects every pending sibling.
 * The suite's existing test timeout bounds FS calls, including a stuck native call.
 */
export class PlanningFixtureIO {
  readonly fs: Pick<typeof nativeFs, Operation | "constants">;
  private child?: ChildProcess;
  private exited?: Promise<void>;
  private failure?: Error;
  private closing = false;
  private nextId = 0;
  private pending = new Map<number, Pending>();
  constructor(private readonly ipc = uidSplitActive()) {
    this.fs = new Proxy(nativeFs, {
      get: (target, key: keyof typeof nativeFs) => {
        if (key === "constants") return target.constants;
        if (!operations.includes(key as Operation)) throw new Error("unsupported fixture operation");
        return (...args: unknown[]) => this.request(key, args);
      },
    });
  }
  private start(): ChildProcess {
    if (this.child) return this.child;
    const wrapped = runnerCommand(process.execPath, ["-e", childProgram]);
    const child = this.child = spawn(wrapped.command, wrapped.args, {
      env: planningEnv(), stdio: ["ignore", "ignore", "ignore", "ipc"], serialization: "advanced",
    });
    const fail = (error: Error): void => {
      this.failure ??= error;
      for (const request of this.pending.values()) request.reject(this.failure);
      this.pending.clear();
    };
    child.on("message", (message: { id: number; started?: boolean; value?: unknown; error?: object }) => {
      const request = this.pending.get(message.id);
      if (!request) return;
      if (message.started) { request.started?.(); return; }
      this.pending.delete(message.id);
      if (message.error) request.reject(Object.assign(new Error(), message.error));
      else request.resolve(decode(message.value));
    });
    child.on("error", fail);
    child.on("disconnect", () => fail(new Error("planning fixture IPC disconnected")));
    this.exited = new Promise(resolve => child.once("close", () => {
      fail(new Error("planning fixture child exited"));
      resolve();
    }));
    return child;
  }
  private request(operation: string, args: unknown[], started?: () => void): Promise<unknown> {
    if (this.failure || this.closing) return Promise.reject(this.failure ?? new Error("planning fixture IO closed"));
    if (!this.ipc) {
      const fn = nativeFs[operation as Operation] as (...args: unknown[]) => Promise<unknown>;
      return fn(...args);
    }
    const child = this.start();
    return new Promise((resolve, reject) => {
      const id = ++this.nextId;
      this.pending.set(id, { resolve, reject, started });
      child.send({ id, operation, args }, error => {
        if (error) {
          this.pending.delete(id);
          reject(error);
        }
      });
    });
  }
  async temporaryDirectory(prefix: string, setup?: (directory: string) => Promise<void>): Promise<string> {
    const root = await this.fs.realpath(runnerTmpdir() ?? os.tmpdir());
    const directory = await this.fs.mkdtemp(path.join(root, prefix));
    try {
      await this.fs.chmod(directory, 0o700);
      await setup?.(directory);
      return directory;
    } catch (error) {
      await this.fs.rm(directory, { recursive: true, force: true });
      throw error;
    }
  }
  // Deterministic lifecycle probes: acknowledge a held request before disrupting IPC.
  async holdForTest(): Promise<{ pending: Promise<unknown> }> {
    let started!: () => void;
    const ready = new Promise<void>(resolve => { started = resolve; });
    const pending = this.request("hold", [], started);
    await Promise.race([ready, pending]);
    return { pending };
  }
  async disruptForTest(operation: "exit" | "disconnect"): Promise<void> {
    const pending = this.request(operation, []);
    await pending.catch(() => {});
    await this.exited;
  }
  async close(): Promise<void> {
    if (!this.closing) {
      this.closing = true;
      if (this.child?.connected) {
        this.child.send({ operation: "close" }, () => {});
      }
    }
    await this.exited;
  }
}
