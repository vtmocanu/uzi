// Issue #4627: a retained claim whose bare storage identity (device or inode) changed while the owner record's path, branch, key
// and run still match is held for its owner at `recovery_wait` / `retained_recovery_adoption_failed` instead of failing the run,
// but only on the first reservation iteration of a claim, on a journal that is not blocked, and only when the journal still names
// the exact reservation the owner record committed. Real Git, the production RunRunner.execute() and the fake API; the identity
// change is injected through the `recoveryOwnerBareIdentity` seam (as runner-recovery-owner-realgit.test.ts does).
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { describe, it, type TestContext } from "node:test";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { RequestError } from "../src/client.js";
import * as gitModule from "../src/git.js";
import type { MemoryParkRecord } from "../src/git.js";
import type { ClaimResponse } from "../src/protocol.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { treeDigest } from "./restore-route-rig.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const FEATURE = "recovery_cause_retained_recovery_adoption_failed";
const EXHAUSTED_FEATURE = "recovery_cause_retained_recovery_exhausted";
const GUIDANCE = "storage identity changed";
const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };

function command(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", stdio: "pipe", env: GIT_ENV }).trim();
}
/** Names and bytes of every work-tree file outside `.git` (git's own bookkeeping legitimately moves under a read). */
function workTree(dir: string): string {
  const h = createHash("sha256");
  const walk = (d: string): void => {
    for (const name of fs.readdirSync(d).sort()) {
      if (d === dir && name === ".git") continue;
      const p = path.join(d, name);
      const st = fs.lstatSync(p);
      h.update(`${path.relative(dir, p)}\0${st.mode}\0`);
      if (st.isDirectory()) walk(p); else if (st.isFile()) h.update(fs.readFileSync(p));
    }
  };
  walk(dir);
  return h.digest("hex");
}
/** The whole bare: its local config and its refs, as bytes. */
const dumps = (bare: string) => ({ config: command(bare, "config", "--local", "--list"), refs: command(bare, "for-each-ref") });

interface Flight {
  parked?: boolean; bareIdentityHold?: boolean; preserveRecoveryClone?: boolean; preserveSession?: boolean;
  retainedLocalCustody?: boolean; claimGeneration: number; runId: string;
  steering: { pendingCredentialSwitch(): number | undefined };
}
type Journal = { runId: string; clonePath: string; recovery?: Record<string, unknown> } & Record<string, unknown>;
type Which = "dev" | "ino" | "path";
type AckHook = (body: Record<string, unknown>, original: () => Promise<Record<string, unknown>>) => Promise<Record<string, unknown>>;

interface Rig {
  claim: ClaimResponse;
  bare: string; clonePath: string; branch: string; key: string; journalKey: string; ownerKey: string;
  registry: ActiveRunRegistry;
  logs: ReturnType<typeof recordingLogger>;
  /** Park bodies the runner sent, in order. */
  parks: Array<Record<string, unknown>>;
  /** Every state report the runner made, in order: the status, with a park's cause. */
  sequence: string[];
  /** How many guidance lines the feed already held when the first park was requested. */
  guidanceAtPark?: number;
  counts: { models: number; captures: number; ledger: number; blocks: number; reserves: number; rejections: unknown[]; holds: number; freeCaptures: number;
    /** How many times the claim exit retired the runner clone (a teardown step). */
    retires: number };
  /** The run's own HOME dir, seeded with a marker file before the claim. */
  home: string;
  flights: Flight[];
  ackHook?: AckHook;
  mismatch: { on: boolean; which: Which };
  execute(): Promise<void>;
  journal(): Journal;
  setJournal(mutate: (j: Journal) => void): void;
  owner(): Record<string, unknown>;
  rawOwner(): string;
}

const IID = 4627;
const UUID_A = "11111111-1111-4111-8111-111111111111";
const UUID_B = "22222222-2222-4222-8222-222222222222";
const UUID_C = "33333333-3333-4333-8333-333333333333";

/** A settled memory-park record of an earlier claim, journaled beside the recovery reservation. */
function settledPark(runId: string, clonePath: string): MemoryParkRecord {
  return {
    version: 1, binding: { runId, claimGeneration: 2, workerId: "worker-1", registerNonce: "nonce-1", memoryEpisode: 3 }, ack: "confirmed",
    origin: { flightId: UUID_A, attemptId: null, executorIncarnation: null, authorityIncarnation: UUID_B, sourceEpoch: UUID_C },
    settlement: { version: 1, confirmed: true, sources: [{ path: clonePath, authorityKey: "1".padStart(64, "a"), attemptId: null, epoch: UUID_C, seq: 1 }],
      evidence: { parkAck: { runId, claimGeneration: 2, workerId: "worker-1", registerNonce: "nonce-1", memoryEpisode: 3 }, interventionIds: [],
        processJoin: "executor", commands: "not_required", steering: "joined", auxiliaries: "none", feedback: "drained", sinkReceipts: "joined",
        parkReap: "not_applicable" } },
  };
}

interface RigOptions {
  /** The journal the previous claim left: a plain reservation (not blocked), or blocked with the named blocker. */
  journal?: "plain" | "budget_exhausted" | "adoption_failed";
  memoryPark?: boolean;
  features?: string[];
  claim?: Record<string, unknown>;
  /** Reserve this many iterations before the claim under test. */
  reserved?: number;
  runnerExtra?: Record<string, unknown>;
}

