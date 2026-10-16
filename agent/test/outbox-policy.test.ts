import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHmac } from "node:crypto";
import { Outbox, type OutboxOptions } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";

const scratch = path.resolve("../.uzi/scratch");
const dirs: string[] = [];
afterEach(async () => { for (const dir of dirs.splice(0)) await fs.rm(dir, { recursive: true, force: true }); });
async function setup(over: Partial<OutboxOptions> = {}) {
  const dir = await fs.mkdtemp(path.join(scratch, "policy-test-")); dirs.push(dir);
  const root = path.join(dir, "outbox");
  const options = { root, log: nullLogger(), runMaxBytes: 1024, maxBytes: 2048, retentionMs: 1, ...over };
  const box = new Outbox(options); await box.init();
  return { root, box, restart: async () => { const next = new Outbox(options); await next.init(); return next; } };
}
const outcome = (source_generation = 1, outcome_id = "11111111-1111-4111-8111-111111111111") => ({
  outcome_id, source_generation, detail: "refused" as const, tag: "cyberPolicy" as const,
  phase: "implementation" as const, correlation_id: "pr-" + "a".repeat(32) + "-1",
  session_id: "session", head: "", published: false,
});

const proofFor = (p: { outcome: { outcome_id: string; source_generation: number }; delivery: { claim_generation: number } }) => ({
  outcome_id: p.outcome.outcome_id, source_generation: p.outcome.source_generation,
  claim_generation: p.delivery.claim_generation, inventory_complete: true, sources_safe: true, custody_settled: true,
});
function canonical(value: unknown): string {
  if (!value || typeof value !== "object") return JSON.stringify(value);
  return "{" + Object.keys(value).sort().map(k => JSON.stringify(k) + ":" + canonical((value as Record<string, unknown>)[k])).join(",") + "}";
}
async function resign(root: string, change: (record: Record<string, unknown>) => void) {
  const file = path.join(root, "r", "policy-1.json");
  const record = JSON.parse(await fs.readFile(file, "utf8")); delete record.mac;
  change(record);
  const key = await fs.readFile(path.join(root, ".key"));
  const mac = createHmac("sha256", key).update("uzi.outbox.policy.v1").update(canonical(record)).digest("hex");
  await fs.writeFile(file, JSON.stringify({ ...record, mac }));
}

it("policy first winner is immutable, authenticated after restart, and separate from terminal fallback", async () => {
  const { root, box, restart } = await setup();
  const first = await box.journalPolicy("r", outcome()); assert.equal(first.journaled, true, JSON.stringify(first));
  const other = await box.journalPolicy("r", outcome(1, "22222222-2222-4222-8222-222222222222"));
  assert.equal(other.journaled, true);
  if (!other.journaled) return;
  assert.equal(other.winner.outcome.outcome_id, outcome().outcome_id);
  assert.equal(other.written, false);
  assert.equal((await box.journalTerminal("r", 1, "implementation", 0, { status: "failed", detail: "unsupported API" })).journaled, true);
  const next = await restart();
  assert.equal(next.listPendingPolicies("r").length, 1);
  const file = path.join(root, "r", "policy-1.json");
  const record = JSON.parse(await fs.readFile(file, "utf8")); record.outcome.detail = "resume_rejected";
  await fs.writeFile(file, JSON.stringify(record));
  assert.equal(await next.readPolicyForRetirement("r", 1), undefined);
  assert.equal(await next.hasPhysicalTerminalProtection("r"), true);
});

it("retained settled A rebound to G2 coexists with B/G2 and rejects delayed old ACKs", async () => {
  const { box, restart } = await setup();
  await box.journalPolicy("r", outcome());
  const old = await box.readPolicyForRetirement("r", 1); assert.ok(old);
  await box.markPolicyServerSettled("r", 1, old.context);
  assert.equal(box.listPendingPolicies()[0]?.server_settled, true);
  const rebound = await box.bindPolicyDelivery("r", 1, 2, old.context.expectedIdentity!);
  assert.equal(rebound.bound, true);
  await box.markPolicyServerSettled("r", 1, old.context);
  assert.equal(box.listPendingPolicies()[0]?.server_settled, undefined);
  assert.equal((await box.journalPolicy("r", outcome(2, "22222222-2222-4222-8222-222222222222"))).journaled, true);
  const next = await restart();
  assert.deepEqual(next.listPendingPolicies().map(p => [p.outcome.source_generation, p.delivery.claim_generation]), [[1, 2], [2, 2]]);
  assert.equal(next.hasPendingPolicyForGeneration("r", 1), true);
  assert.equal(next.hasPendingPolicyForGeneration("r", 2), true);
  const current = await next.readPolicyForRetirement("r", 1); assert.ok(current);
  assert.deepEqual(current.request, { ...outcome(), claim_generation: 2 });
  assert.equal(await next.retirePolicyIfEligible("r", 1, current.context), false, "ACK is not source/custody proof");
});

