import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { markRunEnvSeen, realSeenOps, runEnvSeen, type SeenOps } from "../src/run-env-seen.js";
import { resolveScanNeedles } from "../src/run-env-scan.js";
import type { RunEnvRedactResponse } from "../src/protocol.js";

// PRD 4416 M2: what a publish with no live flight does with each answer of the run-env-redact route, and the
// durable marker that proves a run never received variables on this worker.

const RUN = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const reject = (status: number, body: string) => async (): Promise<RunEnvRedactResponse> => {
  throw Object.assign(new Error("http"), { status, body });
};

describe("resolveScanNeedles: the route's refusals", () => {
  const seen = (answer: boolean | "unknown" | Error) => async () => { if (answer instanceof Error) throw answer; return answer; };

  it("a 404 carrying run_env_run_not_found withholds, even when no marker exists", async () => {
    const res = await resolveScanNeedles({ live: undefined, fetchProtected: reject(404, JSON.stringify({ code: "run_env_run_not_found" })), runEnvSeen: seen(false) });
    assert.deepEqual(res, { kind: "withhold", reason: "protection_run_not_found" });
  });

  it("a 409 carrying run_env_redact_closed withholds", async () => {
    const res = await resolveScanNeedles({ live: undefined, fetchProtected: reject(409, JSON.stringify({ code: "run_env_redact_closed" })), runEnvSeen: seen(false) });
    assert.deepEqual(res, { kind: "withhold", reason: "protection_closed" });
  });

  it("any other 409 withholds", async () => {
    const res = await resolveScanNeedles({ live: undefined, fetchProtected: reject(409, JSON.stringify({ code: "vault_locked" })), runEnvSeen: seen(false) });
    assert.deepEqual(res, { kind: "withhold", reason: "protection_http_409" });
  });

  for (const body of ["404 page not found\n", "", "{not json", JSON.stringify({ error: "nope" }), JSON.stringify({ code: 7 })]) {
    it(`a 404 without the code (${JSON.stringify(body).slice(0, 24)}) yields no needles when no marker exists`, async () => {
      const res = await resolveScanNeedles({ live: undefined, fetchProtected: reject(404, body), runEnvSeen: seen(false) });
      assert.deepEqual(res, { kind: "needles", needles: [] });
    });
  }

  it("a 404 carrying some other code withholds: only an absent code means the route is absent", async () => {
    const res = await resolveScanNeedles({ live: undefined, fetchProtected: reject(404, JSON.stringify({ code: "something_else" })), runEnvSeen: seen(false) });
    assert.deepEqual(res, { kind: "withhold", reason: "protection_http_404" });
  });

  it("a 404 without the code withholds when the marker exists", async () => {
    const res = await resolveScanNeedles({ live: undefined, fetchProtected: reject(404, "404 page not found\n"), runEnvSeen: seen(true) });
    assert.equal(res.kind, "withhold");
  });

  it("a 404 without the code withholds when the marker cannot be read, or its check throws", async () => {
    assert.equal((await resolveScanNeedles({ live: undefined, fetchProtected: reject(404, ""), runEnvSeen: seen("unknown") })).kind, "withhold");
    assert.equal((await resolveScanNeedles({ live: undefined, fetchProtected: reject(404, ""), runEnvSeen: seen(new Error("EIO")) })).kind, "withhold");
  });

  it("a 404 without the code withholds when no marker check is supplied", async () => {
    assert.equal((await resolveScanNeedles({ live: undefined, fetchProtected: reject(404, "") })).kind, "withhold");
  });

  it("the marker is not consulted for a live flight or an answered route", async () => {
    const never = async (): Promise<boolean> => { throw new Error("must not be called"); };
    assert.equal((await resolveScanNeedles({ live: [], fetchProtected: reject(404, ""), runEnvSeen: never })).kind, "needles");
    assert.deepEqual(await resolveScanNeedles({ live: undefined, fetchProtected: async () => ({ protected: false }), runEnvSeen: never }), { kind: "needles", needles: [] });
  });
});