async function build(t: TestContext, o: RigOptions = {}): Promise<Rig> {
  const claim = gitlabClaim(IID, { claim_generation: 3, worker_recovery_episode: 7, worker_recovery_owner_episode: 0, ...o.claim }) as ClaimResponse;
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  const clone = await git.createOrAttachRunnerClone(bare, IID, noProofReseed, claim.run_id);
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "local retained work\n");
  const key = `issue-${IID}`;
  const source = { runId: claim.run_id, clonePath: clone.path };
  const kind = o.journal ?? "plain";
  const first = await git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 2, 7, 0);
  for (let i = 1; i < (o.reserved ?? 1); i++) await git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 2, 7, 0);
  if (kind === "budget_exhausted") {
    for (let i = (o.reserved ?? 1); i < 3; i++) await git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 2, 7, 0);
    await assert.rejects(git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 2, 7, 0), /budget/);
  } else if (kind === "adoption_failed") await git.blockRecoveryEpisode(bare, clone.branch, key, source, "adoption_failed", first);
  if (o.memoryPark) {
    assert.equal(await git.recordMemoryPark(bare, clone.branch, { runId: claim.run_id, clonePath: clone.path }, settledPark(claim.run_id, clone.path)), "settled");
  }
  client.protocolFeatures = o.features ?? [FEATURE, EXHAUSTED_FEATURE];
  const logs = recordingLogger();
  const journalKey = `uzi-recovery.${clone.branch}.clone`;
  const ownerKey = `uzi-recoveryowner.${claim.run_id}.record`;
  const rig: Rig = {
    claim, bare, clonePath: clone.path, branch: clone.branch, key, journalKey, ownerKey, registry: new ActiveRunRegistry(), logs,
    parks: [], sequence: [],
    counts: { models: 0, captures: 0, ledger: 0, blocks: 0, reserves: 0, rejections: [], holds: 0, freeCaptures: 0,
      retires: 0 },
    home: path.join(homeDir, claim.run_id),
    flights: [], mismatch: { on: false, which: "dev" },
    journal: () => JSON.parse(command(bare, "config", journalKey)) as Journal,
    setJournal: mutate => {
      const j = JSON.parse(command(bare, "config", journalKey)) as Journal;
      mutate(j);
      execFileSync("git", ["-C", bare, "config", "--local", journalKey, JSON.stringify(j)], { env: GIT_ENV, stdio: "pipe" });
    },
    owner: () => JSON.parse(command(bare, "config", ownerKey)) as Record<string, unknown>,
    rawOwner: () => command(bare, "config", ownerKey),
    execute: async () => undefined,
  };
  const seam = git as any;
  const identity = JSON.parse(await seam.recoveryOwnerBareIdentity(bare)) as string[];
  const real = seam.recoveryOwnerBareIdentity.bind(git);
  t.mock.method(seam, "recoveryOwnerBareIdentity", async (...args: unknown[]) => {
    if (!rig.mismatch.on) return real(...args);
    const shifted = [...identity];
    if (rig.mismatch.which === "path") shifted[0] = identity[0] + "-moved";
    else shifted[rig.mismatch.which === "dev" ? 1 : 2] = String(BigInt(identity[rig.mismatch.which === "dev" ? 1 : 2]!) + 1n);
    return JSON.stringify(shifted);
  });
  const reserve = git.reserveRecoveryIteration.bind(git);
  t.mock.method(git, "reserveRecoveryIteration", async (...args: Parameters<typeof reserve>) => {
    rig.counts.reserves++;
    try { return await reserve(...args); } catch (error) { rig.counts.rejections.push(error); throw error; }
  });
  const capture = git.recordRecoveryCapture.bind(git);
  t.mock.method(git, "recordRecoveryCapture", async (...args: Parameters<typeof capture>) => { rig.counts.captures++; return capture(...args); });
  const block = git.blockRecoveryEpisode.bind(git);
  t.mock.method(git, "blockRecoveryEpisode", async (...args: Parameters<typeof block>) => { rig.counts.blocks++; return block(...args); });
  const ledger = seam.appendAttemptLedger.bind(git);
  t.mock.method(seam, "appendAttemptLedger", async (...args: unknown[]) => { rig.counts.ledger++; return ledger(...args); });
  const report = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (runId: Parameters<typeof report>[0], body: Parameters<typeof report>[1], signal?: AbortSignal) => {
    const original = () => report(runId, body, signal) as unknown as Promise<Record<string, unknown>>;
    const isPark = body.status === "recovery_wait" && typeof body.recovery_cause === "string";
    rig.sequence.push(isPark ? `${body.status}:${body.recovery_cause}` : String(body.status));
    if (isPark) {
      rig.parks.push(body as unknown as Record<string, unknown>);
      rig.guidanceAtPark ??= guidance(rig).length;
    }
    if (isPark && rig.ackHook) return rig.ackHook(body as unknown as Record<string, unknown>, original) as never;
    if (isPark) return { status: body.status, recoveryCause: body.recovery_cause } as never;
    return report(runId, body, signal);
  });
  // Counted, never replaced: the clone retire a claim exit performs.
  {
    const original = (seam.retireRunnerClone as (...a: unknown[]) => Promise<unknown>).bind(git);
    t.mock.method(seam, "retireRunnerClone", async (...args: unknown[]) => { rig.counts.retires++; return original(...args); });
  }
  rig.execute = async () => {
    api.setOwnershipStatus(claim.run_id, "running", claim.claim_generation);
    const runner = runnerWith(runId => ({ homeDir: path.join(homeDir, runId),
      executor: { run: async () => { rig.counts.models++; throw new Error("must not model"); } } }),
      fakeGitlab().gitlab, undefined, logs.logger, { recoveryRetryMs: 1, activeRuns: rig.registry, ...o.runnerExtra });
    const priv = runner as unknown as Record<string, (...a: unknown[]) => unknown>;
    // The hold exists only with the fix; a runner without it is observed through the other counters.
    if (typeof priv.holdForBareIdentityChange === "function") {
      const hold = priv.holdForBareIdentityChange.bind(runner);
      t.mock.method(priv, "holdForBareIdentityChange", (...args: unknown[]) => { rig.counts.holds++; rig.flights.push(args[1] as Flight); return hold(...args); });
    }
    const free = priv.runFreeCapture!.bind(runner);
    t.mock.method(priv, "runFreeCapture", (...args: unknown[]) => { rig.counts.freeCaptures++; return free(...args); });
    await runner.execute(claim);
  };
  return rig;
}