for (const kind of ["malformed", "symlink", "oversized"] as const) it("physical policy protection: " + kind, async () => {
  const { root, box, restart } = await setup();
  await box.journalFinalize("r", 1);
  const file = path.join(root, "r", "policy-1.json");
  if (kind === "symlink") await fs.symlink("missing", file);
  else await fs.writeFile(file, kind === "oversized" ? "x".repeat(17000) : "{}");
  const next = await restart();
  assert.deepEqual(next.listPendingFinalizes(), []);
  assert.equal(await next.confirmReportsAbsent("r", 1), false);
  await next.retireRun("r"); await next.sweepRetention(Date.now() + 10000);
  assert.ok(await fs.lstat(file));
  assert.equal((await next.journalPolicy("r", outcome())).journaled, false);
});

it("closed policy contract rejects invalid metadata and identities", async () => {
  const { box } = await setup();
  for (const patch of [
    { outcome_id: "00000000-0000-0000-0000-000000000000" }, { source_generation: Number.MAX_SAFE_INTEGER + 1 },
    { phase: "plan" }, { tag: "unknown" }, { correlation_id: "pr-" + "a".repeat(32) + "-9007199254740992" },
    { session_id: "bad\n" }, { published: true }, { head: "A".repeat(40) }, { event: "provider_policy_refusal" },
  ]) assert.equal((await box.journalPolicy("r", { ...outcome(), ...patch } as never)).journaled, false, JSON.stringify(patch));
});

it("retirement needs complete interval source/custody proof and unlinks only the exact policy", async () => {
  const { root, box } = await setup();
  await box.journalPolicy("r", outcome());
  await box.journalFinalize("r", 1);
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  await box.markPolicyServerSettled("r", 1, observation.context);
  for (const missing of ["inventory_complete", "sources_safe", "custody_settled"] as const) {
    assert.equal(await box.retirePolicyIfEligible("r", 1, {
      ...observation.context, eligible: () => true,
      freshProof: async p => ({ ...proofFor(p), [missing]: false }),
    }), false);
  }
  assert.equal(await box.retirePolicyIfEligible("r", 1, {
    ...observation.context, eligible: () => true, freshProof: async p => proofFor(p),
  }), true);
  await assert.rejects(fs.lstat(path.join(root, "r", "policy-1.json")), { code: "ENOENT" });
  assert.ok(await fs.lstat(path.join(root, "r", "finalize-1.json")));
});

it("a rebind during fresh proof prevents stale retirement without holding the outbox lock", async () => {
  const { box } = await setup();
  await box.journalPolicy("r", outcome());
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  await box.markPolicyServerSettled("r", 1, observation.context);
  assert.equal(await box.retirePolicyIfEligible("r", 1, {
    ...observation.context, eligible: () => true,
    freshProof: async p => {
      assert.equal((await box.bindPolicyDelivery("r", 1, 2, observation.context.expectedIdentity!)).bound, true);
      return proofFor(p);
    },
  }), false);
  assert.equal(box.listPendingPolicies()[0]?.delivery.claim_generation, 2);
});

for (const mutation of ["ABA", "version"] as const) it("old ACK and proof reject physical " + mutation, async () => {
  const { root, box } = await setup();
  await box.journalPolicy("r", outcome());
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  const file = path.join(root, "r", "policy-1.json");
  if (mutation === "ABA") {
    const bytes = await fs.readFile(file);
    await fs.rename(file, file + ".old");
    await fs.writeFile(file, bytes);
  } else await fs.utimes(file, new Date(1), new Date(1));
  await box.markPolicyServerSettled("r", 1, observation.context);
  assert.equal(box.listPendingPolicies()[0]?.server_settled, undefined);
  assert.equal(await box.retirePolicyIfEligible("r", 1, {
    ...observation.context, eligible: () => true, freshProof: async p => proofFor(p),
  }), false);
});

