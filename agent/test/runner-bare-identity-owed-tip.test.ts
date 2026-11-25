// Issue #4627 (U3, B owed-tip variants): what a held bare-identity claim exit does with the predecessor's owed tracking head.
//
// A held claim must not write to the predecessor's bare unless that is the one additive retention of E1: when an exact owed pin and
// its records already retain the tip nothing is written and retainCurrentOwedCandidate is never called; when only an episode ref
// reaches the tip, today's retainCurrentOwedCandidate adds exactly one owed pin (plus its candidate record, the owed-path marker at
// the cap, or the refusal evidence above it). A retention ref already at a WRONG target is never moved. Each probe variant runs for the
// Claude and the Codex executor (the wrong-target rows run on Claude only) with a confirmed and with an uncertain (statusless) owner hold, through the production
// RunRunner.execute() on real Git. The probe rows compare every outcome with the BASE outcome of the same state under today's
// unconditional retention (the write-free probe forced to "unretained"). The wrong-target rows are not base comparisons: their
// base already includes the additive-only retention writers, so their discriminating checks are the row-specific ones below.
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { afterEach, describe, it, type TestContext } from "node:test";
import type { PositiveOwedCandidateContext } from "../src/git.js";
import { api, git, installHarness, setUpHarness, tearDownHarness } from "./runner-harness.js";
import { fixtureFetchTracking, fixtureTrackingOptions } from "./runner-tracking-fixture.js";
import {
  CLAIM_GENERATION, PREDECESSOR_GENERATION, alive, buildHold, cleanupHold, commitOnBranch, gitIn, procfs, type Ack, type Harness, type HoldFlight,
  type HoldRig, type Snapshot,
} from "./runner-bare-identity-hold-fixture.js";

installHarness();
afterEach(() => cleanupHold());

const CAP = 2;
const owedRef = (r: HoldRig, sha: string): string => `refs/uzi-owed/${r.runId}/${sha}`;
const episodeRef = (r: HoldRig): string => `refs/uzi-recovery-episode/${r.runId}/1`;
const refusedRef = (r: HoldRig, sha: string): string => `refs/uzi-incoming/${r.runId}/${PREDECESSOR_GENERATION}/refused-${sha}`;
const lines = (s: string): string[] => s === "" ? [] : s.split("\n");
const names = (dump: string): Record<string, string> => Object.fromEntries(JSON.parse(dump) as Array<[string, string]>);
interface Diff { added: string[]; changed: string[]; removed: string[] }
function diffNames(before: string, after: string): Diff {
  const b = names(before), a = names(after);
  return { added: Object.keys(a).filter(n => !(n in b)).sort(), removed: Object.keys(b).filter(n => !(n in a)).sort(),
    changed: Object.keys(a).filter(n => n in b && a[n] !== b[n]).sort() };
}
function refDiff(before: string, after: string): Diff {
  const b = new Set(lines(before)), a = new Set(lines(after));
  return { added: [...a].filter(l => !b.has(l)).sort(), removed: [...b].filter(l => !a.has(l)).sort(), changed: [] };
}
const none: Diff = { added: [], changed: [], removed: [] };

/** A head owned by the predecessor's run: its tracking ref, owner stamp, receipt and context, owed pin and candidate record. */
async function trackedHead(r: HoldRig, parents?: string[]): Promise<string> {
  const h = commitOnBranch(r, "H", parents);
  await fixtureFetchTracking(git, r.bare, r.clonePath, r.branch, r.runId, PREDECESSOR_GENERATION);
  assert.equal(gitIn(r.bare, "rev-parse", owedRef(r, h)), h);
  return h;
}
const candidateFiles = (r: HoldRig, sha: string): string[] => fs.readdirSync(r.owedDir()).filter(n => n.startsWith(`candidate-${r.runId}-${sha}-`));
/** The predecessor's pin of `sha` is gone and only an episode ref still reaches it. */
function onlyEpisodeRef(r: HoldRig, sha: string): void {
  gitIn(r.bare, "update-ref", "-d", owedRef(r, sha));
  for (const n of candidateFiles(r, sha)) fs.unlinkSync(path.join(r.owedDir(), n));
  gitIn(r.bare, "update-ref", episodeRef(r), sha);
}
/** A tracking head produced by an owner with only an owner stamp (no receipt, no context): "historical". */
function historicalHead(r: HoldRig, owner: string): string {
  const h = commitOnBranch(r, "H");
  gitIn(r.bare, "-c", "protocol.file.allow=user", "fetch", "--no-tags", "--no-write-fetch-head", `file://${r.clonePath}`,
    `refs/heads/${r.branch}:refs/uzi-runner/${r.branch}`);
  gitIn(r.bare, "config", "--local", `uzi-trackowner.${r.branch}.owner`, owner);
  return h;
}
async function admit(r: HoldRig): Promise<void> {
  const opts = await fixtureTrackingOptions(git, r.bare, r.branch, r.runId, PREDECESSOR_GENERATION);
  assert.equal(await git.admitRefusalProvenance(opts.context as PositiveOwedCandidateContext), "written");
}
const lowerCap = (cap: number): void => { (git as unknown as { maxOwedCandidates: number }).maxOwedCandidates = cap; };

