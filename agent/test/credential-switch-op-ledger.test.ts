import { describe, it, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  CredentialSwitchOpLedger, applyThroughLedger, defaultLedgerFs, type LedgerFs,
} from "../src/credential-switch-op-ledger.js";
import type { CredentialSwitchApplyRequest, CredentialSwitchApplyResult } from "../src/client.js";

const RUN = "11111111-2222-3333-4444-555555555555";
const REQ: CredentialSwitchApplyRequest = { claimGeneration: 3, registerNonce: "n", opId: "op-1" };

const dirs: string[] = [];
function tmp(): string {
  const d = fs.mkdtempSync(path.join(os.tmpdir(), "csw-ledger-"));
  dirs.push(d);
  return d;
}
afterEach(() => { for (const d of dirs.splice(0)) fs.rmSync(d, { recursive: true, force: true }); });

function spyFs(log: string[], overrides: Partial<LedgerFs> = {}): LedgerFs {
  const base = (name: keyof LedgerFs) => (...args: unknown[]) => {
    log.push(`${name}:${path.basename(String(args[0]))}`);
    return (defaultLedgerFs[name] as (...a: unknown[]) => Promise<unknown>)(...args);
  };
  return {
    mkdirp: base("mkdirp"), readFile: base("readFile"), writeFileSynced: base("writeFileSynced"),
    rename: base("rename"), fsyncDir: base("fsyncDir"), rm: base("rm"), ...overrides,
  } as LedgerFs;
}

function fakeClient(log: string[], result: CredentialSwitchApplyResult | (() => never)) {
  return {
    async applyCredentialSwitch(): Promise<CredentialSwitchApplyResult> {
      log.push("SEND");
      if (typeof result === "function") return result();
      return result;
    },
  };
}

const ledgerFile = (dir: string) => path.join(dir, "credential-switch-ops", `${RUN}.json`);
const onDisk = (dir: string) => JSON.parse(fs.readFileSync(ledgerFile(dir), "utf8")).ops;

