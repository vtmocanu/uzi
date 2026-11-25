// Shared by the pinned-runtime provider P proofs: a direct spawn of the real pinned app-server that
// stands in for the OS supervisor (uid split, kernel drainage). Only the owned real child's exit
// completes disposal. Adapted from M4 makeDirectCodexSpawn.
import { spawn as nodeSpawn, type ChildProcess } from "node:child_process";
import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import type { SpawnSupervisor, SupervisorProcess } from "../../agent/src/codex/launcher.js";

export function makeDirectCodexSpawn(codexBin: string, expectUid: number, envSink: { env?: NodeJS.ProcessEnv }): SpawnSupervisor {
  return (_command, _args, options): SupervisorProcess => {
    const env = { ...options.env };
    envSink.env = env;
    const child: ChildProcess = nodeSpawn(codexBin, ["app-server"], {
      cwd: options.cwd,
      env,
      stdio: ["pipe", "pipe", "pipe"],
    });
    const control = new PassThrough();
    const evidence = new PassThrough();
    const emitter = new EventEmitter();
    let disposing = false;
    let exited = false;
    const ownedPid = child.pid;
    let killDelivered = false;
    let disposalFinished = false;
    let realClose: [number | null, NodeJS.Signals | null] | undefined;
    const forwardClose = (): void => {
      if (realClose && disposalFinished) {
        emitter.emit("close", ...realClose);
        realClose = undefined;
      }
    };
    child.once("close", (code, signal) => {
      realClose = [code, signal];
      forwardClose();
    });

    const writeEvidence = (frame: Record<string, unknown>): void => {
      evidence.write(`${JSON.stringify(frame)}\n`);
    };

    queueMicrotask(() => {
      writeEvidence({
        event: "started",
        supervisorPid: child.pid,
        childPid: ownedPid,
        subreaper: true,
        nondumpable: true,
        uid: expectUid,
        liveCapsZero: true,
        capBoundingSet: "0xc0",
        noNewPrivs: true,
      });
    });

    child.once("exit", (code, signal) => {
      exited = true;
      if (!disposing) emitter.emit("exit", code, signal);
    });
    child.once("error", (err) => emitter.emit("error", err));

    let controlBuf = "";
    control.on("data", (chunk: Buffer) => {
      controlBuf += chunk.toString("utf8");
      let nl = controlBuf.indexOf("\n");
      while (nl !== -1) {
        const line = controlBuf.slice(0, nl);
        controlBuf = controlBuf.slice(nl + 1);
        let frame: Record<string, unknown>;
        try {
          frame = JSON.parse(line) as Record<string, unknown>;
        } catch {
          nl = controlBuf.indexOf("\n");
          continue;
        }
        if (frame.op === "snapshot") {
          writeEvidence({ event: "snapshot", id: frame.id, processes: [] });
        } else if (frame.op === "dispose") {
          disposing = true;
          const finish = (): void => {
            // Only the exact owned child's observed exit can complete this report.
            writeEvidence({
              event: "dispose", id: frame.id, state: "drained", authority: "ECHILD+__WALL",
              killed: killDelivered ? [ownedPid] : [],
              reaped: exited && ownedPid !== undefined ? [ownedPid] : [],
            });
            evidence.end();
            disposalFinished = true;
            // Synthetic supervisor success is separate from the real child's exit/signal.
            emitter.emit("exit", 0, null);
            // PassThrough data parsing is synchronous: the final response is consumed first.
            forwardClose();
          };
          if (exited) finish();
          else {
            child.once("exit", finish);
            try {
              killDelivered = child.kill("SIGKILL");
              if (!killDelivered) emitter.emit("error", new Error("owned child SIGKILL was not delivered"));
            } catch (error) {
              emitter.emit("error", error);
            }
          }
        }
        nl = controlBuf.indexOf("\n");
      }
    });

    const proc: SupervisorProcess = {
      pid: child.pid,
      stdin: child.stdin,
      stdout: child.stdout,
      stderr: child.stderr,
      stdio: [child.stdin, child.stdout, child.stderr, control, evidence],
      once(event: string, listener: (...args: never[]) => void): unknown {
        emitter.once(event, listener as (...a: unknown[]) => void);
        return proc;
      },
      on(event: string, listener: (...args: never[]) => void): unknown {
        emitter.on(event, listener as (...a: unknown[]) => void);
        return proc;
      },
    };
    return proc;
  };
}
