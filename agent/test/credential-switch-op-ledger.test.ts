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

  it("uncertain then not_applied with a reason that leaves the op pending stays ambiguous, in process and across a reload", async () => {
    for (const reason of ["credential_busy", "vault_locked", "credential_unavailable"]) {
      const dir = tmp();
      const first = new CredentialSwitchOpLedger(dir);
      await applyThroughLedger(fakeClient([], { kind: "uncertain", detail: "x" }), first, RUN, REQ);
      const out = await applyThroughLedger(fakeClient([], { kind: "not_applied", reason }), first, RUN, REQ);
      assert.deepEqual(out, { kind: "not_applied", reason, priorAmbiguous: true }, "the answer is returned, flagged with the prior ambiguity");
      assert.equal(onDisk(dir)["op-1"].state, "ambiguous", reason);
      assert.equal((await new CredentialSwitchOpLedger(dir).stateOf(RUN, "op-1"))?.state, "ambiguous", reason);
    }
  });

  it("a never-ambiguous op's not_applied carries no priorAmbiguous flag", async () => {
    const out = await applyThroughLedger(fakeClient([], { kind: "not_applied", reason: "credential_busy" }), new CredentialSwitchOpLedger(tmp()), RUN, REQ);
    assert.deepEqual(out, { kind: "not_applied", reason: "credential_busy" });
  });

  for (const result of [{ kind: "superseded" }, { kind: "not_applied", reason: "no_pending_switch" }, { kind: "not_applied", reason: "generation_mismatch" }] as CredentialSwitchApplyResult[]) {
    it(`uncertain then ${result.kind === "not_applied" ? `${result.kind}:${result.reason}` : result.kind} settles: the server answers it only after its replay check (review NB1)`, async () => {
      const dir = tmp();
      const ledger = new CredentialSwitchOpLedger(dir);
      await applyThroughLedger(fakeClient([], { kind: "uncertain", detail: "x" }), ledger, RUN, REQ);
      assert.equal((await applyThroughLedger(fakeClient([], result), ledger, RUN, REQ)).kind, result.kind);
      assert.equal(onDisk(dir)["op-1"].state, "settled");
    });
  }

  for (const result of [{ kind: "fenced" }] as CredentialSwitchApplyResult[]) {
    it(`uncertain then ${result.kind} stays ambiguous (the first send may have committed), and the answer is still returned`, async () => {
      const dir = tmp();
      const first = new CredentialSwitchOpLedger(dir);
      await applyThroughLedger(fakeClient([], { kind: "uncertain", detail: "x" }), first, RUN, REQ);
      assert.deepEqual((await applyThroughLedger(fakeClient([], result), first, RUN, REQ)).kind, result.kind);
      assert.equal(onDisk(dir)["op-1"].state, "ambiguous");
      // Across a reload too, and a never-ambiguous op still settles.
      assert.equal((await new CredentialSwitchOpLedger(dir).stateOf(RUN, "op-1"))?.state, "ambiguous");
      await applyThroughLedger(fakeClient([], result), first, RUN, { ...REQ, opId: "op-2" });
      assert.equal(onDisk(dir)["op-2"].state, "settled");
    });
  }

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

  it("record(ambiguous) re-creates a missing entry, so a lost entry never reads as clean", async () => {
    const dir = tmp();
    const ledger = new CredentialSwitchOpLedger(dir);
    await assert.rejects(ledger.record(RUN, "ghost", "clean_route_absent"), /no entry to record/);
    assert.equal(await ledger.record(RUN, "ghost", "ambiguous", "uncertain", 4), "ambiguous");
    assert.deepEqual(onDisk(dir)["ghost"], { generation: 4, state: "ambiguous", outcome: "uncertain" });
    // A send of the op after the entry vanished (a prune or cleanup raced it) is still ambiguous.
    const client = fakeClient([], { kind: "route_absent" });
    await ledger.cleanup(RUN, 4);
    assert.equal((await applyThroughLedger(client, ledger, RUN, { ...REQ, opId: "ghost", claimGeneration: 4 })).kind, "uncertain");
  });

  it("cleanup and prune never drop a sending or ambiguous entry of the current generation", async () => {
    const dir = tmp();
    const ledger = new CredentialSwitchOpLedger(dir);
    await ledger.markSending(RUN, "inflight", 5);
    await ledger.markSending(RUN, "unsure", 5);
    await ledger.record(RUN, "unsure", "ambiguous", "uncertain");
    await ledger.markSending(RUN, "done", 5);
    await ledger.record(RUN, "done", "settled", "applied");
    await ledger.markSending(RUN, "stale", 2);
    await ledger.record(RUN, "stale", "ambiguous", "uncertain");
    await ledger.pruneGenerationsBelow(RUN, 5);
    assert.deepEqual(Object.keys(onDisk(dir)).sort(), ["done", "inflight", "unsure"], "only the older generation went");
    await ledger.cleanup(RUN, 5);
    assert.deepEqual(Object.keys(onDisk(dir)).sort(), ["inflight", "unsure"], "the unresolved entries of this generation survive cleanup");
    // A fresh instance (a restart) still sees them, and reads the in-flight send as ambiguous.
    const reborn = new CredentialSwitchOpLedger(dir);
    assert.equal((await reborn.stateOf(RUN, "inflight"))?.state, "ambiguous");
    // Cleanup for a newer generation finally releases them.
    await ledger.cleanup(RUN, 6);
    assert.equal(fs.existsSync(ledgerFile(dir)), false);
  });

  it("a corrupt or unreadable ledger file is uncertain and an unsafe run id is unavailable, never sent", async () => {
    const dir = tmp();
    fs.mkdirSync(path.join(dir, "credential-switch-ops"));
    fs.writeFileSync(ledgerFile(dir), "{not json");
    const log: string[] = [];
    assert.equal((await applyThroughLedger(fakeClient(log, { kind: "superseded" }), new CredentialSwitchOpLedger(dir), RUN, REQ)).kind, "uncertain");
    const unreadable = new CredentialSwitchOpLedger(dir, spyFs([], { async readFile() { throw new Error("eio"); } }));
    assert.equal((await applyThroughLedger(fakeClient(log, { kind: "superseded" }), unreadable, RUN, REQ)).kind, "uncertain");
    assert.equal((await applyThroughLedger(fakeClient(log, { kind: "superseded" }), new CredentialSwitchOpLedger(dir), "../evil", REQ)).kind, "ledger_unavailable");
    assert.ok(!log.includes("SEND"));
  });

  it("a failed write-ahead for an op already ambiguous or settled is uncertain, not unavailable", async () => {
    for (const first of [{ kind: "uncertain", detail: "x" }, { kind: "superseded" }] as CredentialSwitchApplyResult[]) {
      const dir = tmp();
      await applyThroughLedger(fakeClient([], first), new CredentialSwitchOpLedger(dir), RUN, REQ);
      const log: string[] = [];
      const failing = new CredentialSwitchOpLedger(dir, spyFs(log, { async writeFileSynced() { throw new Error("ENOSPC"); } }));
      const out = await applyThroughLedger(fakeClient(log, { kind: "superseded" }), failing, RUN, REQ);
      assert.equal(out.kind, "uncertain", first.kind);
      assert.ok(!log.includes("SEND"));
    }
  });

  it("a settled op re-sent and answered route_absent is uncertain and stays settled", async () => {
    const dir = tmp();
    const ledger = new CredentialSwitchOpLedger(dir);
    await applyThroughLedger(fakeClient([], { kind: "refused_committed", reason: "r" }), ledger, RUN, REQ);
    const out = await applyThroughLedger(fakeClient([], { kind: "route_absent" }), ledger, RUN, REQ);
    assert.equal(out.kind, "uncertain");
    assert.equal(onDisk(dir)["op-1"].state, "settled");
    assert.equal(onDisk(dir)["op-1"].outcome, "refused_committed");
  });

  it("concurrent sends of one op: a slow route_absent never overrides a fast uncertain", async () => {
    const dir = tmp();
    const ledger = new CredentialSwitchOpLedger(dir);
    const delayed = (ms: number, result: CredentialSwitchApplyResult) => ({
      async applyCredentialSwitch(): Promise<CredentialSwitchApplyResult> {
        await new Promise(r => setTimeout(r, ms));
        return result;
      },
    });
    const [a, b] = await Promise.all([
      applyThroughLedger(delayed(100, { kind: "route_absent" }), ledger, RUN, REQ),
      applyThroughLedger(delayed(5, { kind: "uncertain", detail: "x" }), ledger, RUN, REQ),
    ]);
    assert.equal(a.kind, "uncertain");
    assert.equal(b.kind, "uncertain");
    assert.equal(onDisk(dir)["op-1"].state, "ambiguous");
  });

  it("concurrent first loads share one map, so no op is lost", async () => {
    const dir = tmp();
    let reads = 0;
    const ledger = new CredentialSwitchOpLedger(dir, spyFs([], {
      async readFile(file) {
        const n = reads++;
        const text = await defaultLedgerFs.readFile(file);
        await new Promise(r => setTimeout(r, n === 0 ? 60 : 1));
        return text;
      },
    }));
    await Promise.all([ledger.markSending(RUN, "op-1", 1), ledger.markSending(RUN, "op-2", 1)]);
    assert.deepEqual(Object.keys(onDisk(dir)).sort(), ["op-1", "op-2"]);
  });

  it("concurrent persists land in order: every resolved write-ahead is on disk", async () => {
    const dir = tmp();
    let writes = 0;
    const ledger = new CredentialSwitchOpLedger(dir, spyFs([], {
      async writeFileSynced(file, data) {
        const n = writes++;
        if (n === 1) await new Promise(r => setTimeout(r, 60)); // write 0 is the warm-up; op-1's is the slow one
        await defaultLedgerFs.writeFileSynced(file, data);
      },
    }));
    await ledger.markSending(RUN, "warm", 1); // loads the run and syncs the directory once
    await Promise.all([ledger.markSending(RUN, "op-1", 1), ledger.markSending(RUN, "op-2", 1)]);
    assert.deepEqual(Object.keys(onDisk(dir)).sort(), ["op-1", "op-2", "warm"]);
  });

  it("real fs adapter: write-ahead then reload round-trip in an OS tmpdir", async () => {
    const dir = tmp();
    const first = new CredentialSwitchOpLedger(dir, defaultLedgerFs);
    assert.deepEqual(await first.markSending(RUN, "op-1", 3), { wasAmbiguous: false });
    assert.equal(onDisk(dir)["op-1"].state, "sending");
    assert.equal(fs.statSync(ledgerFile(dir)).mode & 0o777, 0o600);
    assert.deepEqual(fs.readdirSync(path.join(dir, "credential-switch-ops")), [`${RUN}.json`]);
    const reloaded = new CredentialSwitchOpLedger(dir, defaultLedgerFs);
    assert.deepEqual(await reloaded.stateOf(RUN, "op-1"), { generation: 3, state: "ambiguous" });
    assert.deepEqual(await reloaded.markSending(RUN, "op-1", 3), { wasAmbiguous: true });
  });
});
