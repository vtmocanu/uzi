import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { MessageBatcher } from "../src/batcher.js";
import { RequestError, type WorkerClient } from "../src/client.js";
import { Outbox } from "../src/outbox.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { sleep } from "../src/util.js";

// Issue #4201 m2b: the batcher's credential-switch hold and the credential epoch stamp.

const RUN = "run-switch-hold";

const tmpRoots: string[] = [];
afterEach(async () => {
  for (const r of tmpRoots.splice(0)) await fs.rm(r, { recursive: true, force: true }).catch(() => undefined);
});

async function mkOutbox(): Promise<Outbox> {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), "batcher-switch-hold-"));
  tmpRoots.push(dir);
  const o = new Outbox({
    root: path.join(dir, "outbox"),
    log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024,
    maxBytes: 512 * 1024 * 1024,
    retentionMs: 7 * 86_400_000,
  });
  await o.init();
  return o;
}

function api(failing = false): { client: WorkerClient; landed: OutgoingMessage[]; setFailing: (f: boolean) => void } {
  const landed: OutgoingMessage[] = [];
  let fail = failing;
  const client = {
    async postMessages(_runId: string, msgs: OutgoingMessage[]): Promise<void> {
      if (fail) throw new RequestError("POST", "/messages", 503, '{"error":"unavailable"}');
      landed.push(...msgs);
    },
  } as unknown as WorkerClient;
  return { client, landed, setFailing: (f) => { fail = f; } };
}

const text = (t: string) => ({ kind: "text" as const, agent: "lead", payload: { text: t } });
const seqs = (msgs: OutgoingMessage[]): number[] => msgs.map((m) => m.seq);
const payloadText = (m: OutgoingMessage | undefined): unknown => (m?.payload as Record<string, unknown> | undefined)?.text;

async function until(pred: () => boolean, ms: number, label: string): Promise<void> {
  const deadline = Date.now() + ms;
  while (Date.now() < deadline) {
    if (pred()) return;
    await sleep(10);
  }
  assert.fail(`timed out waiting for: ${label}`);
}

describe("MessageBatcher credential switch hold", () => {
  it("a hold stops numbering and buffers new emits unnumbered", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 40, 0, nullLogger());
    const hold = b.holdForCredentialSwitch();
    b.emit(text("held-1"));
    b.emit(text("held-2"));
    assert.equal(b.currentSeq(), 40, "no seq was assigned while held");
    await sleep(30);
    assert.deepEqual(a.landed, [], "held frames are not posted");
    hold.cancel();
    await b.close();
  });

  it("release({messageSeq: 41}) renumbers three held frames 42..44 and a later emit gets 45", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 40, 0, nullLogger());
    const hold = b.holdForCredentialSwitch();
    assert.deepEqual(await hold.prepare(), { prepared: true });
    b.emit(text("a"));
    b.emit(text("b"));
    b.emit(text("c"));
    assert.equal(hold.release({ messageSeq: 41 }), true);
    b.emit(text("after"));
    await b.close();
    assert.deepEqual(seqs(a.landed), [42, 43, 44, 45]);
    assert.deepEqual(a.landed.map(payloadText), ["a", "b", "c", "after"]);
  });

  it("refuses a messageSeq below the current tail and stays open", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 10, 0, nullLogger());
    const hold = b.holdForCredentialSwitch();
    await hold.prepare();
    b.emit(text("x"));
    assert.equal(hold.release({ messageSeq: 9 }), false);
    assert.equal(hold.release({ messageSeq: 10.5 }), false);
    assert.equal(hold.release({ messageSeq: 0x7fffffff }), false, "the held frame would overflow int32");
    assert.equal(hold.release({ messageSeq: 10 }), true, "a refused release left the hold open");
    await b.close();
    assert.deepEqual(seqs(a.landed), [11]);
  });

  it("release requires a successful prepare", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 0, 0, nullLogger());
    const hold = b.holdForCredentialSwitch();
    assert.equal(hold.release({ messageSeq: 0 }), false);
    hold.cancel();
    await b.close();
  });

  it("cancel numbers the held frames from the current tail and resumes", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 5, 0, nullLogger());
    b.emit(text("before"));
    const hold = b.holdForCredentialSwitch();
    b.emit(text("h1"));
    b.emit(text("h2"));
    hold.cancel();
    hold.cancel(); // idempotent
    b.emit(text("after"));
    await b.close();
    assert.deepEqual(seqs(a.landed), [6, 7, 8, 9]);
    assert.deepEqual(a.landed.map(payloadText), ["before", "h1", "h2", "after"]);
  });

  it("close while held delivers the held frames at the current tail", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 0, 0, nullLogger());
    b.holdForCredentialSwitch();
    b.emit(text("h1"));
    await b.close();
    assert.deepEqual(seqs(a.landed), [1]);
  });

  it("prepare flushes the already-numbered buffer first", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 0, 60_000, nullLogger());
    b.emit(text("n1"));
    b.emit(text("n2"));
    const hold = b.holdForCredentialSwitch();
    assert.deepEqual(await hold.prepare(), { prepared: true });
    assert.deepEqual(seqs(a.landed), [1, 2]);
    hold.cancel();
    await b.close();
  });

  it("prepare refuses a spilled batcher", async () => {
    const outbox = await mkOutbox();
    const a = api(true);
    const b = new MessageBatcher(a.client, RUN, 0, 5, nullLogger(), undefined, undefined,
      { outbox, generation: 1, transientTripMs: 30 });
    b.emit(text("s1"));
    await until(() => b.isSpilled(), 4000, "batcher enters spill");
    const hold = b.holdForCredentialSwitch();
    const res = await hold.prepare(500);
    assert.equal(res.prepared, false);
    assert.equal(res.reason, "spilled");
    hold.cancel();
    await b.close();
  });

  it("prepare refuses while the outbox still holds undrained messages", async () => {
    const outbox = await mkOutbox();
    const a = api(true);
    const b = new MessageBatcher(a.client, RUN, 0, 5, nullLogger(), undefined, undefined,
      { outbox, generation: 1, transientTripMs: 30 });
    b.emit(text("s1"));
    await until(() => b.isSpilled(), 4000, "batcher enters spill");
    await until(() => outbox.hasUndrainedMessages(RUN), 4000, "frame journaled");
    // Leave spill without draining the journal: the transport is empty but the outbox is not.
    a.setFailing(false);
    const spilled = b as unknown as { spilled: boolean };
    spilled.spilled = false;
    const hold = b.holdForCredentialSwitch();
    const res = await hold.prepare(500);
    assert.equal(res.prepared, false);
    assert.equal(res.reason, "outbox_undrained");
    hold.cancel();
    await b.close();
  });

  it("a hold is refused beside a candidate reservation, and a reservation beside a hold", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 0, 0, nullLogger(), undefined, undefined, { generation: 1 });
    const reservation = b.reserveCandidateTransport();
    assert.throws(() => b.holdForCredentialSwitch(), /cannot be taken/);
    reservation.cancel();
    await b.close();

    const b2 = new MessageBatcher(a.client, RUN, 0, 0, nullLogger(), undefined, undefined, { generation: 1 });
    const hold = b2.holdForCredentialSwitch();
    assert.throws(() => b2.reserveCandidateTransport(), /cannot be reserved/);
    assert.throws(() => b2.holdForCredentialSwitch(), /cannot be taken/);
    hold.cancel();
    await b2.close();
  });

  it("a hold is refused on a closed batcher", async () => {
    const b = new MessageBatcher(api().client, RUN, 0, 0, nullLogger());
    await b.close();
    assert.throws(() => b.holdForCredentialSwitch(), /cannot be taken/);
  });

  it("an emit past the byte cap while held is refused", async () => {
    const b = new MessageBatcher(api().client, RUN, 0, 0, nullLogger(), undefined, undefined, { spillBufferBytes: 200 });
    const hold = b.holdForCredentialSwitch();
    assert.throws(() => b.emit({ kind: "text", payload: { text: "x".repeat(400) } }), /hold refused an event/);
    hold.cancel();
    await b.close();
  });
});