interface Data { tip: string; other?: string; y?: string; warnings: string[] }
interface Variant {
  name: string;
  harnesses?: Harness[];
  guarded?: boolean;
  setup(r: HoldRig, d: Data, t: TestContext): Promise<void>;
  /** Armed immediately before the claim executes. */
  arm?(r: HoldRig, d: Data, t: TestContext): void;
  /** What the probe answers: the kind (and reason) of the first result, or "unretained" then the result of retainCurrentOwedCandidate. */
  probe: "updated" | "unretained" | "not_updated";
  /** The result of the one retainCurrentOwedCandidate call, when it is made. */
  retain?: { kind: "updated" | "not_updated"; reason?: string };
  refs: (r: HoldRig, d: Data) => Diff;
  owed: (r: HoldRig, d: Data) => Diff | "candidate" | "candidate+context";
  refusals: (r: HoldRig, d: Data) => Diff;
  /** The outcome is "updated", i.e. sourceOwedJoined is true. */
  joined: boolean;
  extra?(r: HoldRig, d: Data, after: Snapshot): void;
}

const warnCapture = (d: Data): void => {
  const internals = git as unknown as { log: { warn: (message: string, fields?: unknown) => void } };
  const warn = internals.log.warn.bind(internals.log);
  internals.log = { ...internals.log, warn: (message: string, fields?: unknown) => { d.warnings.push(message + " " + JSON.stringify(fields)); warn(message, fields); } };
};
const failClosed = (t: TestContext, op: string): void => {
  const seam = git as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  const real = seam.assertBareClear!.bind(git);
  t.mock.method(seam, "assertBareClear", async (...args: unknown[]) => {
    if (args[1] === op) throw new Error("the authority refuses the bare");
    return real(...args);
  });
};

