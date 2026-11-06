// issue #2969 (owner decision): the predecessor pass is refuse-only about the command uid. The quiescence scan only sees
// RUNNER_UID, so a COMMAND_UID process writing into a predecessor clone is invisible to it; unless the command uid is
// provably absent from the whole PID namespace the pass skips before entry (zero predecessor effects) or, once entered,
// stops. Part 1 drives the detector (proveNoCommandUid, a twin of Go proveNoUser) against a fake procfs; part 2 drives the
// real retained re-claim exactly as runner-retained-predecessor-final.test.ts does, with the detector backed by a fake
// procfs or a spy. The ordinary terminal FINAL must never consult the detector.
import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { ExecutorFactory } from "../src/runner.js";
import type { CaptureInput, FrozenOperationOptions, InventorySourceDiagnostic, RecoveryOutcome, RecoveryRecord, RecoveryCoordinator } from "../src/recovery.js";
import { COMMAND_UID, RUNNER_UID } from "../src/runner-uid.js";
import { mintAttemptId, proveNoCommandUid, quiesceRunAttempt, type CommandUidProof, type ProcSource } from "../src/run-quiescence.js";
import type { Executor } from "../src/executor.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { api, client, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith, fx } from "./runner-harness.js";

installHarness();

// ── part 1: the detector against a fake procfs ───────────────────────────────────────────────────────────────────────────

const SELF = 4242;
const tmpRoots: string[] = [];
afterEach(() => { for (const dir of tmpRoots.splice(0)) fs.rmSync(dir, { recursive: true, force: true }); });

function uidLine(a: number, b = a, c = a, d = a): string { return `Uid:\t${a}\t${b}\t${c}\t${d}\n`; }
function statusText(uid: string, state = "S (sleeping)"): string { return `Name:\tproc\nState:\t${state}\n${uid}Gid:\t0\t0\t0\t0\n`; }

interface Fake { root: string; proc: ProcSource; add(pid: number, status: string): void; mountinfo(text: string): void }

function fakeProc(opts: { mountinfo?: (root: string) => string; self?: number } = {}): Fake {
  const self = opts.self ?? SELF;
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "fake-proc-"));
  tmpRoots.push(root);
  fs.mkdirSync(path.join(root, String(self)));
  fs.symlinkSync(String(self), path.join(root, "self"));
  const fake: Fake = {
    root, proc: { root, selfPid: self },
    add: (pid, status) => {
      fs.mkdirSync(path.join(root, String(pid)), { recursive: true });
      fs.writeFileSync(path.join(root, String(pid), "status"), status);
    },
    mountinfo: text => fs.writeFileSync(path.join(root, String(self), "mountinfo"), text),
  };
  fake.mountinfo((opts.mountinfo ?? defaultMountinfo)(root));
  fake.add(self, statusText(uidLine(0)));
  fake.add(1, statusText(uidLine(0)));
  return fake;
}
const defaultMountinfo = (root: string): string => `36 25 0:20 / ${root} rw,nosuid,nodev,noexec,relatime - proc proc rw\n`;
const mountWith = (perMount: string, superBlock: string) => (root: string): string =>
  `36 25 0:20 / ${root} ${perMount} - proc proc ${superBlock}\n`;