const feed = (r: Rig): string[] => api.messages(r.claim.run_id).map(m => String((m.payload as { text?: string }).text ?? ""));
const guidance = (r: Rig): string[] => feed(r).filter(t => t.includes(GUIDANCE));
const logged = (r: Rig, msg: string) => r.logs.lines.filter(l => (l as { msg: string }).msg === msg) as Array<Record<string, unknown>>;
const adoptionParks = (r: Rig) => r.parks.filter(p => p.recovery_cause === "retained_recovery_adoption_failed");
const failures = (r: Rig) => api.states.filter(s => s.runId === r.claim.run_id && s.body.status === "failed");
/**
 * Resolved at runtime so this file also loads on a baseline without the typed error; the
 * assertions that need it assert it is defined, so they fail there for the right reason.
 */
const RecoveryOwnerBareIdentityError = (gitModule as Record<string, unknown>).RecoveryOwnerBareIdentityError as
  (new (...a: never[]) => Error) | undefined;
/** The hold-authorizing subclass, constructed only by the read-only probe; resolved at runtime for the same reason. */
const HoldError = (gitModule as Record<string, unknown>).RecoveryOwnerBareIdentityHoldError as
  (new (...a: never[]) => Error) | undefined;
/**
 * Today's disposition for a base-class error: fail() runs, and its own recoveryReservationSnapshot re-read hits the same
 * (still mismatched) identity, so the blocker cannot be persisted and the reason says so; custody is kept regardless.
 */
const FAIL_SHAPE = (r: Rig): void => {
  assert.deepEqual(failures(r).map(f => f.body.failure_reason),
    ["Retained recovery blocked: preservation failed; blocker persistence unavailable; local work and custody retained"]);
  assert.match(String((r.counts.rejections[0] as Error).message), /^recovery owner binding changed: bare storage identity$/);
};
/** The owner record bytes after the reservation died mid-way: the pending charge is consumed and left, deliberately. */
const OWNER_SHAPE = (r: Rig, name: string, before: string): void => {
  const was = JSON.parse(before) as { consumed: number; after: { attempts: number } };
  const now = r.owner() as { state: string; consumed: number; after: { attempts: number } };
  assert.equal(was.after.attempts, 1);
  assert.equal(now.after.attempts, 2, "the charge for this iteration was written");
  assert.equal(now.consumed, was.consumed, "no raw grant was consumed");
  assert.equal(now.state, name === "pending charge" ? "pending" : "committed");
  assert.equal(r.parks.length, 0, "no park of any cause");
  assert.equal(r.journal().recovery!.attempts, name === "pending charge" ? 1 : 2,
    "the journal reservation is written only after the pending charge");
};
const typedRejections = (r: Rig) => r.counts.rejections.filter(e => RecoveryOwnerBareIdentityError !== undefined && e instanceof RecoveryOwnerBareIdentityError);

/** The shared "this was held" assertions of both journals. */
async function assertHeld(t: TestContext, r: Rig, before: ReturnType<typeof dumps>, ownerBefore: string, journalBefore: string, captured: number): Promise<void> {
  void t;
  // Behaviour first, so a baseline fails on the missing hold (a failed report) and not on the missing class.
  assert.equal(failures(r).length, 0, `nothing was failed: ${JSON.stringify(failures(r).map(f => f.body))}`);
  assert.equal(adoptionParks(r).length, 1, JSON.stringify(api.states));
  assert.equal(r.counts.holds, 1);
  assert.ok(RecoveryOwnerBareIdentityError, "git.ts exports RecoveryOwnerBareIdentityError");
  assert.equal(typedRejections(r).length, 1, "the reservation rejected with the typed error");
  assert.equal(adoptionParks(r)[0]!.claim_generation, r.claim.claim_generation);
  assert.equal(adoptionParks(r)[0]!.recovery_detail, undefined, "no detail rides the hold");
  assert.equal(guidance(r).length, 1, "one guidance line");
  const line = guidance(r)[0]!;
  assert.ok(line.includes("execution was refused") && line.includes("uzi run recovery") && line.includes("Cancel ends the run"), line);
  assert.ok(line.length <= 1200 && !line.endsWith("..."));
  assert.equal(r.guidanceAtPark, 1, "the hold is requested only after the guidance reached the server");
  assert.equal(r.flights[0]!.parked, true, "parked only after an ACK naming recovery_wait and the cause");
  assert.equal(r.flights[0]!.bareIdentityHold, true);
  assert.equal(r.flights[0]!.preserveRecoveryClone, true);
  assert.equal(r.flights[0]!.preserveSession, true);
  assert.equal(r.flights[0]!.retainedLocalCustody, true);
  assert.equal(failures(r).length, 0, "nothing was failed");
  assert.equal(r.counts.models, 0);
  assert.equal(r.counts.captures, captured, "no capture");
  assert.equal(r.counts.ledger, 0, "no attempt ledger append");
  assert.equal(r.counts.blocks, 0, "no blocker recorded");
  assert.equal(r.counts.freeCaptures, 0, "no quiescence capture");
  assert.deepEqual(dumps(r.bare), before, "the bare's whole config and refs are byte-identical");
  assert.equal(r.rawOwner(), ownerBefore, "the owner record is byte-identical");
  assert.equal(JSON.stringify(r.journal()), journalBefore, "the journal is byte-identical");
  assert.notEqual(r.owner().state, "pending");
  assert.equal(fs.readFileSync(path.join(r.clonePath, "retained.txt"), "utf8"), "local retained work\n");
  assert.equal(r.registry.has(r.claim.run_id), false, "the flight left the active snapshot");
}