describe("the durable marker", () => {
  let dir: string;
  let root: string;
  beforeEach(() => { dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-seen-")); root = path.join(dir, "run-variables-seen"); });
  afterEach(() => fs.rmSync(dir, { recursive: true, force: true }));

  it("is absent until written, then present, and idempotent", async () => {
    assert.equal(await runEnvSeen(root, RUN), false);
    await markRunEnvSeen(root, RUN);
    assert.equal(await runEnvSeen(root, RUN), true);
    await markRunEnvSeen(root, RUN);
    assert.equal(await runEnvSeen(root, RUN), true);
    assert.deepEqual(fs.readdirSync(root), [`${RUN}.seen`], "no temporary file is left");
  });

  it("writes the file, fsyncs it, renames it, then fsyncs the directory, in that order", async () => {
    const trace: string[] = [];
    const ops: SeenOps = {
      mkdirp: async (d) => { trace.push("mkdirp"); await realSeenOps.mkdirp(d); },
      writeSynced: async (f, data) => { trace.push(`write+fsync:${path.basename(f).includes(".tmp-") ? "tmp" : "final"}`); await realSeenOps.writeSynced(f, data); },
      rename: async (a, b) => { trace.push(`rename:${path.basename(b)}`); assert.ok(fs.existsSync(a)); assert.equal(fs.existsSync(b), false); await realSeenOps.rename(a, b); },
      remove: realSeenOps.remove,
      syncDir: async (d) => { trace.push(`syncDir:${path.basename(d)}`); await realSeenOps.syncDir(d); },
    };
    await markRunEnvSeen(root, RUN, ops);
    assert.deepEqual(trace, ["mkdirp", `syncDir:${path.basename(dir)}`, "write+fsync:tmp", `rename:${RUN}.seen`, "syncDir:run-variables-seen"]);
  });

  it("a failed mkdirp, a failed first directory sync, a failed write or a failed rename throws and leaves no marker or temporary file", async () => {
    for (const failing of ["mkdirp", "syncDir", "writeSynced", "rename"] as const) {
      const ops: SeenOps = { ...realSeenOps, [failing]: async () => { throw new Error(`${failing} failed`); } };
      await assert.rejects(markRunEnvSeen(root, RUN, ops), new RegExp(`${failing} failed`));
      assert.equal(await runEnvSeen(root, RUN), false, failing);
      assert.deepEqual(fs.existsSync(root) ? fs.readdirSync(root) : [], [], failing);
    }
  });

  it("a failed directory sync after the rename throws, leaves no temporary file and leaves the marker (fail-closed)", async () => {
    let syncCalls = 0;
    const ops: SeenOps = {
      ...realSeenOps,
      syncDir: async (d) => {
        syncCalls++;
        if (syncCalls === 2) throw new Error("final syncDir failed");
        await realSeenOps.syncDir(d);
      },
    };
    await assert.rejects(markRunEnvSeen(root, RUN, ops), /final syncDir failed/);
    assert.equal(syncCalls, 2, "the failing call is the one after the rename");
    assert.deepEqual(fs.readdirSync(root), [`${RUN}.seen`], "only the marker remains: a present marker only withholds more");
    assert.equal(await runEnvSeen(root, RUN), true);
  });

  it("a write that fails after the temporary file exists removes it", async () => {
    const ops: SeenOps = {
      ...realSeenOps,
      writeSynced: async (f) => { fs.writeFileSync(f, "partial"); throw new Error("fsync failed"); },
    };
    await assert.rejects(markRunEnvSeen(root, RUN, ops), /fsync failed/);
    assert.deepEqual(fs.readdirSync(root), [], "no temporary file is left");
    assert.equal(await runEnvSeen(root, RUN), false);
  });

  it("a read error other than a missing file is unknown, and so is an unsafe run id", async () => {
    fs.mkdirSync(root);
    fs.writeFileSync(path.join(root, "blocker"), "x");
    assert.equal(await runEnvSeen(path.join(root, "blocker"), RUN), "unknown", "the root is a file");
    assert.equal(await runEnvSeen(root, "../escape"), "unknown");
    await assert.rejects(markRunEnvSeen(root, "../escape"));
  });
});