const VARIANTS: Variant[] = [
  { name: "(retained) an exact owed pin and its records already retain the tip",
    async setup(r, d) { d.tip = await trackedHead(r); },
    probe: "updated", refs: () => none, owed: () => none, refusals: () => none, joined: true },
  { name: "(E1-a, receipt owner) only an episode ref reaches the tip: one additive owed pin and its candidate record",
    async setup(r, d) { d.tip = await trackedHead(r); onlyEpisodeRef(r, d.tip); },
    probe: "unretained", retain: { kind: "updated" },
    refs: (r, d) => ({ ...none, added: [`${owedRef(r, d.tip)} ${d.tip}`] }), owed: () => "candidate", refusals: () => none, joined: true,
    extra(r, d) { gitIn(r.bare, "merge-base", "--is-ancestor", d.tip, owedRef(r, d.tip)); } },
  { name: "(E1-a, historical owner) only an episode ref reaches the tip: the owed pin, its candidate record and its context file",
    async setup(r, d) { d.tip = historicalHead(r, r.runId); gitIn(r.bare, "update-ref", episodeRef(r), d.tip); },
    probe: "unretained", retain: { kind: "updated" },
    refs: (r, d) => ({ ...none, added: [`${owedRef(r, d.tip)} ${d.tip}`] }), owed: () => "candidate+context", refusals: () => none, joined: true,
    extra(r, d) { gitIn(r.bare, "merge-base", "--is-ancestor", d.tip, owedRef(r, d.tip)); } },
  { name: "(step-6 fallback) the exact pin exists but the owed context file is missing: today's refusal, nothing written",
    async setup(r, d) {
      d.tip = await trackedHead(r);
      for (const n of fs.readdirSync(r.owedDir()).filter(x => x.startsWith("context-"))) fs.unlinkSync(path.join(r.owedDir(), n));
    },
    probe: "not_updated", refs: () => none, owed: () => none, refusals: () => none, joined: false },
  { name: "(step-6 fallback) the exact pin exists but the authority refuses the bare-clear check: today's code runs and refuses",
    async setup(r, d) { d.tip = await trackedHead(r); },
    arm(_r, _d, t) { failClosed(t, "owed-pin"); },
    probe: "unretained", retain: { kind: "not_updated", reason: "preservation_failed" },
    refs: () => none, owed: () => none, refusals: () => none, joined: false },
  { name: "(E1-b) the pin fills the owed cap: the pin, its candidate record and the owed-path marker",
    async setup(r, d) {
      d.other = await trackedHead(r);
      d.tip = await trackedHead(r, [d.other]);
      onlyEpisodeRef(r, d.tip);
    },
    arm() { lowerCap(CAP); },
    probe: "unretained", retain: { kind: "updated" },
    refs: (r, d) => ({ ...none, added: [`${owedRef(r, d.tip)} ${d.tip}`] }), owed: () => "candidate",
    refusals: r => ({ ...none, added: [`owedpath-${r.runId}-${PREDECESSOR_GENERATION}.json`] }), joined: true },
  { name: "(E1-c) already at the owed cap: no pin, one refused-head ref with its ledger row and the owed-path marker",
    async setup(r, d) {
      d.other = await trackedHead(r);
      const second = await trackedHead(r, [d.other]);
      d.tip = await trackedHead(r, [second]);
      onlyEpisodeRef(r, d.tip);
      await admit(r);
    },
    arm() { lowerCap(CAP); },
    probe: "unretained", retain: { kind: "not_updated", reason: "owed_limit" },
    refs: (r, d) => ({ ...none, added: [`${refusedRef(r, d.tip)} ${d.tip}`] }), owed: () => none,
    refusals: r => ({ ...none, added: [`owedpath-${r.runId}-${PREDECESSOR_GENERATION}.json`, `refusals-${r.runId}.json`] }), joined: false,
    extra(r, d) { gitIn(r.bare, "cat-file", "-e", `${d.tip}^{commit}`); } },
  { name: "(not owned) the tracking ref belongs to another run: refs unchanged, not_updated",
    async setup(r, d) { d.tip = historicalHead(r, "foreign-run"); },
    probe: "not_updated", refs: () => none, owed: () => none, refusals: () => none, joined: false },
  { name: "(sealed) the generation is sealed: refs unchanged, not_updated",
    async setup(r, d) {
      d.tip = await trackedHead(r);
      fs.mkdirSync(r.refusalDir(), { recursive: true, mode: 0o700 });
      fs.writeFileSync(path.join(r.refusalDir(), `final-${r.runId}-${PREDECESSOR_GENERATION}.json`), "{}\n", { mode: 0o600 });
    },
    probe: "not_updated", refs: () => none, owed: () => none, refusals: () => none, joined: false },
  { name: "(covered) the tip is already covered remotely: refs unchanged, updated, no pin",
    async setup(r, d) { d.tip = await trackedHead(r); onlyEpisodeRef(r, d.tip); r.claim.checkpoint_tip = d.tip; },
    probe: "updated", refs: () => none, owed: () => none, refusals: () => none, joined: true },
];

