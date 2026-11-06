// issue #3974: while a credential switch is withheld (the holdCredentialSwitch hold, or the
// revision-turn defer window) the api serves only the switch signal and withholds every input row,
// so an owner Cancel never arrives through GET inputs. The poll loop makes ONE bounded ownership
// probe (cancellation_requested) and commits the cancel on a positive answer, never entering the
// switch afterwards.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import type http from "node:http";
import { FakeApi } from "./fake-api.js";
import { nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { CredentialSwitchSignal, SteeringChannel } from "../src/steering.js";
import type { Logger } from "../src/log.js";

const TOKEN = "worker-join-token-0123456789";
const RUN = "6f1c2b3a-1111-4222-8333-444455556666";
const GEN = 4;
let api: FakeApi;
let client: WorkerClient;
let cancel: AbortController;
let onCancelCalls: number;
let warns: string[];
let channels: SteeringChannel[];
let pending: http.ServerResponse[];

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  const baseUrl = await api.listen();
  client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
  cancel = new AbortController();
  onCancelCalls = 0;
  warns = [];
  channels = [];
  pending = [];
  api.signalForeignCredentialSwitch(RUN, GEN);
});

afterEach(async () => {
  for (const res of pending) res.destroy();
  for (const ch of channels) await ch.stop().catch(() => undefined);
  await api.close();
});

function logger(): Logger {
  const base = nullLogger();
  return { ...base, warn: (msg: string) => { warns.push(msg); }, child: () => logger() } as Logger;
}

function channel(probeMs = 2_000, generation = GEN): SteeringChannel {
  const ch = new SteeringChannel(client, RUN, 1, logger(), cancel, {
    claimGeneration: generation,
    withheldSwitchCancelProbeMs: probeMs,
    sleep: () => new Promise<void>((r) => setTimeout(r, 3)),
    onCancel: () => { onCancelCalls++; },
  });
  channels.push(ch);
  return ch;
}

const serve = (body: Record<string, unknown>): void => {
  api.ownershipWire.set(RUN, (res) => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(body));
  });
};
const good = (over: Record<string, unknown> = {}): Record<string, unknown> =>
  ({ status: "running", claim_generation: GEN, claim_released: false, cancellation_requested: true, ...over });

async function until(cond: () => boolean, what: string, ms = 4_000): Promise<void> {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) assert.fail(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 5));
  }
}
const gets = (): number => api.inputGets.get(RUN) ?? 0;
const probes = (): number => api.ownershipRequests.length;
/** Let the poll loop make at least `n` more input reads. */
async function pollsMore(n: number): Promise<void> {
  const target = gets() + n;
  await until(() => gets() >= target, `${n} more input polls`);
}
const probeWarns = (): string[] => warns.filter((w) => w.includes("withheld-switch cancel probe did not complete"));
const pollFailures = (): string[] => warns.filter((w) => w.includes("input poll failed"));

describe("withheld-switch cancel probe: when it runs", () => {
  it("probes while a hold has latched the switch", async () => {
    serve(good({ cancellation_requested: false }));
    const ch = channel();
    ch.holdCredentialSwitch();
    ch.start();
    await until(() => probes() >= 1, "a probe");
    assert.equal(ch.credentialSwitchHoldOpen(), true);
    assert.equal(ch.isCancelled(), false);
  });

  it("probes inside a defer window even though shutdownStop was aborted by the held trip", async () => {
    serve(good());
    const ch = channel();
    ch.beginCredentialSwitchDefer();
    ch.start();
    await until(() => ch.isCancelled(), "the cancel commit");
    assert.equal(ch.cancelCommittedUnderWithheldSwitch(), true);
    assert.ok(probes() >= 1);
  });

  it("does not probe with no switch withheld (no hold, no defer): the switch trips instead", async () => {
    serve(good());
    const ch = channel();
    ch.start();
    await until(() => ch.pendingCredentialSwitch() === GEN, "the switch to trip");
    await pollsMore(2);
    assert.equal(probes(), 0);
  });

  it("does not probe without a switch signal even with a hold", async () => {
    api.signalForeignCredentialSwitch(RUN, undefined);
    serve(good());
    const ch = channel();
    ch.holdCredentialSwitch();
    ch.start();
    await pollsMore(3);
    assert.equal(probes(), 0);
    assert.equal(ch.isCancelled(), false);
  });

  it("does not probe for a stale-generation signal", async () => {
    api.signalForeignCredentialSwitch(RUN, GEN - 1);
    serve(good());
    const ch = channel();
    ch.holdCredentialSwitch();
    ch.start();
    await pollsMore(3);
    assert.equal(probes(), 0);
  });

  it("does not probe once the switch is already pending", async () => {
    serve(good());
    const ch = channel();
    ch.tripCredentialSwitch(GEN);
    assert.equal(ch.pendingCredentialSwitch(), GEN);
    ch.holdCredentialSwitch(); // a hold changes nothing with a switch pending
    ch.start();
    await pollsMore(3);
    assert.equal(probes(), 0);
    assert.equal(ch.cancelCommittedUnderWithheldSwitch(), false);
  });
});

