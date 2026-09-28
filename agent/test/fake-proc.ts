// issue #1783 — a FAKE procfs-shaped root for hermetic quiescence tests. The reaper reads
// `<root>/<pid>/{status,stat,environ,cwd}` through procfsTableAt, and a fake view's "kill" removes
// `<root>/<pid>`, so no test here ever reads or signals a host process.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { setQuiescenceViewForTests, type QuiescenceView } from "../src/run-quiescence.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_KEY_ENV } from "../src/worker-spawn-mark.js";
import { HERMETIC_VIEW } from "./setup/hermetic-proc.js";

/** One planted fake process. */
export interface FakeProcEntry {
  uid: number;
  comm?: string;
  ppid?: number;
  pgid?: number;
  sid?: number;
  /** `stat` field 22. */
  startTime?: number;
  zombie?: boolean;
  /** The process env, or "unreadable": `environ` is planted as a DIRECTORY, so reading it fails
   *  with EISDIR even as root (unreadable, and not ENOENT/ESRCH, so never "vanished"). */
  env?: Record<string, string> | "unreadable";
  /** The cwd symlink target (default "/"). */
  cwd?: string;
}

/** Plant `pid` under `root`, replacing any earlier entry. */
export function plantFakeProc(root: string, pid: number, p: FakeProcEntry): void {
  const dir = path.join(root, String(pid));
  fs.rmSync(dir, { recursive: true, force: true });
  fs.mkdirSync(dir, { recursive: true });
  const comm = p.comm ?? `fake${pid}`;
  fs.writeFileSync(
    path.join(dir, "status"),
    `Name:\t${comm}\nState:\t${p.zombie ? "Z (zombie)" : "S (sleeping)"}\nUid:\t${p.uid}\t${p.uid}\t${p.uid}\t${p.uid}\n`,
  );
  // `pid (comm) state ppid pgrp session` then fields 7..21 (15 of them), then field 22 (start).
  fs.writeFileSync(
    path.join(dir, "stat"),
    `${pid} (${comm}) ${p.zombie ? "Z" : "S"} ${p.ppid ?? 1} ${p.pgid ?? pid} ${p.sid ?? pid} ${"0 ".repeat(15)}${p.startTime ?? 1000} 0 0\n`,
  );
  if (p.env === "unreadable") fs.mkdirSync(path.join(dir, "environ"));
  else fs.writeFileSync(path.join(dir, "environ"), Object.entries(p.env ?? {}).map(([k, v]) => `${k}=${v}\u0000`).join(""));
  fs.symlinkSync(p.cwd ?? "/", path.join(dir, "cwd"));
}

/** A same-uid non-dumpable process attributed to nothing: every proof must fail closed on it. */
export function plantUnreadableUnattributed(root: string, pid: number, uid: number = process.getuid?.() ?? 0): void {
  plantFakeProc(root, pid, { uid, comm: "ssh-agent", env: "unreadable", cwd: "/" });
}

/** An in-scope process: cwd inside `clonePath`, carrying `marker` (with `cloneKey`) when given. */
export function plantInScope(
  root: string,
  pid: number,
  clonePath: string,
  opts: { marker?: string; cloneKey?: string; uid?: number } = {},
): void {
  const env: Record<string, string> = {};
  if (opts.marker !== undefined) env[RUN_ATTEMPT_ENV] = opts.marker;
  if (opts.cloneKey !== undefined) env[RUN_CLONE_KEY_ENV] = opts.cloneKey;
  plantFakeProc(root, pid, { uid: opts.uid ?? process.getuid?.() ?? 0, env, cwd: clonePath });
}

/** A fresh, empty fake proc root under os.tmpdir(). */
export function makeFakeProcRoot(prefix = "uzi-fake-proc-"): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), prefix));
}

/** Install `view` for the duration of `fn`, then reinstall `restore` (default: the preload's
 *  empty view). */
export async function withQuiescenceView<T>(view: QuiescenceView, fn: () => Promise<T>, restore: QuiescenceView = HERMETIC_VIEW): Promise<T> {
  setQuiescenceViewForTests(view);
  try {
    return await fn();
  } finally {
    setQuiescenceViewForTests(restore);
  }
}

/** The real procfs narrowed to this test process's descendants, plus `extra.pids` and the pids
 *  recorded in `extra.pidFiles` (a `setsid` plant is reparented away from this process), for a test
 *  that deliberately reaps REAL processes. */
export function scopedRealView(extra: { pids?: number[]; pidFiles?: string[] } = {}): QuiescenceView {
  return { descendantsOf: process.pid, extraPids: extra.pids ?? [], extraPidFiles: extra.pidFiles ?? [] };
}