it("proof cancellation and expired budgets retain the report", async () => {
  const { box } = await setup();
  await box.journalPolicy("r", outcome());
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  await box.markPolicyServerSettled("r", 1, observation.context);
  const controller = new AbortController();
  assert.equal(await box.retirePolicyIfEligible("r", 1, {
    ...observation.context, signal: controller.signal, eligible: () => true,
    freshProof: async p => { controller.abort(); return proofFor(p); },
  }), false);
  assert.equal(await box.retirePolicyIfEligible("r", 1, {
    ...observation.context, deadline: Date.now() - 1, eligible: () => true, freshProof: async p => proofFor(p),
  }), false);
  assert.equal(box.hasPendingPolicy("r"), true);
});

for (const operation of ["install", "adopt", "bind"] as const) it("strict directory fsync failure rejects policy " + operation, async t => {
  const { root, box } = await setup();
  if (operation !== "install") await box.journalPolicy("r", outcome());
  const observation = operation === "bind" ? await box.readPolicyForRetirement("r", 1) : undefined;
  const open = fs.open;
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const fh = await open(...args);
    if (String(args[0]) === path.join(root, "r")) {
      t.mock.method(fh, "sync", async () => { throw Object.assign(new Error("sync failed"), { code: "EIO" }); });
    }
    return fh;
  });
  if (operation === "bind") {
    assert.ok(observation);
    assert.equal((await box.bindPolicyDelivery("r", 1, 2, observation.context.expectedIdentity!)).bound, false);
  } else assert.equal((await box.journalPolicy("r", outcome())).journaled, false);
  assert.equal(await box.hasPhysicalTerminalProtection("r"), true);
});

for (const operation of ["write", "bind"] as const) it("ENOSPC " + operation + " preserves terminal reserve and retries once", async () => {
  let enabled = false;
  let writes = 0;
  let rootPath = "";
  const { root, box } = await setup({ reserveBytes: 65536, rawWrite: async (write, ctx) => {
    if (enabled && ctx.kind === "policy" && ctx.path.endsWith(".tmp") && ++writes === 1) {
      throw Object.assign(new Error("full"), { code: "ENOSPC" });
    }
    if (enabled && ctx.kind === "policy" && writes === 2) assert.ok((await fs.lstat(path.join(rootPath, ".reserve"))).size >= 65536);
    await write();
  } });
  rootPath = root;
  if (operation === "write") {
    enabled = true;
    assert.equal((await box.journalPolicy("r", outcome())).journaled, true);
  } else {
    await box.journalPolicy("r", outcome());
    const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
    enabled = true;
    assert.equal((await box.bindPolicyDelivery("r", 1, 2, observation.context.expectedIdentity!)).bound, true);
  }
  assert.equal(writes, 2);
});

it("binding rechecks the installed version after the temp write before replacing it", async () => {
  let interfere = false;
  let rootPath = "";
  const { root, box } = await setup({ rawWrite: async (write, ctx) => {
    await write();
    if (interfere && ctx.kind === "policy" && ctx.path.endsWith(".tmp")) {
      interfere = false;
      await fs.utimes(path.join(rootPath, "r", "policy-1.json"), new Date(1), new Date(1));
    }
  } });
  rootPath = root;
  await box.journalPolicy("r", outcome());
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  interfere = true;
  assert.equal((await box.bindPolicyDelivery("r", 1, 2, observation.context.expectedIdentity!)).bound, false);
  assert.equal(JSON.parse(await fs.readFile(path.join(root, "r", "policy-1.json"), "utf8")).delivery.claim_generation, 1);
});

it("policy controls survive segment quota eviction and exclude every finalize retirement path", async () => {
  const { root, box } = await setup();
  await box.journalFinalize("r", 1);
  const finalizeIdentity = box.finalizeRecordIdentity("r", 1)!;
  await box.journalPolicy("r", outcome());
  for (let seq = 1; seq <= 4; seq++) await box.appendSegment("r", 1, [
    { seq, kind: "status", payload: { text: "x".repeat(700) } },
  ]);
  assert.ok(await fs.lstat(path.join(root, "r", "policy-1.json")));
  assert.equal(box.depthFor("r")?.pendingPolicy, 1);
  assert.deepEqual(box.listPendingFinalizes(), []);
  assert.deepEqual(box.listPendingFinalizeGenerations(true), []);
  assert.equal(await box.retireFinalize("r", 1), false);
  assert.equal(await box.retireFinalizeIfEligible("r", 1, () => true, new AbortController().signal, finalizeIdentity), false);
});

