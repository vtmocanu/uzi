import { EventEmitter } from "node:events";
import { PassThrough } from "node:stream";
import { createInterface } from "node:readline";
import assert from "node:assert/strict";
import { COMMAND_UID, WORKER_UID } from "../src/runner-uid.js";

/** Withholds started until the real launcher's started timer requests disposal.
 * The caller gates cleanup explicitly; no fixture timer fabricates a rejection. */
export class RejectedStartupTransport extends EventEmitter {
  readonly pid = 12345;
  readonly stdin = new PassThrough();
  readonly stdout = new PassThrough();
  readonly stderr = new PassThrough();
  readonly control = new PassThrough();
  readonly evidence = new PassThrough();
  readonly stdio = [this.stdin, this.stdout, this.stderr, this.control, this.evidence];
  readonly events: string[] = [];
  readonly disposeRequested: Promise<void>;
  private observedDispose!: () => void;
  private disposeId?: number;
  private readonly lines;
  // Launcher timers are unref'd. Keep this controlled transport alive until teardown.
  private readonly keepAlive = setInterval(() => {}, 1000);
  private closeImmediate?: ReturnType<typeof setImmediate>;

  constructor(readonly identity: "worker_pat" | "command") {
    super();
    this.disposeRequested = new Promise((resolve) => { this.observedDispose = resolve; });
    this.lines = createInterface({ input: this.control });
    this.lines.on("line", (line) => {
      const frame = JSON.parse(line);
      assert.equal(frame.op, "dispose");
      assert.equal(this.disposeId, undefined, "one owned cleanup request");
      this.disposeId = frame.id;
      this.events.push("dispose");
      this.observedDispose();
    });
  }

  releaseVerifiedCleanup(): void {
    assert.notEqual(this.disposeId, undefined, "cleanup requires an actual launcher dispose request");
    this.frame({ event: "started", supervisorPid: this.pid, childPid: this.pid + 1,
      subreaper: true, nondumpable: true,
      uid: this.identity === "worker_pat" ? WORKER_UID : COMMAND_UID,
      liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true });
    this.events.push("late_started");
    this.frame({ event: "dispose", id: this.disposeId, state: "drained",
      authority: "ECHILD+__WALL", killed: [], reaped: [this.pid + 1] });
    this.events.push("drained");
    this.emit("exit", 0, null);
    this.events.push("exit");
    this.stdout.end();
    this.stderr.end();
    this.evidence.end();
    this.closeImmediate = setImmediate(() => {
      this.events.push("close");
      this.emit("close", 0, null);
    });
  }

  private frame(value: unknown): void { this.evidence.write(JSON.stringify(value) + "\n"); }

  releaseUnconfirmedCleanup(): void {
    assert.notEqual(this.disposeId, undefined, "requires the real dispose request");
    this.frame({ event: "started", supervisorPid: this.pid, childPid: this.pid + 1,
      subreaper: true, nondumpable: true,
      uid: this.identity === "worker_pat" ? WORKER_UID : COMMAND_UID,
      liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true });
    this.frame({ event: "dispose", id: this.disposeId, state: "unconfirmed",
      reason: "deadline", killed: [], reaped: [], children: [this.pid + 1] });
    this.endChannels(1);
  }

  unsafeStarted(privateEvidence: string): void {
    this.frame({ event: "started", supervisorPid: this.pid, childPid: this.pid + 1,
      subreaper: true, nondumpable: false,
      uid: this.identity === "worker_pat" ? WORKER_UID : COMMAND_UID,
      liveCapsZero: true, capBoundingSet: privateEvidence, noNewPrivs: true });
  }

  malformedEvidence(privateEvidence: string): void {
    this.frame({ event: privateEvidence });
  }

  exitBeforeStarted(privateDiagnostic: string): void {
    this.stderr.write(privateDiagnostic);
    this.endChannels(23);
  }

  private endChannels(code: number): void {
    this.emit("exit", code, null);
    this.stdout.end();
    this.stderr.end();
    this.evidence.end();
    this.closeImmediate = setImmediate(() => this.emit("close", code, null));
  }

  assertClosed(): void {
    assert.deepEqual(this.events, ["dispose", "late_started", "drained", "exit", "close"]);
    assert.equal(this.stdout.readableEnded, true);
    assert.equal(this.stderr.readableEnded, true);
    assert.equal(this.evidence.readableEnded, true);
  }

  destroy(): void {
    clearInterval(this.keepAlive);
    if (this.closeImmediate) clearImmediate(this.closeImmediate);
    this.lines.close();
    for (const stream of this.stdio) stream.destroy();
  }
}