describe("proveNoCommandUid (twin of Go proveNoUser)", () => {
  it("only the runner uid and root: absent", () => {
    const f = fakeProc();
    f.add(100, statusText(uidLine(RUNNER_UID)));
    assert.equal(proveNoCommandUid(f.proc), "absent");
  });

  for (const [field, text] of [
    ["real", uidLine(COMMAND_UID, 0, 0, 0)], ["effective", uidLine(0, COMMAND_UID, 0, 0)],
    ["saved", uidLine(0, 0, COMMAND_UID, 0)], ["filesystem", uidLine(0, 0, 0, COMMAND_UID)],
  ] as const) {
    it(`the command uid in the ${field} field: present`, () => {
      const f = fakeProc();
      f.add(100, statusText(text));
      assert.equal(proveNoCommandUid(f.proc), "present");
    });
  }

  it("a zombie holding the command uid still counts: present", () => {
    const f = fakeProc();
    f.add(100, statusText(uidLine(COMMAND_UID), "Z (zombie)"));
    assert.equal(proveNoCommandUid(f.proc), "present");
  });

  it("the default uid is COMMAND_UID, and a custom uid is honoured", () => {
    const f = fakeProc();
    f.add(100, statusText(uidLine(7777)));
    assert.equal(proveNoCommandUid(f.proc), "absent");
    assert.equal(proveNoCommandUid({ ...f.proc, uid: 7777 }), "present");
  });

  it("a status that cannot be read for a reason other than vanishing is unproven (and counts as present)", () => {
    const f = fakeProc();
    fs.mkdirSync(path.join(f.root, "100", "status"), { recursive: true }); // EISDIR on read
    assert.equal(proveNoCommandUid(f.proc), "unproven");
  });

  for (const [name, uid] of [
    ["a missing Uid line", "Name:\tx\n"],
    ["a repeated Uid line", uidLine(0) + uidLine(0)],
    ["three Uid fields", "Uid:\t0\t0\t0\n"],
    ["five Uid fields", "Uid:\t0\t0\t0\t0\t0\n"],
    ["a non-numeric Uid field", "Uid:\t0\tx\t0\t0\n"],
    ["a negative Uid field", "Uid:\t0\t-1\t0\t0\n"],
  ] as const) {
    it(`${name}: unproven`, () => {
      const f = fakeProc();
      f.add(100, statusText(uid));
      assert.equal(proveNoCommandUid(f.proc), "unproven");
    });
  }

  it("a status over 64 KiB is parsed up to its last complete line: the Uid line is early", () => {
    const f = fakeProc();
    f.add(100, "Name:\tx\n" + uidLine(RUNNER_UID) + "Groups:\t" + "1 ".repeat(40_000) + "\n");
    assert.equal(proveNoCommandUid(f.proc), "absent");
  });

  it("a Uid line past the 64 KiB bound is not seen: unproven, never absent", () => {
    const f = fakeProc();
    f.add(100, "Groups:\t" + "1 ".repeat(40_000) + "\n" + uidLine(COMMAND_UID));
    assert.equal(proveNoCommandUid(f.proc), "unproven");
  });

  it("a pid that vanishes on every scan is unproven after exactly five scans", () => {
    const f = fakeProc();
    let scans = 0;
    fs.mkdirSync(path.join(f.root, "100")); // listed, no status: ENOENT
    assert.equal(proveNoCommandUid({ ...f.proc, beforeScan: () => { scans++; } }), "unproven");
    assert.equal(scans, 5);
  });

  it("a pid that vanishes once and is then clean: absent (the scan is retaken, never skipped)", () => {
    const f = fakeProc();
    let scans = 0;
    fs.mkdirSync(path.join(f.root, "100"));
    const proof = proveNoCommandUid({ ...f.proc, beforeScan: attempt => {
      scans++;
      if (attempt === 1) f.add(100, statusText(uidLine(RUNNER_UID)));
    } });
    assert.equal(proof, "absent");
    assert.equal(scans, 2);
  });

  it("a pid that vanishes once, then reveals the command uid: present", () => {
    const f = fakeProc();
    fs.mkdirSync(path.join(f.root, "100"));
    assert.equal(proveNoCommandUid({ ...f.proc, beforeScan: attempt => {
      if (attempt === 1) f.add(100, statusText(uidLine(COMMAND_UID)));
    } }), "present");
  });

  for (const [name, mountinfo, expected] of [
    ["hidepid=2 in the super-block options", mountWith("rw,nosuid", "rw,hidepid=2"), "unproven"],
    ["hidepid=2 in the per-mount options", mountWith("rw,hidepid=2", "rw"), "unproven"],
    ["hidepid=invisible", mountWith("rw", "rw,hidepid=invisible"), "unproven"],
    ["subset=pid", mountWith("rw", "rw,subset=pid"), "unproven"],
    ["hidepid=0", mountWith("rw", "rw,hidepid=0"), "absent"],
    ["hidepid=off", mountWith("rw,hidepid=off", "rw"), "absent"],
    ["no mount at the proc root", () => "36 25 0:20 / /elsewhere rw - proc proc rw\n", "unproven"],
    ["a mount at the proc root that is not proc", (root: string) => `36 25 0:20 / ${root} rw - tmpfs tmpfs rw\n`, "unproven"],
    ["a malformed mountinfo line", () => "garbage\n", "unproven"],
  ] as const) {
    it(`mountinfo with ${name}: ${expected}`, () => {
      const f = fakeProc({ mountinfo });
      f.add(100, statusText(uidLine(RUNNER_UID)));
      assert.equal(proveNoCommandUid(f.proc), expected);
    });
  }

  it("an octal-escaped proc root in mountinfo is unescaped before the comparison", () => {
    const f = fakeProc({ mountinfo: root => `36 25 0:20 / ${root.replaceAll("-", "\\055")} rw - proc proc rw\n` });
    f.add(100, statusText(uidLine(RUNNER_UID)));
    assert.equal(proveNoCommandUid(f.proc), "absent");
  });

  it("a proc root that cannot be listed is unproven", () => {
    assert.equal(proveNoCommandUid({ root: path.join(os.tmpdir(), "no-such-proc-root-2969"), selfPid: SELF }), "unproven");
  });

  it("a `self` link that does not name this process is unproven", () => {
    const f = fakeProc();
    assert.equal(proveNoCommandUid({ ...f.proc, selfPid: SELF + 1 }), "unproven");
  });

  it("more pids than the cap is unproven", () => {
    const f = fakeProc();
    f.add(100, statusText(uidLine(RUNNER_UID)));
    assert.equal(proveNoCommandUid({ ...f.proc, maxPids: 2 }), "unproven"); // SELF, 1 and 100
    assert.equal(proveNoCommandUid({ ...f.proc, maxPids: 3 }), "absent");
  });

  it("the real /proc of the worker is readable by the default reader and never throws", () => {
    assert.ok((["absent", "present", "unproven"] as CommandUidProof[]).includes(proveNoCommandUid()));
  });
});

