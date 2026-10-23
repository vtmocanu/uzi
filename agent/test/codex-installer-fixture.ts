import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import {
  launchCodexInstallerRoot,
  type LauncherDeps,
  type SupervisorProcess,
} from "../src/codex/launcher.js";

export function installerBarrier<T = void>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>(done => { resolve = done; });
  return { promise, resolve };
}

/** Protocol double only: no OS process, package manager, or native reap evidence. */
export function nativeRootForTest(mode: "clean" | "unclean" | "startup" = "clean", observe?: (phase: "spawn" | "primary") => void) {
  const child = new EventEmitter() as EventEmitter & {
    pid: number; stdin: PassThrough; stdout: PassThrough; stderr: PassThrough; stdio: PassThrough[];
  };
  child.pid = 285200;
  child.stdin = new PassThrough();
  child.stdout = new PassThrough();
  child.stderr = new PassThrough();
  const control = new PassThrough();
  const evidence = new PassThrough();
  child.stdio = [child.stdin, child.stdout, child.stderr, control, evidence];
  const disposing = installerBarrier();
  const release = installerBarrier();
  const primary = installerBarrier();
  const spawned = installerBarrier();
  const events: string[] = [];
  const calls: { command: string; args: readonly string[]; options: { cwd: string; env: NodeJS.ProcessEnv } }[] = [];
  let buffer = "";
  let closed = false;
  let disposal: Promise<void> | undefined;
  const frame = (value: unknown) => evidence.write(JSON.stringify(value) + "\n");
  const close = (code: number) => {
    if (closed) return;
    closed = true;
    child.emit("exit", code, null);
    child.stdout.end(); child.stderr.end(); evidence.end();
    child.emit("close", code, null);
  };
  control.on("data", (chunk: Buffer) => {
    buffer += chunk.toString();
    let newline: number;
    while ((newline = buffer.indexOf("\n")) >= 0) {
      const request = JSON.parse(buffer.slice(0, newline)) as { op: string; id: number };
      buffer = buffer.slice(newline + 1);
      if (request.op === "snapshot") {
        frame({ event: "snapshot", id: request.id, processes: closed ? [] : [
          { pid: child.pid + 1, ppid: child.pid, pgid: child.pid, comm: "fixture" },
        ] });
      } else if (request.op === "dispose" && !disposal) {
        events.push("dispose-request");
        disposing.resolve();
        disposal = release.promise.then(() => {
          events.push(mode === "clean" ? "drained" : "unconfirmed");
          frame(mode === "clean"
            ? { event: "dispose", id: request.id, state: "drained", authority: "ECHILD+__WALL", killed: [], reaped: [child.pid + 1] }
            : { event: "dispose", id: request.id, state: "unconfirmed", reason: "deadline", killed: [], reaped: [],
                children: [{ pid: child.pid + 1, startTime: "123" }] });
          close(mode === "clean" ? 0 : 1);
        });
      }
    }
  });
  control.on("finish", () => {
    if (mode !== "startup" || disposal) return;
    events.push("startup-control-closed");
    disposing.resolve();
    disposal = release.promise.then(() => { events.push("unconfirmed"); close(1); });
  });
  const launcher: typeof launchCodexInstallerRoot = async (request, deps) => {
    const fakeDeps: LauncherDeps = {
      ...deps, env: { UZI_UID_SPLIT: "1" }, resolveRunnerUid: () => 10002,
      rootStartTime: () => 123,
      spawnSupervisor: (command, args, options) => {
        observe?.("spawn");
        calls.push({ command, args, options });
        events.push("spawn");
        spawned.resolve();
        // Buffered evidence lets the real launcher install its readers.
        frame({ event: "started", supervisorPid: child.pid, childPid: child.pid + 1,
          subreaper: true, nondumpable: mode !== "startup", uid: 10002,
          liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true });
        return child as unknown as SupervisorProcess;
      },
    };
    const handle = await launchCodexInstallerRoot(request, fakeDeps);
    return { ...handle, waitChild: async ms => {
      assert.equal(child.stdin.writableEnded, true, "installer stdin closed before primary wait");
      assert.equal(child.stdout.readableFlowing, true);
      assert.equal(child.stderr.readableFlowing, true);
      observe?.("primary");
      events.push("primary-wait");
      frame({ event: "child_exit", code: 0 });
      primary.resolve();
      return handle.waitChild(ms);
    } };
  };
  return {
    launcher, calls, events, disposing: disposing.promise, spawned: spawned.promise, primary: primary.promise,
    release: () => release.resolve(),
    destroy: async () => {
      release.resolve();
      await disposal;
      close(1);
      for (const stream of child.stdio) stream.destroy();
    },
    writeIgnoredOutput: () => {
      child.stdout.write("installer-output-must-not-escape");
      child.stderr.write("installer-error-must-not-escape");
    },
  };
}