describe("MessageBatcher credential epoch stamp", () => {
  const withOrdinal = (t: string, ordinal: number) => ({ ...text(t), credentialEpochOrdinal: ordinal });

  it("stamps credential_epoch_ordinal only when it is above 0 and the feature is negotiated", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 0, 0, nullLogger(), undefined, undefined,
      { credentialEpochsNegotiated: () => true });
    b.emit(withOrdinal("base", 0));
    b.emit(text("unset"));
    b.emit(withOrdinal("epoch2", 2));
    await b.close();
    assert.deepEqual(a.landed.map((m) => m.credential_epoch_ordinal), [undefined, undefined, 2]);
    assert.equal(Object.hasOwn(a.landed[0]!, "credential_epoch_ordinal"), false, "absent, not zero");
  });

  it("never stamps when the feature is not negotiated", async () => {
    const a = api();
    const plain = new MessageBatcher(a.client, RUN, 0, 0, nullLogger());
    plain.emit(withOrdinal("x", 3));
    await plain.close();
    const off = new MessageBatcher(a.client, RUN, 10, 0, nullLogger(), undefined, undefined,
      { credentialEpochsNegotiated: () => false });
    off.emit(withOrdinal("y", 3));
    await off.close();
    assert.equal(a.landed.length, 2);
    for (const m of a.landed) assert.equal(Object.hasOwn(m, "credential_epoch_ordinal"), false);
  });

  it("a held frame keeps its ordinal through renumbering", async () => {
    const a = api();
    const b = new MessageBatcher(a.client, RUN, 0, 0, nullLogger(), undefined, undefined,
      { credentialEpochsNegotiated: () => true });
    const hold = b.holdForCredentialSwitch();
    await hold.prepare();
    b.emit(withOrdinal("h", 1));
    hold.release({ messageSeq: 7 });
    await b.close();
    assert.deepEqual(a.landed.map((m) => [m.seq, m.credential_epoch_ordinal]), [[8, 1]]);
  });

  it("an outbox-spilled frame keeps its ordinal on replay", async () => {
    const outbox = await mkOutbox();
    const a = api(true);
    const b = new MessageBatcher(a.client, RUN, 0, 5, nullLogger(), undefined, undefined,
      { outbox, generation: 2, transientTripMs: 30, credentialEpochsNegotiated: () => true });
    b.emit(withOrdinal("s1", 2));
    b.emit(text("s2"));
    await until(() => b.isSpilled(), 4000, "batcher enters spill");
    await b.close();
    const replayed: OutgoingMessage[] = [];
    const res = await outbox.drainRun(RUN, async (batch) => { replayed.push(...batch); });
    assert.equal(res.retired, true);
    assert.deepEqual(replayed.map((m) => [m.seq, m.credential_epoch_ordinal]), [[1, 2], [2, undefined]]);
  });
});