async function armedHold(t: TestContext, o: RigOptions = {}, which: Which = "dev") {
  const r = await build(t, o);
  r.mismatch.which = which;
  r.mismatch.on = true;
  const before = dumps(r.bare);
  return { r, before, ownerBefore: r.rawOwner(), journalBefore: JSON.stringify(r.journal()) };
}

describe("a changed bare storage identity is held for the owner on the first iteration of a not-blocked journal", () => {
  for (const memoryPark of [true, false]) for (const which of ["dev", "ino"] as const) {
    it(`${memoryPark ? "with a settled memory park" : "without a memory park"}: ${which} changed -> held, nothing written`, async t => {
      const { r, before, ownerBefore, journalBefore } = await armedHold(t, { memoryPark }, which);
      const captured = r.counts.captures;
      await r.execute();
      await assertHeld(t, r, before, ownerBefore, journalBefore, captured);
      assert.equal(logged(r, "retained recovery bare identity change held").length, 1);
      assert.equal(logged(r, "retained recovery bare identity change hold unconfirmed").length, 0);
      if (memoryPark) assert.ok(r.journal().memoryPark, "the memory park record is untouched");
    });
  }

  it("a second claim at the next generation and episode holds again and consumes no grant", async t => {
    const { r, before, ownerBefore, journalBefore } = await armedHold(t);
    await r.execute();
    const consumed = r.owner().consumed;
    r.claim.claim_generation = 4;
    r.claim.worker_recovery_episode = 8;
    r.claim.worker_recovery_owner_episode = 1;
    await r.execute();
    assert.equal(adoptionParks(r).length, 2);
    assert.equal(r.counts.holds, 2);
    assert.equal(r.owner().consumed, consumed, "the consumed high-water mark did not move");
    assert.equal(r.rawOwner(), ownerBefore);
    assert.equal(JSON.stringify(r.journal()), journalBefore);
    assert.deepEqual(dumps(r.bare), before);
    assert.equal(failures(r).length, 0);
    assert.equal(r.counts.models, 0);
  });

  it("the hold request fences on the claim's generation", async t => {
    const { r } = await armedHold(t);
    await r.execute();
    assert.equal(adoptionParks(r)[0]!.claim_generation, r.flights[0]!.claimGeneration);
    assert.equal(r.flights[0]!.claimGeneration, r.claim.claim_generation);
  });
});