/** The wrong-target rows run on Claude only: mechanism 6 is harness-independent. */
const WRONG_TARGET: Variant[] = [
  { name: "(wrong-target owed ref) the owed ref already points at another commit: never moved, nothing written, custody kept",
    harnesses: ["claude"], guarded: true,
    async setup(r, d) {
      d.tip = await trackedHead(r);
      d.y = gitIn(r.bare, "rev-parse", "refs/remotes/origin/main");
      gitIn(r.bare, "update-ref", "-d", owedRef(r, d.tip));
      for (const n of candidateFiles(r, d.tip)) fs.unlinkSync(path.join(r.owedDir(), n));
      gitIn(r.bare, "update-ref", owedRef(r, d.tip), d.y);
      await preAdmit(r); // keeps today's admission write out of the diff; it is pinned by the admission describe at the end of this file
    },
    arm(_r, d) { warnCapture(d); },
    probe: "unretained", retain: { kind: "not_updated", reason: "preservation_failed" },
    refs: () => none, owed: () => none, refusals: () => none, joined: false,
    extra(r, d) {
      assert.equal(gitIn(r.bare, "rev-parse", owedRef(r, d.tip)), d.y, "the wrong-target ref still points at Y");
      assert.equal(candidateFiles(r, d.tip).length, 0, "no candidate record was written");
      assert.ok(d.warnings.some(w => /retention ref target mismatch; ref left untouched/.test(w) && w.includes(owedRef(r, d.tip))), JSON.stringify(d.warnings));
    } },
  { name: "(wrong-target refusal ref) at the cap the refused-head ref already points at another commit: never moved, fail-closed latch and incomplete marker, custody kept",
    harnesses: ["claude"], guarded: true,
    async setup(r, d) {
      d.other = await trackedHead(r);
      const second = await trackedHead(r, [d.other]);
      d.tip = await trackedHead(r, [second]);
      onlyEpisodeRef(r, d.tip);
      await admit(r);
      d.y = gitIn(r.bare, "rev-parse", "refs/remotes/origin/main");
      gitIn(r.bare, "update-ref", refusedRef(r, d.tip), d.y);
      await preAdmit(r); // keeps today's admission write out of the diff; it is pinned by the admission describe at the end of this file
    },
    arm(_r, d) { lowerCap(CAP); warnCapture(d); },
    probe: "unretained", retain: { kind: "not_updated", reason: "owed_limit" },
    refs: () => none, owed: () => none,
    refusals: r => ({ ...none, added: [`incomplete-${r.runId}.json`, `owedpath-${r.runId}-${PREDECESSOR_GENERATION}.json`, `refusals-${r.runId}.json`] }), joined: false,
    extra(r, d) {
      assert.equal(gitIn(r.bare, "rev-parse", refusedRef(r, d.tip)), d.y, "the wrong-target refusal ref still points at Y");
      assert.equal((git as unknown as { refusalRecordingFailed: Set<string> }).refusalRecordingFailed.has(r.runId), true, "the fail-closed latch is set");
      assert.ok(d.warnings.some(w => /retention ref target mismatch; ref left untouched/.test(w) && w.includes(refusedRef(r, d.tip))), JSON.stringify(d.warnings));
    } },
];

/** A guarded flight admits its generation's refusal provenance when the exit first asks for the owed options (today's order, before
 *  any pin); doing it here keeps that one admission out of the retention diff the rows assert. */
async function preAdmit(r: HoldRig): Promise<void> {
  const opts = await fixtureTrackingOptions(git, r.bare, r.branch, r.runId, r.claim.claim_generation!);
  await git.admitRefusalProvenance(opts.context as PositiveOwedCandidateContext);
}

interface Outcome {
  r: HoldRig; d: Data; before: Snapshot; after: Snapshot; flight: HoldFlight;
  probes: Array<{ kind: string; reason?: string }>; retains: Array<{ kind: string; reason?: string }>; retainCalls: number;
  /** What the retention finally returned, wherever it came from. */
  final: { kind: string; reason?: string };
}
const parse = (s: string[]): Array<{ kind: string; reason?: string }> => s.map(x => JSON.parse(x) as { kind: string; reason?: string });

async function execute(t: TestContext, v: Variant, target: Target, ack: Ack, mode: "real" | "unretained"): Promise<Outcome> {
  const d: Data = { tip: "", warnings: [] };
  const r = await buildHold(t, { harness: target.harness, ack, closeCodexFacade: target.closed, ...(v.guarded ? { guarded: true } : {}), probeMode: mode,
    tracked: async rig => { await v.setup(rig, d, t); },
    beforeExecute: rig => { v.arm?.(rig, d, t); } });
  const before = await r.snapshot();
  await r.executeClaim();
  const after = await r.snapshot();
  assert.equal(r.counts.holds, 1, "the hold ran");
  const flight = r.flights[0]!;
  const probes = parse(r.counts.probeResults), retains = parse(r.counts.retainResults);
  const final = probes[0] && probes[0].kind !== "unretained" ? probes[0] : retains[0] ?? { kind: "none" };
  return { r, d, before, after, flight, probes, retains, retainCalls: r.counts.retainCalls, final };
}

