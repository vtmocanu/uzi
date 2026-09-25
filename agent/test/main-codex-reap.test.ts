import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { reapCodexOrphansAtStartup, startupReapVerdict } from "../src/main.js";
import type { ReapOrphansResult } from "../src/codex/launcher.js";

// Issue #1598 M5 — the worker-startup Codex command orphan reap that main() awaits
// before worker.run(). Importing main.ts does not start the worker (entrypoint guard).

type LogFn = (m: string, f?: Record<string, unknown>) => void;
function recorder(): { log: { info: LogFn; warn: LogFn; error: LogFn }; lines: Array<{ level: string; msg: string; fields?: Record<string, unknown> }> } {
  const lines: Array<{ level: string; msg: string; fields?: Record<string, unknown> }> = [];
  return {
    lines,
    log: {
      info: (msg, fields) => lines.push({ level: "info", msg, fields }),
      warn: (msg, fields) => lines.push({ level: "warn", msg, fields }),
      error: (msg, fields) => lines.push({ level: "error", msg, fields }),
    },
  };
}

const CLEAN: ReapOrphansResult = {
  ok: true, scanned: 3, live: 0, removed: 3, retained: 0, foreign: 0, proof: "held", truncated: false, direntsExamined: 5,
};

describe("reapCodexOrphansAtStartup", () => {
  it("does nothing without the uid split", async () => {
    const r = recorder();
    let calls = 0;
    const result = await reapCodexOrphansAtStartup(r.log, { splitActive: false, reap: async () => { calls += 1; return CLEAN; } });
    assert.equal(result, undefined);
    assert.equal(calls, 0);
    assert.deepEqual(r.lines, []);
  });

  it("runs one pass under the split and logs a clean result at info", async () => {
    const r = recorder();
    let calls = 0;
    const result = await reapCodexOrphansAtStartup(r.log, { splitActive: true, reap: async () => { calls += 1; return CLEAN; } });
    assert.deepEqual(result, CLEAN);
    assert.equal(calls, 1);
    assert.deepEqual(r.lines, [{
      level: "info",
      msg: "codex command orphan reap",
      fields: { scanned: 3, live: 0, removed: 3, retained: 0, foreign: 0, proof: "held", truncated: false, dirents_examined: 5 },
    }]);
  });

  it("warns on retained candidates or an unproven pass", async () => {
    for (const result of [
      { ...CLEAN, removed: 2, retained: 1 },
      { ...CLEAN, removed: 0, retained: 3, proof: "unknown" as const },
      { ...CLEAN, removed: 2, live: 1 },
    ]) {
      const r = recorder();
      await reapCodexOrphansAtStartup(r.log, { splitActive: true, reap: async () => result });
      assert.equal(r.lines[0]?.level, "warn");
    }
  });

  it("warns on a truncated pass even when it removed everything it scanned (issue #1621)", async () => {
    const truncated: ReapOrphansResult = { ...CLEAN, scanned: 64, removed: 64, truncated: true, direntsExamined: 4096 };
    const r = recorder();
    const result = await reapCodexOrphansAtStartup(r.log, { splitActive: true, reap: async () => truncated });
    assert.deepEqual(result, truncated);
    assert.deepEqual(r.lines, [{
      level: "warn",
      msg: "codex command orphan reap was truncated; orphans may remain until a later startup",
      fields: { scanned: 64, live: 0, removed: 64, retained: 0, foreign: 0, proof: "held", truncated: true, dirents_examined: 4096 },
    }]);
    assert.equal(startupReapVerdict(result, false), "run", "truncation never blocks startup");
  });

  it("a failed pass (binary absent, reap_error) warns and never throws", async () => {
    const r = recorder();
    const result = await reapCodexOrphansAtStartup(r.log, {
      splitActive: true,
      reap: async () => ({ ok: false, reason: "spawn", detail: "spawn ENOENT" }),
    });
    assert.deepEqual(result, { ok: false, reason: "spawn", detail: "spawn ENOENT" });
    assert.deepEqual(r.lines, [{ level: "warn", msg: "codex command orphan reap failed", fields: { reason: "spawn", detail: "spawn ENOENT" } }]);

    const thrown = recorder();
    const fromThrow = await reapCodexOrphansAtStartup(thrown.log, { splitActive: true, reap: async () => { throw new Error("boom"); } });
    assert.equal(fromThrow?.ok, false);
    assert.equal(thrown.lines[0]?.level, "warn");
  });

  it("an unkilled reaper is an error line; a shutdown abort is info", async () => {
    const r = recorder();
    await reapCodexOrphansAtStartup(r.log, {
      splitActive: true,
      reap: async () => ({ ok: false, reason: "timeout_unkilled", detail: "timeout; kill delivered; the reaper did not exit" }),
    });
    assert.equal(r.lines.length, 1);
    assert.equal(r.lines[0]?.level, "error");
    assert.equal(r.lines[0]?.fields?.reason, "timeout_unkilled");

    const a = recorder();
    await reapCodexOrphansAtStartup(a.log, { splitActive: true, reap: async () => ({ ok: false, reason: "aborted" }) });
    assert.deepEqual(a.lines, [{ level: "info", msg: "codex command orphan reap aborted by shutdown", fields: undefined }]);
  });

  it("passes the shutdown signal to the reap", async () => {
    const controller = new AbortController();
    let seen: AbortSignal | undefined;
    await reapCodexOrphansAtStartup(recorder().log, {
      splitActive: true,
      signal: controller.signal,
      reap: async (signal) => { seen = signal; return CLEAN; },
    });
    assert.equal(seen, controller.signal);
  });
});

describe("startupReapVerdict", () => {
  it("exits on an unkilled reaper (even when shutting down), skips the worker on shutdown, else runs", () => {
    const unkilled: ReapOrphansResult = { ok: false, reason: "timeout_unkilled" };
    assert.equal(startupReapVerdict(unkilled, false), "exit");
    assert.equal(startupReapVerdict(unkilled, true), "exit");
    assert.equal(startupReapVerdict({ ok: false, reason: "aborted" }, true), "shutdown");
    assert.equal(startupReapVerdict(CLEAN, true), "shutdown");
    assert.equal(startupReapVerdict(undefined, true), "shutdown");
    assert.equal(startupReapVerdict(CLEAN, false), "run");
    assert.equal(startupReapVerdict(undefined, false), "run");
    for (const reason of ["timeout", "spawn", "protocol", "cache_root"]) {
      assert.equal(startupReapVerdict({ ok: false, reason }, false), "run", `${reason}: a finished failed pass only leaves disk behind`);
    }
  });
});