it("key loss retains policy files and disables sending after restart", async () => {
  const { root, box, restart } = await setup();
  await box.journalPolicy("r", outcome());
  await fs.unlink(path.join(root, ".key"));
  const next = await restart();
  assert.equal(next.isDisabled(), true);
  assert.equal(next.hasPendingPolicy("r"), true);
  assert.equal(await next.readPolicyForRetirement("r", 1), undefined);
  await next.retireRun("r");
  assert.ok(await fs.lstat(path.join(root, "r", "policy-1.json")));
});

for (const field of ["outer", "outcome", "delivery", "backward"] as const) it("authenticated policy parsing is closed: " + field, async () => {
  const { root, box, restart } = await setup();
  await box.journalPolicy("r", outcome());
  await resign(root, () => undefined);
  assert.ok(await box.readPolicyForRetirement("r", 1), "resigning must produce an authenticated fixture");
  await resign(root, record => {
    if (field === "outer") record.unexpected = true;
    if (field === "outcome") (record.outcome as Record<string, unknown>).event = "provider_policy_refusal";
    if (field === "delivery") (record.delivery as Record<string, unknown>).extra = true;
    if (field === "backward") (record.delivery as Record<string, unknown>).claim_generation = 0;
  });
  const next = await restart();
  assert.deepEqual(next.listPendingPolicies(), []);
  assert.equal(await next.hasPhysicalTerminalProtection("r"), true);
  assert.equal(await next.readPolicyForRetirement("r", 1), undefined);
});

it("source-through-current predicate is inclusive and binding cannot move backwards", async () => {
  const { box } = await setup();
  const first = await box.journalPolicy("r", outcome(2));
  assert.equal(first.journaled, true); if (!first.journaled) return;
  assert.equal(box.hasPendingPolicyForGeneration("r", 1), false);
  assert.equal(box.hasPendingPolicyForGeneration("r", 2), true);
  assert.equal(box.hasPendingPolicyForGeneration("r", 3), false);
  const bound = await box.bindPolicyDelivery("r", 2, 4, first.context.expectedIdentity!);
  assert.equal(bound.bound, true); if (!bound.bound) return;
  assert.equal(box.hasPendingPolicyForGeneration("r", 3), true);
  assert.equal(box.hasPendingPolicyForGeneration("r", 4), true);
  assert.equal(box.hasPendingPolicyForGeneration("r", 5), false);
  assert.equal((await box.bindPolicyDelivery("r", 2, 3, bound.context.expectedIdentity!)).bound, false);
  assert.equal((await box.bindPolicyDelivery("r", 2, 5, first.context.expectedIdentity!)).bound, false);
});

it("logical pending policies cannot be bypassed by deleting their physical file", async () => {
  const { root, box } = await setup();
  await box.journalFinalize("r", 1);
  await box.journalPolicy("r", outcome());
  await fs.unlink(path.join(root, "r", "policy-1.json"));
  assert.equal(await box.confirmReportsAbsent("r", 1), false);
  assert.equal(await box.confirmTerminalAbsent("r", 1), false);
  assert.equal(await box.retireFinalize("r", 1), false);
  await box.retireRun("r");
  assert.ok(await fs.lstat(path.join(root, "r", "finalize-1.json")));
});

it("an unrelated durable write cannot erase a previously authenticated missing outcome", async () => {
  const { root, box } = await setup();
  await box.journalPolicy("r", outcome());
  await fs.unlink(path.join(root, "r", "policy-1.json"));
  assert.equal(await box.readPolicyForRetirement("r", 1), undefined);
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), true);
  assert.equal((await box.journalPolicy("r", outcome(2))).journaled, true);
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), true, "lost authenticated evidence remains unknown");
  assert.equal(await box.confirmReportsAbsent("r", 1), false);
});

it("ACKed policy custody evidence does not block independent finalize attestation", async () => {
  const { box } = await setup();
  await box.journalPolicy("r", outcome());
  await box.journalFinalize("r", 2);
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  await box.markPolicyServerSettled("r", 1, observation.context);
  assert.deepEqual(box.listPendingFinalizes(), [{ run_id: "r", claim_generation: 2 }]);
  assert.deepEqual(box.listPendingFinalizeGenerations(), [{ run_id: "r", claim_generation: 2 }]);
  assert.equal(await box.retireFinalize("r", 2), true);
  assert.equal(box.hasPendingPolicy("r"), true, "source evidence stays protected");
  assert.equal(await box.hasPhysicalTerminalProtection("r"), true);
});