describe("controls: only the exact typed first-iteration mismatch is held", () => {
  const notHeld = (r: Rig, why: string): void => {
    assert.equal(r.counts.holds, 0, why);
    assert.equal(guidance(r).length, 0, why + ": no guidance line");
    assert.equal(r.flights.length, 0, why);
  };

  const PRESERVE = "Retained recovery blocked: preservation failed; blocker persistence unavailable; local work and custody retained";
  const CORRUPT = "Retained recovery blocked: corrupt journal or unsafe local recovery storage; persistence unavailable; local work and custody retained";
  /** Today's disposition: exactly one failed report with this reason, and nothing parked. */
  const failedWith = (r: Rig, reason: string): void => {
    assert.deepEqual(failures(r).map(f => f.body.failure_reason), [reason]);
    assert.equal(r.parks.length, 0, "no park of any cause");
  };

  it("a changed path is not the typed error and takes today's path", async t => {
    const { r } = await armedHold(t, {}, "path");
    await r.execute();
    notHeld(r, "path");
    failedWith(r, PRESERVE);
    assert.equal(typedRejections(r).length, 0);
    assert.equal(r.counts.rejections.length, 1);
    assert.match(String((r.counts.rejections[0] as Error).message), /^recovery owner binding changed$/);
  });

  for (const [name, mutate] of [
    ["branch", (o: Record<string, unknown>) => { o.branch = "agent/issue-other"; }],
    ["key", (o: Record<string, unknown>) => { o.key = "issue-other"; }],
    ["run id", (o: Record<string, unknown>) => { o.runId = "run-other"; }],
  ] as const) {
    it(`a changed owner ${name} takes today's path`, async t => {
      const { r } = await armedHold(t);
      const record = r.owner();
      mutate(record);
      execFileSync("git", ["-C", r.bare, "config", "--local", r.ownerKey, JSON.stringify(record)], { env: GIT_ENV, stdio: "pipe" });
      await r.execute();
      notHeld(r, name);
      failedWith(r, PRESERVE);
      assert.equal(typedRejections(r).length, 0);
    });
  }

  it("a malformed owner record takes today's path", async t => {
    const { r } = await armedHold(t);
    execFileSync("git", ["-C", r.bare, "config", "--local", r.ownerKey, "{not json"], { env: GIT_ENV, stdio: "pipe" });
    await r.execute();
    notHeld(r, "malformed");
    failedWith(r, PRESERVE);
  });

  it("a duplicate owner record takes today's path", async t => {
    const { r } = await armedHold(t);
    execFileSync("git", ["-C", r.bare, "config", "--local", "--add", r.ownerKey, r.rawOwner()], { env: GIT_ENV, stdio: "pipe" });
    await r.execute();
    notHeld(r, "duplicate");
    failedWith(r, PRESERVE);
  });

  it("precedence: a duplicate owner record plus a stale claim fence keeps today's error", async t => {
    const { r } = await armedHold(t);
    execFileSync("git", ["-C", r.bare, "config", "--local", "--add", r.ownerKey, r.rawOwner()], { env: GIT_ENV, stdio: "pipe" });
    r.setJournal(j => { j.recovery!.claimGeneration = 9; });
    await r.execute();
    notHeld(r, "duplicate and stale");
    failedWith(r, PRESERVE);
    assert.equal(typedRejections(r).length, 0);
    assert.match(String((r.counts.rejections[0] as Error).message), /stale recovery claim fence/);
  });

  it("precedence: a duplicate owner record plus a changed source keeps today's error", async t => {
    const { r } = await armedHold(t);
    execFileSync("git", ["-C", r.bare, "config", "--local", "--add", r.ownerKey, r.rawOwner()], { env: GIT_ENV, stdio: "pipe" });
    const reserve = (git.reserveRecoveryIteration as unknown as (...a: unknown[]) => Promise<unknown>);
    t.mock.method(git, "reserveRecoveryIteration", async (...args: unknown[]) => {
      r.setJournal(j => { j.clonePath = j.clonePath + "-elsewhere"; });
      return reserve.apply(git, args);
    });
    await r.execute();
    notHeld(r, "duplicate and missing source");
    failedWith(r, PRESERVE);
    assert.equal(typedRejections(r).length, 0);
  });

  it("a matching identity reaches capture and holds nothing", async t => {
    const r = await build(t);
    await r.execute();
    notHeld(r, "matching");
    assert.ok(r.counts.captures > 0, "the claim reached capture");
    assert.equal(typedRejections(r).length, 0);
  });

  it("a pending owner record is not held", async t => {
    const { r } = await armedHold(t);
    const record = r.owner();
    record.state = "pending";
    execFileSync("git", ["-C", r.bare, "config", "--local", r.ownerKey, JSON.stringify(record)], { env: GIT_ENV, stdio: "pipe" });
    await r.execute();
    notHeld(r, "pending");
    failedWith(r, PRESERVE);
  });

  for (const field of ["attempts", "deadline", "startedAt", "episode"] as const) {
    it(`a journal whose ${field} no longer matches the owner's reservation is not held`, async t => {
      const { r } = await armedHold(t);
      r.setJournal(j => { j.recovery![field] = Number(j.recovery![field] ?? 0) + 1; });
      await r.execute();
      notHeld(r, field);
      failedWith(r, field === "deadline" || field === "startedAt" ? CORRUPT : PRESERVE);
    });
  }

  it("a changed source is not held", async t => {
    const { r } = await armedHold(t);
    const reserve = (git.reserveRecoveryIteration as unknown as (...a: unknown[]) => Promise<unknown>);
    t.mock.method(git, "reserveRecoveryIteration", async (...args: unknown[]) => {
      r.setJournal(j => { j.clonePath = j.clonePath + "-elsewhere"; });
      return reserve.apply(git, args);
    });
    await r.execute();
    notHeld(r, "source");
    failedWith(r, PRESERVE);
  });

  it("a stale claim fence is not held", async t => {
    const { r } = await armedHold(t);
    r.setJournal(j => { j.recovery!.claimGeneration = 9; });
    await r.execute();
    notHeld(r, "stale fence");
    failedWith(r, PRESERVE);
  });

  it("a later iteration of the same claim takes today's path", async t => {
    const r = await build(t);
    let calls = 0;
    const reserve = git.reserveRecoveryIteration.bind(git);
    t.mock.method(git, "reserveRecoveryIteration", async (...args: Parameters<typeof reserve>) => {
      if (++calls === 2) r.mismatch.on = true;
      return reserve(...args);
    });
    t.mock.method(git, "recordRecoveryCapture", async () => { throw new Error("injected capture failure"); });
    await r.execute();
    assert.ok(calls >= 2, "a second reservation happened");
    notHeld(r, "later iteration");
    failedWith(r, PRESERVE);
    assert.equal(adoptionParks(r).length, 0);
  });

  it("without the negotiated feature nothing is parked and the claim fails closed with custody kept", async t => {
    const { r, before } = await armedHold(t, { features: [] });
    await r.execute();
    notHeld(r, "no feature");
    failedWith(r, PRESERVE);
    assert.equal(adoptionParks(r).length, 0);
    assert.equal(fs.existsSync(r.clonePath), true);
    assert.equal(fs.readFileSync(path.join(r.clonePath, "retained.txt"), "utf8"), "local retained work\n");
    void before;
  });

  it("a first running report not acknowledged as running is not held", async t => {
    const { r } = await armedHold(t);
    api.stateAckWire = (body, run) => body.status === "running" ? { status: 200, body: { run: { ...run, status: "paused" } } } : undefined;
    await r.execute();
    api.stateAckWire = undefined;
    notHeld(r, "ACK status");
    failedWith(r, PRESERVE);
  });
});