/** What every owed-tip variant must hold: nothing of the predecessor but the variant's own additive retention moved. */
function assertUntouched(o: Outcome): void {
  const { r, before, after } = o;
  assert.equal(alive(r.child!), true, "the predecessor-attributed child is alive");
  assert.equal(r.counts.reaps, 0, "no reap");
  assert.equal(r.counts.reclaims, 0, "no cache reclaim");
  assert.equal(r.counts.models, 0);
  assert.equal(r.counts.ledger, 0, "no attempt ledger append");
  assert.equal(api.states.filter(s => s.runId === r.runId && s.body.status === "failed").length, 0, "no failed report");
  assert.equal(after.journal, before.journal, "the journal is byte-identical");
  assert.equal(after.owner, before.owner, "the owner record is byte-identical");
  assert.equal(after.attemptMode, before.attemptMode, "the attempt mode is byte-identical");
  assert.equal(after.attempts, before.attempts, "uzi-attempts.* is byte-identical");
  assert.equal(after.config, before.config, "the bare's whole config is byte-identical");
  assert.equal(after.clone, before.clone);
  assert.equal(after.home, before.home);
  assert.equal(after.caches, before.caches);
  assert.equal(after.provision, before.provision);
  assert.equal(after.skills, before.skills);
  assert.deepEqual(after.slot, before.slot);
  assert.deepEqual(after.bareSlot, before.bareSlot);
  assert.equal(o.flight.bareIdentityHold, true);
  assert.equal(o.flight.preserveRecoveryClone, true);
  assert.equal(o.flight.retainedLocalCustody, true);
  assert.equal(o.flight.preserveSession, true);
  assert.equal(fs.readFileSync(path.join(r.clonePath, "retained.txt"), "utf8"), "local retained work\n");
}

const outcomes = new Map<string, Outcome>();
const keyOf = (v: Variant, target: Target, ack: Ack): string => `${v.name}|${target.label}|${ack}`;
const ACKS: Ack[] = ["confirmed", "statusless"];
interface Target { label: string; harness: Harness; closed: boolean; /** The exit never reaches the owed retention. */ skipped: boolean }
const TARGETS: Target[] = [
  { label: "claude", harness: "claude", closed: false, skipped: false },
  // A fresh Codex facade cannot prove its terminal disposal before any boundary ran (its registry is open), so the exit
  // keeps the clone and never reaches the owed retention, today and with the hold alike.
  { label: "codex", harness: "codex", closed: false, skipped: true },
  // The same exit with the facade closed by one earlier boundary, so the disposal proves and the retention runs: the only way to
  // exercise the probe under the Codex harness. An arrangement, not a state a first-iteration hold can reach.
  { label: "codex (facade closed by an earlier boundary)", harness: "codex", closed: true, skipped: false },
];

/** The BASE outcome of a variant when its `base:` sibling did not run first (a single test selected by name): run it in a scene
 *  of its own, keep only its plain recorded values, then tear that scene down and set up a fresh one for the hold under test.
 *  Nothing of the first scene (its filesystem, its mocks, its planted child) is read again. */
async function freshBase(t: TestContext, v: Variant, target: Target, ack: Ack): Promise<Outcome> {
  const o = await baseRun(t, v, target, ack);
  t.mock.restoreAll();
  cleanupHold();
  await tearDownHarness();
  await setUpHarness(t);
  return o;
}

async function baseRun(t: TestContext, v: Variant, target: Target, ack: Ack): Promise<Outcome> {
  const o = await execute(t, v, target, ack, "unretained");
  assert.equal(o.retainCalls, target.skipped ? 0 : 1, target.skipped ? "an unproven Codex disposal skips the retention" : "today's retention ran once");
  return o;
}

/** Run ids and commit ids differ between two fixtures of the same variant; compare their shapes. */
function norm(o: Outcome, text: string): string {
  let out = text.split(o.r.runId).join("<RUN>");
  for (const [name, sha] of [["TIP", o.d.tip], ["OTHER", o.d.other], ["Y", o.d.y]] as const) if (sha) out = out.split(sha).join(`<${name}>`);
  return out.replace(/context-[0-9a-f]{64}/g, "context-<HASH>").replace(/-[0-9a-f]{64}\.json/g, "-<HASH>.json");
}
const shape = (o: Outcome, d: Diff): string => norm(o, JSON.stringify(d));