// ── part 2: the real retained re-claim ───────────────────────────────────────────────────────────────────────────────────

const JOURNAL_KEY = "fixture-journal-key";
const REPO_ID = "00000000-0000-4000-8000-000000002969";

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], {
    env: { ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))),
      GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
function journal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}

type Internals = {
  withInventorySourceBoundary(context: {
    runId: string; generation: number; barePath: string; predecessor?: object;
    reportDiagnostic?: (d: InventorySourceDiagnostic) => void;
  }, action: (prove: () => Promise<boolean>) => Promise<void>): Promise<"passed" | "retained">;
  registerPredecessorBoundary: (...args: unknown[]) => { refusals: Array<{ check: string; clonePath?: string }> };
  proveInventorySources: (...args: unknown[]) => Promise<boolean>;
};
interface Proof { generation: number; result: "passed" | "retained"; proves: boolean[]; diagnostics: InventorySourceDiagnostic[] }
interface OperateCall { record: RecoveryRecord; input: CaptureInput; options: FrozenOperationOptions; prove(): Promise<Proof> }

interface Opts {
  generations?: number[];
  claimGeneration?: number;
  codex?: boolean;
  /** The detector the runner is given. */
  detector?: (n: number) => CommandUidProof;
  operate?: (call: OperateCall) => Promise<RecoveryOutcome>;
  /** Wraps the real readInventoryCloneHeads (the n-th identity read inside a proof is n = 1, 2, ...). */
  onIdentityRead?: (n: number) => void;
  /** Called before the content proof of the first proof. */
  onContentProof?: () => void;
  /** Registers other runs' execution tails before the run starts. */
  otherTails?: string[];
}