describe("blocked journals keep today's disposition", () => {
  it("a renewable budget_exhausted journal: generic error at the reservation, then the #3580 exhausted rehold", async t => {
    const { r, ownerBefore, journalBefore } = await armedHold(t, { journal: "budget_exhausted", claim: { claim_generation: 3, worker_recovery_episode: 8, worker_recovery_owner_episode: 0 } });
    await r.execute();
    // Measured on the pre-fix baseline: the journal's config value is left unchanged by this path.
    assert.equal(JSON.stringify(r.journal()), journalBefore, "the journal is unchanged from before, as on base");
    assert.equal(r.counts.rejections.length, 1, "the reservation was reached");
    assert.equal(typedRejections(r).length, 0, "the generic error, not the typed one");
    assert.match(String((r.counts.rejections[0] as Error).message), /^recovery owner binding changed$/);
    assert.equal(r.counts.holds, 0);
    assert.equal(guidance(r).length, 0);
    assert.equal(adoptionParks(r).length, 0, "no adoption park");
    assert.equal(r.parks.filter(p => p.recovery_cause === "retained_recovery_exhausted").length, 1, JSON.stringify(r.parks));
    assert.ok(r.counts.blocks > 0, "the #3580 re-hold ran blockRecoveryEpisode");
    assert.equal(failures(r).length, 0);
    assert.deepEqual(r.sequence, BUDGET_BASE_SEQUENCE);
    assert.deepEqual(r.parks, BUDGET_BASE_PARKS);
    assert.equal(r.rawOwner(), ownerBefore, "the owner record is byte-identical to base's");
  });

  it("a renewable adoption_failed journal is unreachable at the reservation: today's adoption rehold, not the new hold", async t => {
    const { r, ownerBefore, journalBefore } = await armedHold(t, { journal: "adoption_failed", claim: { claim_generation: 3, worker_recovery_episode: 8, worker_recovery_owner_episode: 1 } });
    await r.execute();
    // Measured on the pre-fix baseline: journal and owner record are both left unchanged by this path.
    assert.equal(JSON.stringify(r.journal()), journalBefore, "the journal is unchanged from before, as on base");
    assert.equal(r.rawOwner(), ownerBefore, "the owner record is unchanged from before, as on base");
    // Base carries the generic cause only; the typed message exists only where the class does.
    if (RecoveryOwnerBareIdentityError) {
      const cause = String(logged(r, "retained adoption renewal evidence unavailable")[0]?.cause);
      assert.ok(cause.includes("recovery owner binding changed: bare storage identity"), cause);
    }
    assert.equal(r.counts.rejections.length, 0, "canRenewRecoveryAdoption refused before any reservation");
    assert.equal(r.counts.holds, 0);
    assert.equal(guidance(r).length, 0, "no bare-identity guidance line");
    assert.equal(r.flights.length, 0, "bareIdentityHold was never set");
    assert.ok(r.counts.blocks > 0, "blockRecoveryEpisode ran");
    assert.ok(r.counts.freeCaptures > 0, "the quiescence capture ran");
    assert.equal(adoptionParks(r).length, 1);
    assert.equal(failures(r).length, 0);
    assert.ok(logged(r, "retained adoption renewal evidence unavailable").length === 1);
    assert.deepEqual(r.sequence, ADOPTION_BASE_SEQUENCE);
    assert.deepEqual(r.parks, ADOPTION_BASE_PARKS);
  });

  it("the runner guard alone: a stubbed typed rejection on a renewable blocked budget_exhausted source takes today's fail() route", async t => {
    const r = await build(t, { journal: "budget_exhausted", claim: { claim_generation: 3, worker_recovery_episode: 8, worker_recovery_owner_episode: 0 } });
    // The hold subclass, so the runner's `stage !== "blocked"` guard is what refuses it (the base class would not match at all).
    assert.ok(HoldError, "git.ts exports RecoveryOwnerBareIdentityHoldError");
    assert.ok(RecoveryOwnerBareIdentityError, "git.ts exports RecoveryOwnerBareIdentityError");
    const Hold = HoldError as new (probed: Error) => Error;
    const Base = RecoveryOwnerBareIdentityError as new (...a: unknown[]) => Error;
    t.mock.method(git, "reserveRecoveryIteration", async () => {
      throw new Hold(new Base({ dev: "1", ino: "2" }, { dev: "1", ino: "3" }, r.owner()));
    });
    await r.execute();
    assert.equal(r.counts.holds, 0);
    assert.equal(guidance(r).length, 0);
    assert.equal(adoptionParks(r).length, 0);
    assert.equal(r.parks.filter(p => p.recovery_cause === "retained_recovery_exhausted").length, 1, JSON.stringify(r.parks));
  });
});

