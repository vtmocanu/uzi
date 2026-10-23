import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { launchCodexProvisionRoot, type SupervisorProcess } from "../src/codex/launcher.js";
import { installerBarrier } from "./codex-installer-fixture.js";

/** Native protocol double; exercises the production launcher without an OS process. */
export function provisionNative(options: {
  hold?: boolean; startup?: boolean; unknown?: boolean; overflow?: boolean;
  code?: number; stderr?: string; transientOnce?: boolean; pendingStartup?: boolean;
} = {}) {
  const spawned = installerBarrier(), disposing = installerBarrier(), release = installerBarrier();
  const calls: string[][] = [];
  const children: { streams: PassThrough[]; close: () => void }[] = [];
  if (!options.hold) release.resolve();
  const launcher: typeof launchCodexProvisionRoot = (request, deps) =>
    launchCodexProvisionRoot(request, {
      ...deps, env: { UZI_UID_SPLIT: "1" }, resolveRunnerUid: () => 10002, rootStartTime: () => 123,
      spawnSupervisor: (_command, args) => {
        calls.push([...args]);
        const child = new EventEmitter() as EventEmitter & { pid: number; stdin: PassThrough;
          stdout: PassThrough; stderr: PassThrough; stdio: PassThrough[] };
        child.pid = 286000 + calls.length * 2;
        child.stdin = new PassThrough(); child.stdout = new PassThrough(); child.stderr = new PassThrough();
        const control = new PassThrough(), evidence = new PassThrough();
        child.stdio = [child.stdin, child.stdout, child.stderr, control, evidence];
        const frame = (value: unknown) => evidence.write(JSON.stringify(value) + "\n");
        let closed = false, disposingStarted = false, buffer = "";
        const close = () => {
          if (closed) return;
          closed = true;
          child.emit("exit", 0, null);
          child.stdout.end(); child.stderr.end(); evidence.end();
          child.emit("close", 0, null);
        };
        children.push({ streams: child.stdio, close });
        const finish = (id?: number) => {
          if (disposingStarted) return;
          disposingStarted = true;
          disposing.resolve();
          void release.promise.then(() => {
            if (id !== undefined) frame(options.unknown
              ? { event: "dispose", id, state: "unconfirmed", reason: "deadline", killed: [], reaped: [],
                  children: [{ pid: child.pid + 1, startTime: "123" }] }
              : { event: "dispose", id, state: "drained", authority: "ECHILD+__WALL",
                  killed: [], reaped: [child.pid + 1] });
            close();
          });
        };
        control.on("data", (chunk: Buffer) => {
          buffer += chunk.toString();
          let newline: number;
          while ((newline = buffer.indexOf("\n")) >= 0) {
            const command = JSON.parse(buffer.slice(0, newline)) as { op: string; id: number };
            buffer = buffer.slice(newline + 1);
            if (command.op === "dispose") finish(command.id);
            if (command.op === "snapshot") frame({ event: "snapshot", id: command.id, processes: [] });
          }
        });
        control.on("finish", () => { if (options.startup) finish(); });
        // Queue behind launchSetupRoot's immediate collector attachment.
        queueMicrotask(() => {
          child.stdout.write(options.overflow ? Buffer.alloc(8 * 1024 * 1024 + 1) : "export NIX_SSL_CERT_FILE=/fixture/cert;\n");
          if (options.stderr) child.stderr.write(options.stderr);
          if (options.transientOnce && calls.length === 1) child.stderr.write("connection reset by peer");
          const started = () => {
          frame({ event: "started", supervisorPid: child.pid, childPid: child.pid + 1,
            subreaper: true, nondumpable: !options.startup, uid: 10002,
            liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true });
          frame({ event: "child_exit", code: options.transientOnce && calls.length === 1 ? 1 : options.code ?? 0 });
          };
          if (options.pendingStartup) void release.promise.then(started);
          else started();
          spawned.resolve();
        });
        return child as unknown as SupervisorProcess;
      },
    });
  return { launcher, calls, spawned: spawned.promise, disposing: disposing.promise,
    release: () => release.resolve(),
    destroy: () => { release.resolve(); for (const child of children) {
      child.close(); for (const stream of child.streams) stream.destroy();
    } },
  };
}
