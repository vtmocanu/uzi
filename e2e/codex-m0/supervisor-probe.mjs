// M0 lifecycle characterization fixture; no production disposal contract is implemented here.
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { rm } from "node:fs/promises";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";
import { Probe, deadline } from "./harness.mjs";

export class SupervisorProbe extends Probe {
  spawnAppServer(bin) {
    const child = spawn("/usr/bin/python3", [fileURLToPath(new URL("./supervisor.py", import.meta.url)), bin,
      "--dangerously-bypass-hook-trust", "app-server"], {
      cwd: this.workspace, env: this.env, detached: true,
      stdio: ["pipe", "pipe", "pipe", "pipe", "pipe"],
    });
    this.supervisorMessages = [];
    this.callbackReplies = new Set();
    this.supervisorLines = createInterface({ input: child.stdio[4] });
    this.supervisorLines.on("line", (line) => {
      try {
        assert.ok(Buffer.byteLength(line) <= 64 * 1024, "M0 supervisor report too large");
        assert.ok(this.supervisorMessages.length < 100, "M0 supervisor report count exceeded");
        this.supervisorMessages.push(JSON.parse(line));
      } catch (error) {
        this.errors.push(error.message);
      }
      this.events.emit("change");
    });
    child.stdio[3].on("error", (error) => {
      if (!this.closing) this.errors.push(error.message);
    });
    return child;
  }

  answerDynamic(request) {
    const answer = super.answerDynamic(request);
    this.callbackReplies.add(answer);
    // finally creates its own rejecting promise. Handle that derivative while
    // preserving the original answer for callers and in-flight disposal.
    void answer.finally(() => this.callbackReplies.delete(answer)).catch(() => {});
    return answer;
  }

  async supervisorCommand(op, extra = {}) {
    const id = this.nextId++;
    assert.ok(!this.exited && !this.child.stdio[3].destroyed, "supervisor unavailable; disposal unconfirmed");
    this.child.stdio[3].write(`${JSON.stringify({ id, op, ...extra })}\n`);
    return this.until(() => this.supervisorMessages.find((value) => value.id === id), `supervisor ${op}`, 5000);
  }

  async dispose(noSignal = false) {
    // Controller admission is revoked before awaiting any outstanding callbacks.
    // This fixture owns exactly one held callback and its deterministic release.
    this.admitted = false;
    this.releaseCallback?.();
    if (this.callbackSettled) await deadline(this.callbackSettled, "callback settlement", 1000);
    await deadline(Promise.all([...this.callbackReplies]), "all admitted callback replies settled", 1000);
    assert.equal(this.callbackReplies.size, 0);
    const result = await this.supervisorCommand("dispose", { noSignal, timeoutMs: noSignal ? 40 : 2000 });
    if (result.state === "drained") {
      const exit = await deadline(this.exit, "supervisor successful exit", 2000);
      assert.deepEqual(exit, { code: 0, signal: null }, "evidence plus normal supervisor exit required");
      this.drained = true;
    }
    return result;
  }

  async close() {
    if (this.closing) return;
    this.closing = true;
    let cleanupError;
    try {
      if (this.child && !this.drained) {
        assert.ok(!this.exited, "supervisor exited without confirmed disposal; unconfirmed");
        const result = await this.dispose();
        assert.equal(result.state, "drained", JSON.stringify(result));
      }
    } catch (error) { cleanupError = error; }
    try {
      if (this.versionChild?.pid && this.versionChild.exitCode === null && this.versionChild.signalCode === null) {
        this.versionChild.kill("SIGKILL");
        await deadline(this.versionExit, "version child exit", 2000);
      }
    } catch (error) { cleanupError ??= error; }
    this.releaseResponse?.();
    this.lines?.close();
    this.supervisorLines?.close();
    // EOF tells a still-live helper to attempt its bounded abnormal cleanup.
    // That does not promote an unconfirmed result to successful disposal.
    this.child?.stdio[3]?.end();
    if (this.server) {
      this.server.closeAllConnections();
      try {
        await deadline(new Promise((resolve) => this.server.close(resolve)), "fake server close", 2000);
      } catch (error) { cleanupError ??= error; }
    }
    if (cleanupError) throw new Error(`M0 cleanup unconfirmed; retained ${this.root}`, { cause: cleanupError });
    await rm(this.root, { recursive: true, force: true });
  }
}