it("unknown policy beside ACKed evidence still blocks finalize retirement", async () => {
  const { root, box } = await setup();
  await box.journalPolicy("r", outcome());
  await box.journalFinalize("r", 2);
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  await box.markPolicyServerSettled("r", 1, observation.context);
  await fs.writeFile(path.join(root, "r", "policy-unknown.json"), "{}");
  assert.equal(await box.retireFinalize("r", 2), false);
  assert.deepEqual(box.listPendingFinalizes(), []);
  assert.equal(box.hasPendingPolicy("r"), true);
});

it("physical control count is bounded independently of segment quotas", async () => {
  const { root, box } = await setup();
  await fs.mkdir(path.join(root, "r"));
  for (let source = 1; source <= 256; source++) await fs.writeFile(path.join(root, "r", "policy-" + source + ".json"), "{}");
  const result = await box.journalPolicy("r", outcome(257));
  assert.equal(result.journaled, false);
  if (!result.journaled) assert.equal(result.reason, "control_capacity");
  assert.equal((await fs.readdir(path.join(root, "r"))).length, 256);
});

it("terminal rejection cleanup never acquires authority over policy records", async () => {
  const { root, box } = await setup();
  await box.journalPolicy("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", outcome());
  const observation = await box.observeTerminalAuthentication("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 1, "policy-1.json");
  assert.equal(observation.kind, "malformed");
  let queried = false;
  assert.equal(await box.cleanupRejectedTerminalFiles("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", 1,
    "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", [observation], async () => { queried = true; return undefined; }), 0);
  assert.equal(queried, false);
  assert.ok(await fs.lstat(path.join(root, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "policy-1.json")));
});

it("policy write ENOSPC retries once and failure never offers an unjournaled request", async () => {
  let writes = 0;
  const { box } = await setup({ rawWrite: async (write, ctx) => {
    if (ctx.kind === "policy" && ++writes <= 2) throw Object.assign(new Error("full"), { code: "ENOSPC" });
    await write();
  } });
  assert.equal((await box.journalPolicy("r", outcome())).journaled, false);
  assert.equal(writes, 2);
  assert.deepEqual(box.listPendingPolicies(), []);
  assert.equal((await box.journalPolicy("r", outcome())).journaled, true);
});

it("complete source and custody proof cannot retire a policy before its server ACK", async () => {
  const { root, box } = await setup();
  await box.journalPolicy("r", outcome());
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  let proofs = 0;
  assert.equal(await box.retirePolicyIfEligible("r", 1, {
    ...observation.context, eligible: () => true,
    freshProof: async p => { proofs++; return proofFor(p); },
  }), false);
  assert.equal(proofs, 0);
  assert.ok(await fs.lstat(path.join(root, "r", "policy-1.json")));
  assert.equal(box.hasPendingPolicy("r"), true);
});

for (const operation of ["journal", "read", "bind"] as const) {
  for (const kind of ["malformed", "symlink", "unsafe-directory"] as const) {
    it("late unknown policy protects logical state on " + operation + ": " + kind, async () => {
      const { root, box } = await setup();
      if (kind === "unsafe-directory") {
        await fs.mkdir(path.join(root, "elsewhere"));
        await fs.symlink("elsewhere", path.join(root, "r"));
      } else {
        await fs.mkdir(path.join(root, "r"));
        const file = path.join(root, "r", "policy-1.json");
        if (kind === "symlink") await fs.symlink("missing", file);
        else await fs.writeFile(file, "{}");
      }
      if (operation === "journal") {
        assert.deepEqual(await box.journalPolicy("r", outcome()), { journaled: false, reason: "winner_unavailable" });
      } else if (operation === "read") assert.equal(await box.readPolicyForRetirement("r", 1), undefined);
      else assert.equal((await box.bindPolicyDelivery("r", 1, 2, {})).bound, false);
      assert.equal(box.hasPendingPolicy("r"), true);
      assert.equal(box.hasPendingPolicyForGeneration("r", 99), true);
    });
  }
}

it("positive policy absence does not poison logical protection", async () => {
  const { box } = await setup();
  assert.equal(await box.readPolicyForRetirement("r", 1), undefined);
  assert.equal((await box.bindPolicyDelivery("r", 1, 2, {})).bound, false);
  assert.equal(box.hasPendingPolicy("r"), false);
  await box.journalFinalize("r", 1);
  assert.equal(await box.readPolicyForRetirement("r", 1), undefined);
  assert.equal(box.hasPendingPolicy("r"), false);
  assert.equal((await box.journalPolicy("r", outcome())).journaled, true);
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), false);
});