describe("a base-class bare-identity error from any other owner read inside the reservation is never held", () => {
  /** Marks the span of the reservation under test, so retained discovery's own earlier calls never trigger a flip. */
  const scope = (t: TestContext, r: Rig): { inReserve: boolean } => {
    const state = { inReserve: false };
    const reserve = git.reserveRecoveryIteration.bind(git);
    t.mock.method(git, "reserveRecoveryIteration", async (...args: Parameters<typeof reserve>) => {
      state.inReserve = true;
      try { return await reserve(...args); } finally { state.inReserve = false; }
    });
    void r;
    return state;
  };
  /** The predecessor's state before the claim: the run HOME (seeded here), the bare's refs, and the clone's HEAD and work tree. */
  const predecessor = (r: Rig) => {
    fs.mkdirSync(path.join(r.home, "sub"), { recursive: true });
    fs.writeFileSync(path.join(r.home, "sub", "transcript.jsonl"), "session bytes\n");
    // One rebuildable cache subtree (a RUN_CACHE_SUBTREES entry), to observe whether today's exit drops it.
    fs.mkdirSync(path.join(r.home, ".cache", "go-build"), { recursive: true });
    fs.writeFileSync(path.join(r.home, ".cache", "go-build", "entry"), "cache bytes\n");
    return { homeDigest: treeDigest(r.home), refs: command(r.bare, "for-each-ref"), head: command(r.clonePath, "rev-parse", "HEAD"),
      status: command(r.clonePath, "status", "--porcelain"), cloneDigest: workTree(r.clonePath) };
  };
  const assertTodaysDisposition = (r: Rig, before: ReturnType<typeof predecessor>): void => {
    // Behaviour first, so a baseline that holds fails on the hold and not on the missing class.
    assert.equal(r.counts.holds, 0, "not held");
    assert.equal(guidance(r).length, 0, "no storage-identity guidance line");
    assert.equal(r.flights.length, 0);
    assert.equal(adoptionParks(r).length, 0, "no retained_recovery_adoption_failed park");
    assert.ok(RecoveryOwnerBareIdentityError && HoldError, "git.ts exports both classes");
    assert.equal(r.counts.rejections.length, 1, "the reservation rejected once");
    const error = r.counts.rejections[0];
    assert.ok(error instanceof RecoveryOwnerBareIdentityError, "a base-class bare-identity error");
    assert.ok(!(error instanceof HoldError), "not the hold-authorizing subclass");
    assert.equal(r.parks.length, 0, JSON.stringify(r.parks));
    assert.equal(failures(r).length, 1, JSON.stringify(failures(r).map(f => f.body)));
    assert.equal(r.counts.models, 0);
    assert.equal(r.counts.captures, 0, "no capture");
    assert.equal(r.counts.freeCaptures, 0, "no quiescence capture");
    assert.equal(fs.existsSync(r.clonePath), true, "custody kept");
    assert.equal(fs.readFileSync(path.join(r.clonePath, "retained.txt"), "utf8"), "local retained work\n");
    // No teardown: the clone was not retired; the run HOME, the bare's refs and the clone are as before the claim.
    assert.equal(r.counts.retires, 0, "no clone retire");
    assert.equal(fs.existsSync(r.home), true, "the run HOME still exists");
    assert.equal(treeDigest(r.home), before.homeDigest, "the run HOME is unchanged");
    assert.equal(command(r.bare, "for-each-ref"), before.refs, "the bare's refs are byte-identical");
    assert.equal(command(r.clonePath, "rev-parse", "HEAD"), before.head, "the clone's HEAD is unchanged");
    assert.equal(command(r.clonePath, "status", "--porcelain"), before.status, "the clone's work tree status is unchanged");
    assert.equal(workTree(r.clonePath), before.cloneDigest, "the clone's work tree is unchanged");
  };

  it("the identity changes between the probe and the ordinary owner read: today's preservation_failed, nothing held", async t => {
    const r = await build(t);
    const ownerBefore = r.rawOwner();
    const pre = predecessor(r);
    const state = scope(t, r);
    const seam = git as any;
    const real = seam.checkedRecovery.bind(git);
    let first = true;
    t.mock.method(seam, "checkedRecovery", async (...args: unknown[]) => {
      const out = await real(...args);
      if (state.inReserve && first) { first = false; r.mismatch.on = true; }
      return out;
    });
    await r.execute();
    assert.equal(first, false, "the flip happened inside the reservation");
    assertTodaysDisposition(r, pre);
    assert.equal(r.rawOwner(), ownerBefore, "the owner record is untouched: the ordinary read refused before any write");
    FAIL_SHAPE(r);
  });

  for (const [name, needle] of [["pending charge", `"state":"pending"`], ["committed", `"state":"committed"`]] as const) {
    it(`the identity changes right after the ${name} write: the readback's base error takes today's preservation_failed route`, async t => {
      const r = await build(t);
      const ownerBefore = r.rawOwner();
      const pre = predecessor(r);
      const state = scope(t, r);
      const seam = git as unknown as { runGit(bare: string, args: string[], ...rest: unknown[]): Promise<string> };
      const real = seam.runGit.bind(git);
      let flipped = false;
      t.mock.method(seam, "runGit", async (bare: string, args: string[], ...rest: unknown[]) => {
        const out = await real(bare, args, ...rest);
        if (state.inReserve && !flipped && args[0] === "config" && args[2] === r.ownerKey && String(args[3]).includes(needle)) {
          flipped = true;
          r.mismatch.on = true;
        }
        return out;
      });
      await r.execute();
      assert.equal(flipped, true, "the flip followed the owner-record write");
      assertTodaysDisposition(r, pre);
      OWNER_SHAPE(r, name, ownerBefore);
      FAIL_SHAPE(r);
    });
  }
});