describe("withheld-switch cancel probe: committing", () => {
  it("commits the cancel exactly like a cancel input and never a switch", async () => {
    serve(good());
    const ch = channel();
    const seenPending: Array<number | undefined> = [];
    const hold = ch.holdCredentialSwitch();
    ch.start();
    await until(() => ch.isCancelled(), "the cancel commit");
    seenPending.push(ch.pendingCredentialSwitch());
    assert.equal(onCancelCalls, 1);
    assert.equal(cancel.signal.aborted, true);
    assert.ok(!(cancel.signal.reason instanceof CredentialSwitchSignal), "not a switch abort reason");
    assert.equal(ch.lifecycleSignal().aborted, true);
    assert.ok(!(ch.lifecycleSignal().reason instanceof CredentialSwitchSignal));
    assert.equal(ch.cancelCommittedUnderWithheldSwitch(), true);
    assert.equal(ch.pendingCredentialSwitch(), undefined);

    // Nothing afterwards trips the switch.
    hold.close({ service: true });
    ch.tripCredentialSwitch(GEN);
    ch.tripCredentialSwitch(GEN);
    seenPending.push(ch.pendingCredentialSwitch());
    assert.deepEqual(seenPending, [undefined, undefined]);
    assert.equal(onCancelCalls, 1, "committed once");
    assert.equal(probeWarns().length, 0);
    assert.equal(pollFailures().length, 0);
  });

  it("first poll after cancellation_requested turns true probes, with no switch trip in between", async () => {
    serve(good({ cancellation_requested: false }));
    const ch = channel();
    const pendingAtCancel: Array<number | undefined> = [];
    ch.holdCredentialSwitch();
    ch.start();
    await until(() => probes() >= 2, "two negative probes");
    assert.equal(ch.pendingCredentialSwitch(), undefined);
    assert.equal(ch.isCancelled(), false);
    serve(good());
    pendingAtCancel.push(ch.pendingCredentialSwitch());
    await until(() => ch.isCancelled(), "the cancel commit after the flip");
    pendingAtCancel.push(ch.pendingCredentialSwitch());
    assert.deepEqual(pendingAtCancel, [undefined, undefined]);
    assert.equal(cancel.signal.aborted, true);
  });
});

describe("withheld-switch cancel probe: unknown commits nothing", () => {
  const cases: Array<[string, () => void]> = [
    ["field absent", () => { const b = good(); delete b.cancellation_requested; serve(b); }],
    ["field false", () => serve(good({ cancellation_requested: false }))],
    ["a non-accepted status", () => serve(good({ status: "paused" }))],
    ["generation mismatch", () => serve(good({ claim_generation: GEN + 1 }))],
    ["claim released", () => serve(good({ claim_released: true }))],
  ];
  for (const [name, arm] of cases) {
    it(`${name}: no commit, polling continues, no failure counted`, async () => {
      arm();
      const ch = channel();
      ch.holdCredentialSwitch();
      ch.start();
      await until(() => probes() >= 1, "a probe");
      await pollsMore(3);
      assert.equal(ch.isCancelled(), false);
      assert.equal(ch.cancelCommittedUnderWithheldSwitch(), false);
      assert.equal(cancel.signal.aborted, false);
      assert.equal(onCancelCalls, 0);
      assert.equal(pollFailures().length, 0);
      assert.equal(probeWarns().length, 0, "a definite negative is not a probe failure");
    });
  }

  it("a throwing probe: one warn per probe, no commit, polling continues, no delivery failure", async () => {
    api.ownershipWire.set(RUN, (res) => { res.destroy(); });
    const ch = channel();
    ch.holdCredentialSwitch();
    ch.start();
    await until(() => probeWarns().length >= 1, "the probe warn");
    await pollsMore(3);
    assert.equal(ch.isCancelled(), false);
    assert.equal(ch.cancelCommittedUnderWithheldSwitch(), false);
    assert.equal(pollFailures().length, 0);
  });

  it("a probe that outlives its bound: unknown, no commit, polling continues", async () => {
    api.ownershipWire.set(RUN, (res) => { pending.push(res); });
    const ch = channel(40);
    ch.holdCredentialSwitch();
    ch.start();
    await until(() => probeWarns().length >= 1, "the probe timeout warn");
    await pollsMore(2);
    assert.equal(ch.isCancelled(), false);
    assert.equal(pollFailures().length, 0);
  });
});

describe("withheld-switch cancel probe: stop", () => {
  it("stop() during an in-flight probe returns promptly", async () => {
    api.ownershipWire.set(RUN, (res) => { pending.push(res); });
    const ch = channel(60_000);
    ch.holdCredentialSwitch();
    ch.start();
    await until(() => probes() >= 1, "the in-flight probe");
    const started = Date.now();
    await ch.stop();
    assert.ok(Date.now() - started < 2_000, `stop took ${Date.now() - started}ms`);
    assert.equal(ch.isCancelled(), false);
  });
});