for (const v of [...VARIANTS, ...WRONG_TARGET]) {
  for (const target of TARGETS.filter(x => v.harnesses === undefined || v.harnesses.includes(x.harness) && !x.closed)) {
    for (const ack of ACKS) {
      describe(`${v.name}: ${target.label}, ${ack === "confirmed" ? "confirmed" : "uncertain"} park`, procfs, () => {
        // The skipped Codex arrangement never reaches the retention, so one base run per ack (on the variant that would write the
        // most) proves today's unconditional call is skipped there too; the other rows' hold runs assert it directly.
        const baseRuns = !target.skipped || v === VARIANTS[1];
        if (baseRuns) it("base: today's unconditional retention yields the outcome the hold is compared with", async t => {
          const o = await baseRun(t, v, target, ack);
          outcomes.set(keyOf(v, target, ack), o);
        });

        it("hold: the exit leaves the predecessor's refs and files as the variant requires", async t => {
          const base = outcomes.get(keyOf(v, target, ack)) ?? await freshBase(t, v, target, ack);
          const o = await execute(t, v, target, ack, "real");
          assertUntouched(o);
          assert.equal(gitIn(o.r.bare, "cat-file", "-t", o.d.tip), "commit", "the tip is still reachable");
          gitIn(o.r.bare, "cat-file", "-e", `${o.d.tip}^{commit}`);
          if (target.skipped) {
            // The retention is not reached: nothing is probed, called or written, exactly as under today's unconditional call.
            assert.deepEqual(o.probes, [], "no probe");
            assert.equal(o.retainCalls, 0, "no retention call");
            assert.equal(o.flight.sourceJoinFailed, true, "the unproven disposal keeps the source un-joined");
            assert.ok(o.r.logs.lines.some(l => (l as { msg: string }).msg === "codex terminal disposal unproven; retaining runner clone"));
            assert.deepEqual(refDiff(o.before.refs, o.after.refs), none, "refs");
            assert.equal(o.after.owed, o.before.owed, "uzi-owed files");
            assert.equal(o.after.refusals, o.before.refusals, "refusal files");
            assert.equal(base.retainCalls, 0, "today's unconditional call is skipped too");
            assert.equal(base.flight.sourceJoinFailed, true);
            return;
          }
          // The probe's answer, then the retention call the exit made (if any).
          assert.equal(o.probes.length, 1, JSON.stringify(o.probes));
          assert.equal(o.probes[0]!.kind, v.probe);
          if (v.probe === "updated") assert.equal(o.retainCalls, 0, "retainCurrentOwedCandidate was never called");
          if (v.probe === "not_updated") assert.equal(o.retainCalls, 0, "a final probe outcome is not retried");
          if (v.probe === "unretained") {
            assert.equal(o.retainCalls, 1, "only an unretained tip falls back to today's retention");
            assert.equal(o.retains[0]!.kind, v.retain!.kind);
            if (v.retain!.reason) assert.equal(o.retains[0]!.reason, v.retain!.reason);
          }
          // sourceOwedJoined: updated -> true; any other kind sets sourceJoinFailed.
          assert.equal(o.final.kind === "updated", v.joined);
          if (!v.joined) assert.equal(o.flight.sourceJoinFailed, true, "sourceJoinFailed is set when the retention did not update");
          if (v.joined && target.harness === "claude" && ack === "confirmed") {
            assert.equal(o.flight.sourceJoinFailed === true, false, "sourceOwedJoined: nothing set sourceJoinFailed");
            assert.equal(o.flight.sourceCleanupJoined, true, "the confirmed Claude park joined its source cleanup");
          }
          if (!v.joined) assert.equal(o.flight.sourceCleanupJoined === true, false);
          // The exact ref and file diffs.
          assert.deepEqual(refDiff(o.before.refs, o.after.refs), v.refs(o.r, o.d), "refs");
          const owed = v.owed(o.r, o.d);
          const owedDiff = diffNames(o.before.owed, o.after.owed);
          assert.deepEqual(owedDiff.changed, [], "an existing owed file changed");
          assert.deepEqual(owedDiff.removed, [], "an owed file was removed");
          if (typeof owed === "string") {
            const candidate = owedDiff.added.filter(n => n.startsWith(`candidate-${o.r.runId}-${o.d.tip}-`));
            assert.equal(candidate.length, 1, JSON.stringify(owedDiff));
            assert.equal(owedDiff.added.filter(n => n.startsWith("context-")).length, owed === "candidate+context" ? 1 : 0, JSON.stringify(owedDiff));
            assert.equal(owedDiff.added.length, owed === "candidate+context" ? 2 : 1, JSON.stringify(owedDiff));
          } else assert.deepEqual(owedDiff, owed, "uzi-owed files");
          assert.deepEqual(diffNames(o.before.refusals, o.after.refusals), v.refusals(o.r, o.d), "refusal files");
          v.extra?.(o.r, o.d, o.after);
          // The same state under today's unconditional retention ends the same way: only the spy count differs on a retained tip.
          assert.deepEqual({ kind: o.final.kind, reason: o.final.reason }, { kind: base.final.kind, reason: base.final.reason }, "the outcome equals the base outcome");
          assert.equal(o.flight.sourceJoinFailed === true, base.flight.sourceJoinFailed === true, "sourceJoinFailed equals the base");
          assert.equal(o.flight.sourceCleanupJoined === true, base.flight.sourceCleanupJoined === true, "sourceCleanupJoined equals the base");
          assert.equal(shape(o, refDiff(o.before.refs, o.after.refs)), shape(base, refDiff(base.before.refs, base.after.refs)), "the ref diff equals the base's");
          assert.equal(shape(o, diffNames(o.before.owed, o.after.owed)), shape(base, diffNames(base.before.owed, base.after.owed)), "the uzi-owed diff equals the base's");
          assert.equal(shape(o, diffNames(o.before.refusals, o.after.refusals)), shape(base, diffNames(base.before.refusals, base.after.refusals)), "the refusal diff equals the base's");
        });
      });
    }
  }
}