describe("credential switch op ledger", () => {
  it("writes ahead in temp, fsync file, rename, fsync dir order before the send", async () => {
    const dir = tmp();
    const log: string[] = [];
    const ledger = new CredentialSwitchOpLedger(dir, spyFs(log));
    const out = await applyThroughLedger(fakeClient(log, { kind: "superseded" }), ledger, RUN, REQ);
    assert.equal(out.kind, "superseded");
    const send = log.indexOf("SEND");
    const before = log.slice(0, send);
    const w = before.findIndex(e => e.startsWith("writeFileSynced:"));
    const r = before.findIndex(e => e.startsWith("rename:"));
    const d = before.findLastIndex(e => e.startsWith("fsyncDir:"));
    assert.ok(w >= 0 && r > w && d > r, `order wrong: ${before.join(",")}`);
    assert.match(before[w]!, /\.tmp-/);
    // the parent of the new directory is fsynced too
    assert.ok(before.filter(e => e.startsWith("fsyncDir:")).length >= 3);
    assert.equal(onDisk(dir)["op-1"].state, "settled");
    assert.equal(onDisk(dir)["op-1"].outcome, "superseded");
  });

  it("does not send when the write-ahead fails", async () => {
    const dir = tmp();
    const log: string[] = [];
    const ledger = new CredentialSwitchOpLedger(dir, spyFs(log, {
      async writeFileSynced() { throw new Error("disk full"); },
    }));
    const out = await applyThroughLedger(fakeClient(log, { kind: "superseded" }), ledger, RUN, REQ);
    assert.equal(out.kind, "ledger_unavailable");
    assert.ok(!log.includes("SEND"));
  });

  it("does not send when the dir fsync of the write-ahead fails", async () => {
    const dir = tmp();
    const log: string[] = [];
    const ledger = new CredentialSwitchOpLedger(dir, spyFs(log, {
      async fsyncDir() { throw new Error("eio"); },
    }));
    const out = await applyThroughLedger(fakeClient(log, { kind: "superseded" }), ledger, RUN, REQ);
    assert.equal(out.kind, "ledger_unavailable");
    assert.ok(!log.includes("SEND"));
  });

  it("a clean never-sent op with route_absent is clean_route_absent", async () => {
    const dir = tmp();
    const ledger = new CredentialSwitchOpLedger(dir);
    const out = await applyThroughLedger(fakeClient([], { kind: "route_absent" }), ledger, RUN, REQ);
    assert.equal(out.kind, "route_absent");
    assert.equal(onDisk(dir)["op-1"].state, "clean_route_absent");
  });

  it("uncertain then route_absent stays uncertain (sticky), in process and across a reload", async () => {
    const dir = tmp();
    const first = new CredentialSwitchOpLedger(dir);
    assert.equal((await applyThroughLedger(fakeClient([], { kind: "uncertain", detail: "x" }), first, RUN, REQ)).kind, "uncertain");
    assert.equal(onDisk(dir)["op-1"].state, "ambiguous");
    assert.equal((await applyThroughLedger(fakeClient([], { kind: "route_absent" }), first, RUN, REQ)).kind, "uncertain");
    const reloaded = new CredentialSwitchOpLedger(dir);
    assert.equal((await applyThroughLedger(fakeClient([], { kind: "route_absent" }), reloaded, RUN, REQ)).kind, "uncertain");
    assert.equal(onDisk(dir)["op-1"].state, "ambiguous");
    // a different op is not affected
    const other = await applyThroughLedger(fakeClient([], { kind: "route_absent" }), reloaded, RUN, { ...REQ, opId: "op-2" });
    assert.equal(other.kind, "route_absent");
  });

  it("a crash mid-send (entry left sending) is ambiguous after reload", async () => {
    const dir = tmp();
    const crashed = new CredentialSwitchOpLedger(dir);
    await crashed.markSending(RUN, "op-1", 3);
    assert.equal(onDisk(dir)["op-1"].state, "sending");
    const reloaded = new CredentialSwitchOpLedger(dir);
    assert.equal((await applyThroughLedger(fakeClient([], { kind: "route_absent" }), reloaded, RUN, REQ)).kind, "uncertain");
    assert.equal(onDisk(dir)["op-1"].state, "ambiguous");
  });

  it("a send that throws leaves the op sending and the next send upgrades it to ambiguous", async () => {
    const dir = tmp();
    const ledger = new CredentialSwitchOpLedger(dir);
    await assert.rejects(applyThroughLedger(fakeClient([], () => { throw new Error("boom"); }), ledger, RUN, REQ));
    assert.equal(onDisk(dir)["op-1"].state, "sending");
    assert.equal((await applyThroughLedger(fakeClient([], { kind: "route_absent" }), ledger, RUN, REQ)).kind, "uncertain");
  });

  it("definite outcomes settle with that outcome", async () => {
    const TOKEN = ["tok", "12345678"].join("-"); // assembled at runtime: no token-shaped source literal
    for (const result of [
      { kind: "applied", token: TOKEN, ordinal: 1, messageSeq: 2, replay: false },
      { kind: "refused_committed", reason: "r" }, { kind: "not_applied", reason: "r" }, { kind: "fenced" },
    ] as CredentialSwitchApplyResult[]) {
      const dir = tmp();
      const ledger = new CredentialSwitchOpLedger(dir);
      assert.equal((await applyThroughLedger(fakeClient([], result), ledger, RUN, REQ)).kind, result.kind);
      assert.equal(onDisk(dir)["op-1"].state, "settled");
      assert.equal(onDisk(dir)["op-1"].outcome, result.kind);
      assert.ok(!fs.readFileSync(ledgerFile(dir), "utf8").includes(TOKEN), "token must never be persisted");
    }
  });

  it("cleanup removes the file and fsyncs the dir; prune drops older generations", async () => {
    const dir = tmp();
    const log: string[] = [];
    const ledger = new CredentialSwitchOpLedger(dir, spyFs(log));
    await applyThroughLedger(fakeClient([], { kind: "superseded" }), ledger, RUN, { ...REQ, opId: "old", claimGeneration: 1 });
    await applyThroughLedger(fakeClient([], { kind: "superseded" }), ledger, RUN, { ...REQ, opId: "new", claimGeneration: 5 });
    await ledger.pruneGenerationsBelow(RUN, 5);
    assert.deepEqual(Object.keys(onDisk(dir)), ["new"]);
    log.length = 0;
    await ledger.cleanup(RUN);
    assert.equal(fs.existsSync(ledgerFile(dir)), false);
    assert.ok(log.indexOf(`rm:${RUN}.json`) >= 0 && log.findIndex(e => e.startsWith("fsyncDir:")) > log.indexOf(`rm:${RUN}.json`));
    // cleanup of a run that never had a ledger is fine
    await new CredentialSwitchOpLedger(dir).cleanup("never-seen");
  });

  it("a corrupt ledger file or an unsafe run id is unavailable, never sent", async () => {
    const dir = tmp();
    fs.mkdirSync(path.join(dir, "credential-switch-ops"));
    fs.writeFileSync(ledgerFile(dir), "{not json");
    const log: string[] = [];
    assert.equal((await applyThroughLedger(fakeClient(log, { kind: "superseded" }), new CredentialSwitchOpLedger(dir), RUN, REQ)).kind, "ledger_unavailable");
    assert.equal((await applyThroughLedger(fakeClient(log, { kind: "superseded" }), new CredentialSwitchOpLedger(dir), "../evil", REQ)).kind, "ledger_unavailable");
    assert.ok(!log.includes("SEND"));
  });
});