describe("park outcomes", () => {
  const unconfirmed = (r: Rig): void => {
    assert.equal(logged(r, "retained recovery bare identity change held").length, 0);
    assert.equal(guidance(r).filter(t => t.includes("was not confirmed")).length, 0);
    assert.equal(failures(r).length, 0, "nothing was failed");
    assert.equal(fs.existsSync(r.clonePath), true);
  };

  it("an exact replay ACK confirms the hold", async t => {
    const { r } = await armedHold(t);
    let n = 0;
    r.ackHook = async (body, original) => {
      await original();
      if (++n === 1) throw new Error("connection reset after the server applied the park");
      return { status: "recovery_wait", recoveryCause: body.recovery_cause };
    };
    await r.execute();
    assert.equal(r.parks.length, 2, "the lost report was replayed once");
    assert.equal(r.flights[0]!.parked, true);
    assert.equal(logged(r, "retained recovery bare identity change held").length, 1);
  });

  for (const [name, ack] of [
    ["a statusless ACK", async () => ({})],
    ["a lost ACK", async () => { throw new Error("connection reset"); }],
    ["an ACK naming another status", async (body: Record<string, unknown>) => ({ status: "claimed", recoveryCause: body.recovery_cause })],
  ] as const) {
    it(`${name} is unresolved: at most two reports, not parked, nothing failed, no held line`, async t => {
      const { r } = await armedHold(t);
      r.ackHook = ack as AckHook;
      await r.execute();
      assert.ok(r.parks.length <= 2, String(r.parks.length));
      assert.notEqual(r.flights[0]!.parked, true);
      assert.equal(logged(r, "retained recovery bare identity change hold unconfirmed").length, 1);
      unconfirmed(r);
      assert.equal(guidance(r).length, 1, "only the original guidance");
      assert.equal(feed(r).filter(t => t.includes("not confirmed")).length, 1, "a conditional correction follows");
    });
  }

  it("a definitive 400: not parked, corrected line, no further state report, the flight leaves the snapshot, no terminal journaled", async t => {
    const { r, before } = await armedHold(t);
    r.ackHook = async () => { throw new RequestError("POST", "/state", 400, "unsupported recovery_cause"); };
    const added = t.mock.method(r.registry, "add");
    await r.execute();
    assert.equal(added.mock.callCount(), 1);
    assert.equal(r.parks.length, 1, "no replay after a first-attempt 400");
    assert.notEqual(r.flights[0]!.parked, true);
    assert.equal(logged(r, "retained recovery bare identity change hold unconfirmed").length, 1);
    unconfirmed(r);
    assert.equal(feed(r).filter(t => t.includes("not confirmed")).length, 1);
    assert.deepEqual(r.sequence, ["running", "recovery_wait:retained_recovery_adoption_failed"], "no state report after the 400");
    assert.equal(r.registry.has(r.claim.run_id), false, "dropped from the active-run snapshot");
    assert.deepEqual(dumps(r.bare), before);
  });

  it("a stale claim reaches its own arm and is neither held nor failed", async t => {
    const { r } = await armedHold(t);
    let n = 0;
    r.ackHook = async (_body, original) => {
      if (++n > 1) return original();
      await original();
      api.failStateWhen(r.claim.run_id, b => b.status === "recovery_wait", { disposition: "stale_claim", runStatus: "queued" });
      throw new Error("connection reset after the server applied the park");
    };
    await r.execute();
    assert.notEqual(r.flights[0]!.parked, true);
    assert.equal(logged(r, "retained recovery bare identity change held").length, 0);
    assert.equal(logged(r, "retained recovery bare identity change hold unconfirmed").length, 0, "a stale claim is not an unresolved hold");
    assert.equal(failures(r).length, 0);
  });

  it("another claim generation owning the run refuses the hold", async t => {
    const { r } = await armedHold(t);
    r.ackHook = async () => { api.setOwnershipStatus(r.claim.run_id, "running", 99); throw new Error("connection reset"); };
    await r.execute();
    assert.notEqual(r.flights[0]!.parked, true);
    assert.equal(logged(r, "retained recovery bare identity change held").length, 0);
    assert.equal(failures(r).length, 0);
  });

  it("a lost ownership propagates without an unconfirmed-hold log or a corrected line", async t => {
    const { r } = await armedHold(t);
    r.ackHook = async () => { api.setOwnershipNotOwned(r.claim.run_id); throw new Error("connection reset"); };
    await r.execute();
    assert.equal(logged(r, "retained recovery bare identity change hold unconfirmed").length, 0);
    assert.equal(feed(r).filter(t => t.includes("not confirmed")).length, 0);
    assert.equal(failures(r).length, 0);
  });
});

describe("races during the guidance flush and the park", () => {
  it("a credential switch during the park reconciliation reaches its arm: not held, not failed, custody kept", async t => {
    const { r, before } = await armedHold(t);
    let tripped = false;
    r.ackHook = async (_body, original) => {
      if (!tripped) {
        tripped = true;
        api.requestCredentialSwitch(r.claim.run_id, r.claim.claim_generation!);
        const end = Date.now() + 10_000;
        while (r.flights[0]!.steering.pendingCredentialSwitch() !== r.claim.claim_generation && Date.now() < end) await new Promise(resolve => setTimeout(resolve, 5));
        assert.equal(r.flights[0]!.steering.pendingCredentialSwitch(), r.claim.claim_generation, "the switch tripped");
      }
      return { ...(await original()), recoveryCause: "retained_recovery_adoption_failed" };
    };
    await r.execute();
    assert.equal(tripped, true);
    assert.equal(logged(r, "retained recovery bare identity change held").length, 0);
    assert.equal(failures(r).length, 0);
    assert.equal(fs.existsSync(r.clonePath), true);
    assert.deepEqual(dumps(r.bare).config, before.config);
  });

  it("an owner Cancel during the guidance flush wins through its arm: no park, custody kept", async t => {
    const { r } = await armedHold(t);
    const real = client.postMessages.bind(client);
    const posted = t.mock.method(client, "postMessages", (async (...args: Parameters<typeof real>) => {
      const out = await real(...args);
      api.setInputs(r.claim.run_id, [{ id: 1, kind: "cancel" }]);
      const end = Date.now() + 10_000;
      const steering = r.flights[0]!.steering as unknown as { isCancelled(): boolean };
      while (!steering.isCancelled() && Date.now() < end) await new Promise(resolve => setTimeout(resolve, 5));
      assert.equal(steering.isCancelled(), true, "the cancel reached the flight");
      return out;
    }) as never);
    await r.execute();
    assert.ok(posted.mock.callCount() > 0, "the guidance flush ran");
    assert.equal(r.counts.holds, 1, "the hold ran exactly once");
    assert.equal(guidance(r).length, 1, "exactly one bare-identity guidance line was posted");
    assert.equal(adoptionParks(r).length, 0, "the cancel preempted the park");
    assert.equal(logged(r, "retained recovery bare identity change held").length, 0);
    assert.equal(fs.existsSync(r.clonePath), true);
    assert.equal(fs.readFileSync(path.join(r.clonePath, "retained.txt"), "utf8"), "local retained work\n");
  });
});

// The statuses a base run produces for the blocked fixtures (each fixture's sequence is identical on base 776af85a1, where the
// typed mismatch is the generic error at the same points); see the red/green evidence in the unit report.
const BUDGET_BASE_PARKS: Array<Record<string, unknown>> = [{ status: "recovery_wait", recovery_cause: "retained_recovery_exhausted", claim_generation: 3 }];
const ADOPTION_BASE_PARKS: Array<Record<string, unknown>> = [{ status: "recovery_wait", recovery_cause: "retained_recovery_adoption_failed", claim_generation: 3 }];
const BUDGET_BASE_SEQUENCE: string[] = ["running", "recovery_wait:retained_recovery_exhausted"];
const ADOPTION_BASE_SEQUENCE: string[] = ["running", "recovery_wait:retained_recovery_adoption_failed"];