// Disclosure of one write that is today's behaviour on ANY guarded exit, base included: the exit's teardown asks for the owed options,
// which admits the generation's refusal provenance (info/uzi-refusals/admission-<run>-<generation>.json). The rows above that must
// assert a retention diff pre-admit in setup (preAdmit) to keep this one file out of it; this test pins the write itself.
function infoTree(bare: string): Record<string, string> {
  const out: Record<string, string> = {};
  const walk = (dir: string): void => {
    for (const n of fs.readdirSync(dir).sort()) {
      const full = path.join(dir, n);
      if (fs.statSync(full).isDirectory()) walk(full);
      else out[path.relative(bare, full)] = fs.readFileSync(full, "utf8");
    }
  };
  const info = path.join(bare, "info");
  if (fs.existsSync(info)) walk(info);
  return out;
}

describe("the guarded hold exit's refusal-provenance admission write (today's behaviour)", procfs, () => {
  async function run(t: TestContext, guarded: boolean, mode: "real" | "unretained") {
    const r = await buildHold(t, { harness: "claude", ack: "confirmed", ...(guarded ? { guarded: true } : {}), probeMode: mode,
      tracked: async rig => { await trackedHead(rig); } });
    const before = await r.snapshot();
    const infoBefore = infoTree(r.bare);
    await r.executeClaim();
    const after = await r.snapshot();
    const infoAfter = infoTree(r.bare);
    assert.equal(r.counts.holds, 1, "the hold ran");
    const added = Object.keys(infoAfter).filter(n => !(n in infoBefore)).map(n => n.split(r.runId).join("<RUN>"));
    assert.deepEqual(Object.keys(infoBefore).filter(n => infoAfter[n] !== infoBefore[n]), [], "no existing info file changed");
    assert.equal(r.counts.reclaims, 0, "no cache reclaim");
    assert.equal(after.refs, before.refs, "refs unchanged");
    assert.equal(after.config, before.config, "config unchanged");
    assert.equal(after.journal, before.journal, "journal unchanged");
    assert.equal(after.owner, before.owner, "owner record unchanged");
    assert.equal(after.attempts, before.attempts, "attempt ledger unchanged");
    assert.equal(after.owed, before.owed, "uzi-owed unchanged");
    return added;
  }
  const ADMISSION = `info/uzi-refusals/admission-<RUN>-${CLAIM_GENERATION}.json`;

  it("a guarded exit adds exactly the one admission file, as the base exit does", async t => {
    assert.deepEqual(await run(t, true, "real"), [ADMISSION]);
  });
  it("base: today's unconditional retention writes the same single admission file", async t => {
    assert.deepEqual(await run(t, true, "unretained"), [ADMISSION]);
  });
  it("an unguarded claim writes nothing under info/", async t => {
    assert.deepEqual(await run(t, false, "real"), []);
  });
});