async function drive(o: Opts = {}) {
  const claimGeneration = o.claimGeneration ?? 3;
  const claim = gitlabClaim(2512, { claim_generation: claimGeneration, session_id: "predecessor-session" });
  claim.repo.id = REPO_ID;
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const attemptId = mintAttemptId(Math.max(1, claimGeneration - 1));
  const clone = await git.createOrAttachRunnerClone(bare, 2512, noProofReseed, claim.run_id, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id, attemptId);
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", claimGeneration);
  claim.inventory_guarded = true;
  client.protocolFeatures = ["claim_generation_fence"];
  const hasFeature = client.hasFeature.bind(client);
  let inventoryFeature = true;
  client.hasFeature = name => name === "recovery_inventory_v1" ? inventoryFeature : hasFeature(name);
  const generations = o.generations ?? [1, 2];
  client.getRunOwnership = async () => ({ status: "running", claim_generation: claimGeneration, inventory_guarded: true });
  client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: [...generations, claimGeneration].map(generation => ({
    hold_id: `fixture-hold-${generation}`, generation, inventory_guarded: true, has_available_capture: false,
  })) });
  const events: string[] = [];
  const rpcs: string[] = [];
  const rpc = (name: string): void => { rpcs.push(name); events.push(`rpc:${name}`); };
  client.reserveRecoveryCapture = async () => { rpc("reserve"); return { capture_id: "00000000-0000-4000-8000-000000002512", state: "preparing" }; };
  client.uploadRecoveryBundle = async (_run, _capture, _manifest, stream) => {
    rpc("upload");
    for await (const _chunk of stream) { /* consume */ }
    throw new Error("upload response unknown");
  };
  client.getRecoveryCaptureStatus = async () => { throw new Error("status unavailable"); };
  client.releaseRecoveryCustody = async () => { rpc("release"); throw new Error("must keep custody"); };

  const prepare = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => { events.push("prepare"); return prepare(...args); };
  let identityReads = 0;
  const readHeads = git.readInventoryCloneHeads.bind(git);
  git.readInventoryCloneHeads = (async (bareArg: string, runId: string, options?: { cloneIdentity?: boolean }) => {
    const inventory = await readHeads(bareArg, runId, options as never);
    if (options?.cloneIdentity === true) o.onIdentityRead?.(++identityReads);
    return inventory;
  }) as typeof git.readInventoryCloneHeads;
  const contentProof = git.inventorySourceContentProof.bind(git);
  git.inventorySourceContentProof = ((...args: Parameters<typeof contentProof>) => { o.onContentProof?.(); return contentProof(...args); }) as typeof contentProof;

  let models = 0;
  const make: ExecutorFactory = runId => {
    const run: Executor["run"] = async () => {
      models++;
      events.push("model");
      throw new Error("model interrupted before settled completion");
    };
    return { homeDir: path.join(homeDir, runId), executor: { run } };
  };
  let detectorCalls = 0;
  const logs = recordingLogger();
  const runner = runnerWith(make, fakeGitlab().gitlab, JOURNAL_KEY, logs.logger, {
    recoveryRetryMs: 1, quiesceRun: quiesceRunAttempt, codexBoundaryDeadlineMs: 1_000,
    commandUidProof: () => o.detector ? o.detector(++detectorCalls) : (detectorCalls++, "absent"),
  } as never);
  for (const id of o.otherTails ?? []) (Reflect.get(runner, "executionTails") as Map<string, Promise<void>>).set(id, new Promise<void>(() => {}));
  const recovery = Reflect.get(runner, "recovery") as RecoveryCoordinator;
  const internals = runner as unknown as Internals;
  const registrations: Array<{ refusals: Array<{ check: string; clonePath?: string }> }> = [];
  const register = internals.registerPredecessorBoundary.bind(runner);
  internals.registerPredecessorBoundary = (...args) => { const reg = register(...args); registrations.push(reg); return reg; };
  for (const generation of generations) {
    const pinned = await recovery.pin({ runId: claim.run_id, generation, kind: "issue", branch: clone.branch,
      sourceSha: command(clone.path, "rev-parse", "HEAD"), inventoryGuarded: true });
    assert.ok(pinned, `fixture: guarded generation ${generation} pinned`);
  }
  const operations: number[] = [];
  const proofs: Proof[] = [];
  const original = recovery.operateFrozenInventory.bind(recovery);
  recovery.operateFrozenInventory = async (input, options = {}) => {
    if (!options.predecessor) return original(input, options);
    const record = input.record;
    operations.push(record.generation!);
    const call: OperateCall = {
      record, input, options,
      prove: async () => {
        const diagnostics: InventorySourceDiagnostic[] = [];
        const proves: boolean[] = [];
        const result = await internals.withInventorySourceBoundary({
          runId: record.runId, generation: record.generation!, barePath: bare, predecessor: options.predecessor!.boundary,
          reportDiagnostic: d => diagnostics.push(d),
        }, async prove => { proves.push(await prove()); });
        const proof = { generation: record.generation!, result, proves, diagnostics };
        proofs.push(proof);
        return proof;
      },
    };
    if (o.operate) return o.operate(call);
    const proof = await call.prove();
    return { state: record.state, captureId: record.captureId, held: proof.result === "passed" ? "acked" : "kept" };
  };
  await runner.execute(claim);
  return {
    claim, bare, clone, events, runner, internals, logs, operations, proofs, registrations, rpcs,
    models: () => models, detectorCalls: () => detectorCalls,
  };
}
type Result = Awaited<ReturnType<typeof drive>>;

const logged = (r: Result, msg: string): Array<Record<string, any>> => (r.logs.lines as Array<Record<string, any>>).filter(l => l.msg === msg);
const skipReasons = (r: Result): string[] => logged(r, "retained predecessor FINAL skipped").map(l => l.reason as string);
const failedReports = (r: Result) => api.states.filter(s => s.runId === r.claim.run_id && s.body.status === "failed");