for (const unknown of [false, true]) it("durable retry rescans sticky protection; other unknown control = " + unknown, async () => {
  let fail = true;
  const { root, box } = await setup({ rawWrite: async (write, ctx) => {
    if (fail && ctx.kind === "policy") throw Object.assign(new Error("full"), { code: "ENOSPC" });
    await write();
  } });
  assert.equal((await box.journalPolicy("r", outcome())).journaled, false);
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), true);
  if (unknown) {
    await fs.mkdir(path.join(root, "r"), { recursive: true });
    await fs.writeFile(path.join(root, "r", "policy-unknown.json"), "{}");
  }
  fail = false;
  assert.equal((await box.journalPolicy("r", outcome())).journaled, true);
  assert.equal(box.hasPendingPolicyForGeneration("r", 1), true);
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), unknown);
});

it("post-unlink policy directory sync failure keeps logical protection", async t => {
  const { root, box } = await setup();
  await box.journalPolicy("r", outcome());
  const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
  await box.markPolicyServerSettled("r", 1, observation.context);
  let unlinked = false;
  const unlink = fs.unlink;
  t.mock.method(fs, "unlink", async (...args: Parameters<typeof fs.unlink>) => {
    await unlink(...args);
    if (String(args[0]) === path.join(root, "r", "policy-1.json")) unlinked = true;
  });
  const open = fs.open;
  t.mock.method(fs, "open", async (...args: Parameters<typeof fs.open>) => {
    const fh = await open(...args);
    if (unlinked && String(args[0]) === path.join(root, "r")) {
      t.mock.method(fh, "sync", async () => { throw Object.assign(new Error("sync failed"), { code: "EIO" }); });
    }
    return fh;
  });
  assert.equal(await box.retirePolicyIfEligible("r", 1, {
    ...observation.context, eligible: () => true, freshProof: async p => proofFor(p),
  }), false);
  assert.equal(unlinked, true);
  assert.deepEqual(box.listPendingPolicies(), []);
  assert.equal(box.hasPendingPolicy("r"), true);
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), true);
});

for (const operation of ["adopt", "bind"] as const) it("durable " + operation + " retry clears only rescanned protection", async () => {
  let fail = false;
  const { box } = await setup({ rawWrite: async (write, ctx) => {
    if (fail && ctx.kind === "policy") throw Object.assign(new Error("full"), { code: "ENOSPC" });
    await write();
  } });
  const first = await box.journalPolicy("r", outcome()); assert.ok(first.journaled);
  fail = true;
  assert.equal((await box.bindPolicyDelivery("r", 1, 2, first.context.expectedIdentity!)).bound, false);
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), true);
  fail = false;
  if (operation === "adopt") {
    const adopted = await box.journalPolicy("r", outcome()); assert.ok(adopted.journaled);
  } else {
    const observation = await box.readPolicyForRetirement("r", 1); assert.ok(observation);
    assert.equal(box.hasPendingPolicyForGeneration("r", 99), true);
    assert.equal((await box.bindPolicyDelivery("r", 1, 2, observation.context.expectedIdentity!)).bound, true);
    assert.equal(box.hasPendingPolicyForGeneration("r", 2), true);
  }
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), false);
});

it("an incomplete retry rescan preserves sticky protection until a complete scan succeeds", async t => {
  let fail = true;
  const { root, box } = await setup({ rawWrite: async (write, ctx) => {
    if (fail && ctx.kind === "policy") throw Object.assign(new Error("full"), { code: "ENOSPC" });
    await write();
  } });
  assert.equal((await box.journalPolicy("r", outcome())).journaled, false);
  fail = false;
  const opendir = fs.opendir;
  let scans = 0;
  const mock = t.mock.method(fs, "opendir", async (...args: Parameters<typeof fs.opendir>) => {
    if (String(args[0]) === path.join(root, "r")) {
      scans++;
      assert.equal(box.hasPendingPolicyForGeneration("r", 99), true, "sticky protection remains during the scan");
      throw Object.assign(new Error("scan failed"), { code: "EIO" });
    }
    return opendir(...args);
  });
  assert.equal((await box.journalPolicy("r", outcome())).journaled, true);
  assert.ok(scans > 0);
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), true);
  mock.mock.restore();
  assert.equal((await box.journalPolicy("r", outcome())).journaled, true);
  assert.equal(box.hasPendingPolicyForGeneration("r", 99), false);
});