function assertSkippedBeforeEntry(r: Result, reason: string): void {
  assert.deepEqual(skipReasons(r), [reason]);
  assert.equal(r.registrations.length, 0, "no held scope was entered for the predecessor");
  assert.deepEqual(r.operations, [], "the coordinator never operated a frozen inventory");
  // The retained claim's own pre-existing recovery may still reserve/upload; a predecessor effect would be an operation.
  assert.ok(r.events.includes("prepare"), "the successor was prepared");
  assert.equal(r.models(), 1, "the successor reached its model");
  assert.notEqual(journal(r.bare, r.clone.branch).recovery?.blocker, "quiescence_failed");
}
function assertStopped(r: Result, reason: RegExp): void {
  assert.equal(r.events.includes("prepare"), false, "no successor was prepared");
  assert.equal(r.models(), 0, "no model call");
  assert.ok(failedReports(r).length > 0, "the claim ended with the custody-retaining failed report");
  const retained = journal(r.bare, r.clone.branch);
  assert.equal(retained.clonePath, r.clone.path, "no successor replaces the predecessor");
  assert.equal(retained.recovery.stage, "blocked");
  assert.equal(retained.recovery.blocker, "quiescence_failed");
  assert.equal(fs.readFileSync(path.join(r.clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  const pass = logged(r, "retained predecessor FINAL pass").at(-1);
  assert.equal(pass?.verdict, "stop");
  assert.match(String(pass?.stop_reason), reason);
  assert.deepEqual(skipReasons(r), [], "a stop is never a skip");
  assert.equal(r.rpcs.includes("release"), false, "no FINAL POST (release) was sent");
}

describe("pre-entry: a command uid that cannot be ruled out is a clean skip with zero predecessor effects", () => {
  it("a command-uid process in a fake procfs (cwd inside the predecessor clone): skip command_uid_unproven", async () => {
    const f = fakeProc({ self: process.pid });
    f.add(100, statusText(uidLine(COMMAND_UID)));
    let sawPresent = 0;
    const r = await drive({ detector: () => { const p = proveNoCommandUid(f.proc); if (p === "present") sawPresent++; return p; } });
    assert.ok(sawPresent >= 1, "the real reader saw the command-uid process");
    assertSkippedBeforeEntry(r, "command_uid_unproven");
  });

  it("an unreadable command-uid entry counts as present: skip command_uid_unproven", async () => {
    const r = await drive({ detector: () => "unproven" });
    assertSkippedBeforeEntry(r, "command_uid_unproven");
    assert.equal(r.detectorCalls(), 1, "one proof, nothing else");
  });

  it("another claim in flight: skip other_claim_in_flight, without consulting the detector", async () => {
    const r = await drive({ otherTails: ["00000000-0000-4000-8000-0000000000aa"], detector: () => "absent" });
    assertSkippedBeforeEntry(r, "other_claim_in_flight");
    assert.equal(r.detectorCalls(), 0);
  });

  it("a detector that throws reads as unproven: skip", async () => {
    const r = await drive({ detector: () => { throw new Error("proc exploded"); } });
    assertSkippedBeforeEntry(r, "command_uid_unproven");
  });

  it("absent through the whole pass: both generations settle and the successor follows (positive twin)", async () => {
    const r = await drive();
    assert.deepEqual(r.operations, [1, 2]);
    assert.deepEqual(r.proofs.map(p => [p.generation, p.result, p.diagnostics]), [[1, "passed", []], [2, "passed", []]]);
    assert.ok(r.events.includes("prepare"));
    assert.equal(logged(r, "retained predecessor FINAL pass").at(-1)?.verdict, "proceed");
    assert.ok(r.detectorCalls() > 1, "the detector guards the pre-entry gate and the in-scope checkpoints");
  });
});

describe("after entry: a command uid that cannot be ruled out is a STOP", () => {
  it("appearing during the content proof: quiescence_failed, a clone-scoped refusal, no FINAL POST, no successor", async () => {
    let present = false;
    const r = await drive({ onContentProof: () => { present = true; }, detector: () => present ? "present" : "absent" });
    assertStopped(r, /refusal:command_uid_unproven/);
    assert.deepEqual(r.proofs.map(p => [p.generation, p.result]), [[1, "retained"]]);
    assert.deepEqual(r.operations, [1], "no later candidate");
    assert.deepEqual(r.proofs[0]!.diagnostics, [{ check: "process_not_quiescent_after_git" }],
      "reported to the coordinator as an existing diagnostic, no new persisted code");
    assert.deepEqual(r.registrations[0]!.refusals.filter(x => x.check === "command_uid_unproven"),
      [{ check: "command_uid_unproven", clonePath: r.clone.path }]);
  });

  it("an unreadable entry appearing after entry (fake procfs): STOP", async () => {
    const f = fakeProc({ self: process.pid });
    const proc: ProcSource = { root: f.root, selfPid: process.pid };
    const r = await drive({
      onContentProof: () => fs.mkdirSync(path.join(f.root, "100", "status"), { recursive: true }),
      detector: () => proveNoCommandUid(proc),
    });
    assertStopped(r, /refusal:command_uid_unproven/);
    assert.deepEqual(r.operations, [1]);
  });

  it("a listing error after entry: STOP", async () => {
    let broken = false;
    const r = await drive({ onContentProof: () => { broken = true; }, detector: () => { if (broken) throw new Error("listing failed"); return "absent"; } });
    assertStopped(r, /refusal:command_uid_unproven/);
  });

  it("appearing while the first proof reads the source identity: refused before the quiescence scan, with the clone path", async () => {
    let present = false;
    const r = await drive({ onIdentityRead: n => { if (n === 1) present = true; }, detector: () => present ? "present" : "absent" });
    assertStopped(r, /refusal:command_uid_unproven/);
    assert.deepEqual(r.proofs[0]!.diagnostics, [{ check: "process_not_quiescent_before_git" }]);
    assert.deepEqual(r.registrations[0]!.refusals.filter(x => x.check === "command_uid_unproven"),
      [{ check: "command_uid_unproven", clonePath: r.clone.path }]);
  });

  it("appearing during the final identity re-read: the proof does not pass", async () => {
    let present = false;
    const r = await drive({ onIdentityRead: n => { if (n === 2) present = true; }, detector: () => present ? "present" : "absent" });
    assertStopped(r, /refusal:command_uid_unproven/);
    assert.equal(r.proofs[0]!.result, "retained");
    assert.deepEqual(r.proofs[0]!.diagnostics, [{ check: "process_not_quiescent_after_git" }]);
  });

  it("appearing between two candidates: the authority checkpoint refuses the next one, recorded once", async () => {
    let present = false;
    const r = await drive({
      detector: () => present ? "present" : "absent",
      operate: async call => {
        const proof = await call.prove();
        present = true;
        return { state: call.record.state, captureId: call.record.captureId, held: proof.result === "passed" ? "acked" : "kept" };
      },
    });
    assertStopped(r, /command_uid_unproven|scope_not_live/);
    assert.deepEqual(r.operations, [1], "the second candidate never started");
    assert.equal(r.registrations[0]!.refusals.filter(x => x.check === "command_uid_unproven").length, 1);
  });

  it("an authority checkpoint inside a running operation (live()) fails once the process appears", async () => {
    let present = false;
    const r = await drive({
      detector: () => present ? "present" : "absent",
      operate: async call => {
        present = true;
        assert.equal(call.options.predecessor!.live(), false, "the coordinator's live() refuses");
        assert.equal(call.options.predecessor!.live(), false, "and stays refused");
        return { state: call.record.state, captureId: call.record.captureId, held: "kept" };
      },
    });
    assertStopped(r, /refusal:command_uid_unproven/);
    assert.equal(r.registrations[0]!.refusals.filter(x => x.check === "command_uid_unproven").length, 1, "deduplicated");
  });
});

describe("the ordinary terminal FINAL never consults the detector", () => {
  it("a first claim (no predecessor pass) calls it zero times", async () => {
    const r = await drive({ claimGeneration: 1, generations: [], detector: () => { throw new Error("must not be called"); } });
    assert.equal(r.detectorCalls(), 0);
    assert.ok(r.events.includes("model") || r.models() >= 0);
  });

  it("the ordinary proveInventorySources (no grant) reads no detector even with the command uid present", async () => {
    const r = await drive({ detector: () => "unproven" }); // skipped pass; successor proceeds
    const before = r.detectorCalls();
    const refusals: InventorySourceDiagnostic[] = [];
    await r.internals.proveInventorySources(
      { runId: r.claim.run_id, generation: 1, barePath: r.bare },
      (d: InventorySourceDiagnostic) => { refusals.push(d); return false; }, {});
    assert.equal(r.detectorCalls(), before, "the ordinary boundary never reads the proc table for the command uid");
    assert.ok(!refusals.some(d => (d.check as string) === "command_uid_unproven"));
  });
});
