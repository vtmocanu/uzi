// Issue #3644 m3: the producer-to-consumer regression across REAL separate processes.
//
// The seed and route tests build the predecessor P by hand (a rig that writes its journal, ledger entry and slot). This file lets the
// PRODUCER write them: generation 1 is a child node process running the production RunRunner (its own default-incarnation authority,
// its own boundary record, real phaseClone, early pin and bindFlightSource), parked at the plan gate and SIGKILLed. A fresh child
// process S (a different incarnation, a later execution) then claims generation 2 of the same run over the same data root, and a third
// child S2 is a later restart. This parent hosts FakeApi and the fake forge and ONLY asserts: it constructs no RunRunner and injects no
// incarnation, attempt identity, journal, ledger entry or slot.
//
//   (a) Docker-wired: P chose an attempt clone; S restores from the verified pin and never reads P's newer unpinned commit.
//   (b) unwired: P chose the canonical clone; S parks the run for its owner and writes nothing.
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { afterEach, describe, it } from "node:test";
import { peekAuthoritySlot, type SlotSnapshot } from "../src/completion-authority.js";
import { parseRestoreValue } from "../src/restore-journal.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { FetchFn } from "../src/forge.js";
import { TOKEN, api, baseUrl, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness } from "./runner-harness.js";
import { AUTH_TOKEN, FEATURES as RIG_FEATURES, PARK_FEATURE, gitIn, host, makeProc, treeDigest } from "./restore-route-rig.js";
import { exitedWithin, startChild, startForgeBridge, waitFor, type Child, type ChildConfig, type LogLine } from "./restore-producer-rig.js";

const children: Child[] = [];
const closers: Array<() => Promise<void>> = [];
const tmpDirs: string[] = [];

installHarness({}, async () => {
  const live = children.splice(0);
  for (const child of live) if (child.proc.exitCode === null && child.proc.signalCode === null) child.proc.kill("SIGKILL");
  for (const child of live) await child.exited.catch(() => undefined);
  for (const close of closers.splice(0)) await close();
});
afterEach(() => { for (const dir of tmpDirs.splice(0)) fs.rmSync(dir, { recursive: true, force: true }); });

/** The route rig's client features plus the two a guarded, certifiably completing generation needs (custody inventory, terminal report). */
const FEATURES = [...RIG_FEATURES, "recovery_inventory_v1", "terminal_rejection_report"];
const wire = JSON.parse(fs.readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
const RECEIPT_REPO_ID: string = wire.ack.completed_publication_receipt.repo_id;

const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

interface World {
  iid: number;
  branch: string;
  claim: ClaimResponse;
  runId: string;
  dataDir: string;
  bare: string;
  journalKey: string;
  ledgerKey: string;
  forgeUrl: string;
  procs: { p: string; s: string };
  /** The config of one generation's child; `logName` keeps the logs of the three children apart. */
  config(role: ChildConfig["role"], generation: number, extra?: Partial<ChildConfig>): ChildConfig;
  spawn(role: ChildConfig["role"], generation: number, extra?: Partial<ChildConfig>): Child;
}

async function world(iid: number, features: string[]): Promise<World> {
  api.setRegisterProtocolFeatures(features);
  const tmp = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "restore-producer-")));
  tmpDirs.push(tmp);
  const dataDir = fs.realpathSync(fx.dataDir);
  const forge = fakeGitlab();
  const bridge = await startForgeBridge((forge.gitlab as unknown as { fetchFn: FetchFn }).fetchFn, head => { forge.pr.head = head; });
  closers.push(bridge.close);
  const claim = gitlabClaim(iid, { claim_generation: 1, inventory_guarded: true,
    repo: { id: RECEIPT_REPO_ID, url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, default_branch: "main", forge_type: "gitlab" } });
  // The same compose slot for every generation (same hostname and container id); P's execution is strictly older than S's.
  const procs = { p: path.join(tmp, "proc-p"), s: path.join(tmp, "proc-s") };
  await makeProc(procs.p, { ticks: "1000" });
  await makeProc(procs.s, { ticks: "2000" });
  const branch = `agent/issue-${iid}`;
  const config: World["config"] = (role, generation, extra = {}) => ({
    role, apiUrl: baseUrl,
    apiToken: TOKEN, joinToken: AUTH_TOKEN, dataDir, homeRoot: homeDir, procRoot: role === "P" ? procs.p : procs.s, hostname: host(),
    forgeUrl: bridge.url, claim: { ...claim, claim_generation: generation,
      // A later generation resumes the run's message sequence where the api last recorded it (the server's claim carries it).
      ...(role === "P" ? {} : { last_seq: Math.max(0, ...api.messages(claim.run_id).map(m => m.seq)) }) }, features,
    logPath: path.join(tmp, `${role}.jsonl`), ...extra,
  });
  return {
    iid, branch, claim, runId: claim.run_id, dataDir, bare: git.barePathFor(fx.originPath),
    journalKey: `uzi-recovery.${branch}.clone`, ledgerKey: `uzi-attempts.${branch}.entry`, forgeUrl: bridge.url, procs, config,
    spawn: (role, generation, extra) => {
      const cfg = config(role, generation, extra);
      const child = startChild(cfg, path.join(tmp, `${role}.config.json`), tmp);
      children.push(child);
      return child;
    },
  };
}

const cfgGet = (w: World, key: string): string | undefined => { try { return gitIn(w.bare, ["config", "--local", "--get", key]) || undefined; } catch { return undefined; } };
const cfgAll = (w: World, key: string): string[] => { try { return gitIn(w.bare, ["config", "--local", "--get-all", key]).split("\n").filter(Boolean); } catch { return []; } };
const configText = (w: World): string => fs.readFileSync(path.join(w.bare, "config"), "utf8");
const msgs = (lines: LogLine[], msg: string): LogLine[] => lines.filter(l => l.msg === msg);
const states = (w: World, status: string) => api.states.filter(s => s.runId === w.runId && s.body.status === status);

/** Generation 1: starts the producer, waits for it to park at the plan gate, kills it hard, and returns its log. */
async function produceAndKill(w: World, extra: Partial<ChildConfig> = {}): Promise<{ p: Child; incarnation: string }> {
  api.setOwnershipStatus(w.runId, "running", 1);
  const p = w.spawn("P", 1, extra);
  await waitFor("the producer to park at the plan gate", () => states(w, "awaiting_approval").length > 0, p);
  p.proc.kill("SIGKILL");
  const [, signal] = await p.exited;
  assert.equal(signal, "SIGKILL", "the producer was killed, not stopped");
  const identity = msgs(p.log(), "child:identity")[0]!;
  assert.equal(identity.boundaryOk, true);
  assert.equal(states(w, "failed").length, 0, "a killed producer reports nothing terminal");
  return { p, incarnation: String(identity.incarnation) };
}

interface PFacts {
  journal: string | undefined; ledger: string | undefined; slot: SlotSnapshot | null; clone: string; home: string; provision: string;
}
/** Everything the successors must leave byte-identical: P's journal key, ledger entry, authority slot, clone, HOME and provision dir. */
async function pFacts(w: World, pPath: string): Promise<PFacts> {
  return {
    journal: cfgGet(w, w.journalKey), ledger: cfgAll(w, w.ledgerKey)[0], slot: await peekAuthoritySlot({ dataDir: w.dataDir, workerToken: AUTH_TOKEN }, pPath),
    clone: treeDigest(pPath), home: treeDigest(path.join(homeDir, w.runId)), provision: treeDigest(path.join(w.dataDir, "provision", w.runId)),
  };
}

/** After the kill, before the successor: P's clone gains a commit that was never pinned, and a dirty file. Returns the new HEAD. */
function advanceP(pPath: string): string {
  fs.writeFileSync(path.join(pPath, "UNPINNED.txt"), "newer than the pin\n");
  gitIn(pPath, ["add", "UNPINNED.txt"]);
  gitIn(pPath, [...IDENT, "commit", "-m", "unpinned work"]);
  fs.writeFileSync(path.join(pPath, "DIRTY.txt"), "dirty bytes that only the predecessor's clone holds\n");
  return gitIn(pPath, ["rev-parse", "HEAD"]);
}

/** The pin records the producer wrote under `<dataDir>/recovery/<runId>/` (read-only; the file name is the capture id). */
function pinRecords(w: World): Array<{ captureId: string; raw: Record<string, unknown> }> {
  const dir = path.join(w.dataDir, "recovery", w.runId);
  if (!fs.existsSync(dir)) return [];
  return fs.readdirSync(dir).filter(n => n.endsWith(".json")).sort()
    .map(n => ({ captureId: n.slice(0, -".json".length), raw: JSON.parse(fs.readFileSync(path.join(dir, n), "utf8")) as Record<string, unknown> }));
}

/** Advances the bare's origin/main by one descendant commit (the pin stays an ancestor); returns the new tip. */
function advanceOriginMain(w: World): string {
  const ref = "refs/remotes/origin/main";
  const tip = gitIn(w.bare, ["rev-parse", ref]);
  const next = gitIn(w.bare, [...IDENT, "commit-tree", `${tip}^{tree}`, "-p", tip, "-m", "origin moved after the pin"]);
  gitIn(w.bare, ["update-ref", ref, next, tip]);
  return next;
}

const sha = (text: string): string => createHash("sha256").update(text).digest("hex");

describe("a producer-written predecessor, restored by a successor process (issue #3644 m3)", () => {
  it("(a) Docker-wired: the producer's attempt clone is restored from its verified pin, never from its newer unpinned commit, and S and a later restart leave P untouched", async () => {
    const w = await world(3644, FEATURES);
    const docker = { dockerHost: "tcp://docker:2375" };
    const { incarnation: pInc } = await produceAndKill(w, docker);

    // The PRODUCER chose attempt-path mode: an attempt id in its journal, an attempt clone path, a ledger entry of its own.
    const journal = JSON.parse(cfgGet(w, w.journalKey)!) as { runId: string; clonePath: string; attemptId?: string };
    assert.equal(journal.runId, w.runId);
    assert.match(String(journal.attemptId), /./, "the producer's journal carries an attempt id");
    assert.match(journal.clonePath, /\.attempt-/, "the producer's clone is an attempt clone");
    const pPath = journal.clonePath;
    assert.equal(cfgAll(w, w.ledgerKey).length, 1, "one ledger entry: the producer's own");
    const pinSha = gitIn(w.bare, ["rev-parse", "refs/remotes/origin/main"]);
    const slot = await peekAuthoritySlot({ dataDir: w.dataDir, workerToken: AUTH_TOKEN }, pPath);
    assert.equal(slot?.state, "active", "the killed producer's slot is still active under its own incarnation");

    const pins = pinRecords(w);
    assert.equal(pins.length, 1, "the producer wrote exactly one pin record");
    const pinCaptureId = pins[0]!.captureId;
    assert.equal(pins[0]!.raw.sourceSha, pinSha, "the producer's pin record names the default tip it saw");

    const pHead = advanceP(pPath);
    assert.notEqual(pHead, pinSha, "P holds a commit the pin does not");
    // origin/main moves on after the kill (the pin stays an ancestor): "seeded from P's pin" and "seeded from whatever origin/main is now" differ.
    const newTip = advanceOriginMain(w);
    assert.notEqual(newTip, pinSha);
    const before = await pFacts(w, pPath);

    // ---- S: a fresh process, a different incarnation, generation 2 ----
    api.setOwnershipStatus(w.runId, "running", 2);
    const s = w.spawn("S", 2, docker);
    const [sCode] = await exitedWithin(s, "S");
    const sLog = s.log();
    assert.equal(sCode, 0, `S exited cleanly: ${s.stderr()} ${JSON.stringify(msgs(sLog, "child:error"))}`);
    const sInc = String(msgs(sLog, "child:identity")[0]!.incarnation);
    assert.notEqual(sInc, pInc, "S is a different incarnation than the producer");

    const seeded = msgs(sLog, "independent restore seeded");
    assert.equal(seeded.length, 1, JSON.stringify(sLog.filter(l => /restore/i.test(l.msg))));
    const seededNote = seeded[0]!;
    assert.equal(seededNote.sourceSha, pinSha, "the pin the producer wrote names the default tip");
    assert.equal(seededNote.seededHead, pinSha, "S starts at the pin P recorded");
    assert.notEqual(seededNote.seededHead, newTip, "not at the origin tip that moved after the pin");
    assert.notEqual(seededNote.seededHead, pHead, "not at P's newer commit");
    assert.equal(seededNote.pinGeneration, 1, "the pin is the producer's generation 1");
    assert.equal(seededNote.captureId, pinCaptureId, "the pin is the capture the producer wrote");
    assert.equal(seededNote.binding, "on_default");
    assert.equal((seededNote.verdict as { predecessorIncarnation: string }).predecessorIncarnation, pInc, "the verdict names the producer's incarnation");
    assert.match(String(seededNote.attemptPath), /\.attempt-[0-9TZ]+-g2-/);
    assert.notEqual(seededNote.attemptPath, pPath, "a distinct attempt path");
    assert.equal(msgs(sLog, "independent restore refused; predecessor retained").length, 0);

    // S reached executor admission, in the seeded clone, with a per-attempt HOME.
    const built = msgs(sLog, "child:executor built");
    assert.deepEqual(built.map(b => b.generation), [null, 2], "the initial build, then exactly one successor build at generation 2");
    assert.equal(built[1]!.home, path.join(homeDir, `${w.runId}.g2`), "S's HOME is <runId>.g2");
    const admitted = msgs(sLog, "child:executor admitted");
    assert.equal(admitted.length, 1);
    assert.equal(admitted[0]!.worktree, seededNote.attemptPath);
    assert.equal(admitted[0]!.head, pinSha, "the executor saw the pin");
    assert.equal((admitted[0]!.tracked as string[]).includes("UNPINNED.txt"), false, "P's unpinned commit is not in S's tree");
    assert.deepEqual(admitted[0]!.untracked, [], "P's dirty bytes were never copied");

    // The run completed through S, and the owner's feed says what was restored.
    assert.equal(states(w, "completed").length, 1, "the run completes in the api");
    assert.equal(states(w, "failed").length, 0);
    const feed = api.messages(w.runId).map(m => JSON.stringify(m.payload)).filter(t => t.includes("continues from verified saved work at pin"));
    assert.equal(feed.length, 1, "the restore feed line is emitted once");
    assert.match(feed[0]!, /later unpinned edits may remain only in the retained predecessor on the worker \(not measured\)/);

    // The completion was certified: S's work was checkpointed to the origin and S's clone retired under the receipt, which turned the
    // restore entry into a tombstone. P's journal key, ledger entry and slot (asserted below) were never cleared by it.
    const worked = msgs(sLog, "child:executor worked")[0]!;
    assert.equal(gitIn(fx.originPath, ["rev-parse", `refs/uzi-checkpoints/${w.branch}`]), worked.checkpointHead, "the real checkpoint was delivered");
    assert.notEqual(worked.checkpointHead, worked.finalHead);
    assert.equal(fs.existsSync(String(seededNote.attemptPath)), false, "S's attempt clone was retired");
    const restore = parseRestoreValue(cfgGet(w, `uzi-restore.${w.branch}.journal`)!);
    assert.equal(restore.kind, "tombstone", "S's restore entry is a tombstone");
    if (restore.kind === "tombstone") {
      assert.equal(restore.basis.restoreTip, pinSha);
      assert.equal(restore.successor.clonePath, seededNote.attemptPath);
    }
    const ledgers = new Map(cfgAll(w, w.ledgerKey).map(l => JSON.parse(l) as { clonePath: string; state: string }).map(l => [l.clonePath, l.state]));
    assert.equal(ledgers.get(String(seededNote.attemptPath)), "retired", "S's last ledger value is retired");
    assert.equal(ledgers.get(pPath), "live", "P's ledger entry is never touched");

    // P: byte-identical after S.
    const afterS = await pFacts(w, pPath);
    assert.deepEqual(afterS, before, "P's journal key, ledger entry, slot, clone, HOME and provision directory are unchanged by S");
    assert.equal(gitIn(pPath, ["rev-parse", "HEAD"]), pHead);

    // ---- S2: a later restart (generation 3) ----
    const config = configText(w);
    const clones = fs.readdirSync(path.dirname(pPath)).sort();
    const markerBytes = sha(fs.readFileSync(path.join(w.dataDir, "retained-homes", `${w.runId}.json`), "utf8"));
    api.setOwnershipStatus(w.runId, "running", 3);
    const s2 = w.spawn("S2", 3, docker);
    const [s2Code] = await exitedWithin(s2, "S2");
    const s2Log = s2.log();
    assert.equal(s2Code, 0, `S2 exited cleanly: ${s2.stderr()} ${JSON.stringify(msgs(s2Log, "child:error"))}`);
    assert.notEqual(String(msgs(s2Log, "child:identity")[0]!.incarnation), sInc, "S2 is a fresh incarnation");
    assert.equal(msgs(s2Log, "restore entry present; claim stopped").length, 1, "the restart meets S's tombstone and stops");
    assert.equal(msgs(s2Log, "independent restore seeded").length, 0, "the restart seeds nothing");
    assert.equal(msgs(s2Log, "child:executor admitted").length, 0, "the restart admits no executor");
    assert.equal(states(w, "failed").length, 0);
    assert.deepEqual(await pFacts(w, pPath), before, "P is unchanged after the restart too");
    assert.equal(configText(w), config, "the bare's config is unchanged by the restart");
    assert.deepEqual(fs.readdirSync(path.dirname(pPath)).sort(), clones, "the restart created no clone");
    assert.equal(sha(fs.readFileSync(path.join(w.dataDir, "retained-homes", `${w.runId}.json`), "utf8")), markerBytes);
  });

  it("(b) unwired: the producer's canonical clone is not restorable, so S parks the run for its owner and writes nothing", async () => {
    const w = await world(3645, [...FEATURES, PARK_FEATURE]);
    const { incarnation: pInc } = await produceAndKill(w);

    // The PRODUCER chose canonical mode: exactly {clonePath, runId} and the canonical path (no attempt suffix).
    const journal = JSON.parse(cfgGet(w, w.journalKey)!) as Record<string, unknown>;
    assert.deepEqual(Object.keys(journal).sort(), ["clonePath", "runId"], "the producer's journal is the canonical shape");
    const pPath = String(journal.clonePath);
    assert.equal(pPath, git.runnerClonePath(w.bare, `issue-${w.iid}`), "the canonical clone path");
    assert.equal(pPath.includes(".attempt-"), false);
    assert.equal(cfgAll(w, w.ledgerKey).length, 0, "an attempt-less source has no ledger entry");

    const pSlot = await peekAuthoritySlot({ dataDir: w.dataDir, workerToken: AUTH_TOKEN }, pPath);
    assert.equal(pSlot?.state, "active", "the killed producer's slot exists and is active, so 'unchanged' is not null-vs-null");
    advanceP(pPath);
    const before = await pFacts(w, pPath);
    const config = configText(w);

    api.setOwnershipStatus(w.runId, "running", 2);
    const s = w.spawn("S", 2, { echoRecoveryCause: true });
    const [sCode] = await exitedWithin(s, "S");
    const sLog = s.log();
    assert.equal(sCode, 0, `S exited cleanly: ${s.stderr()} ${JSON.stringify(msgs(sLog, "child:error"))}`);
    assert.notEqual(String(msgs(sLog, "child:identity")[0]!.incarnation), pInc);

    const parks = states(w, "recovery_wait");
    assert.equal(parks.length, 1, "exactly one owner-hold park");
    assert.equal(parks[0]!.body.recovery_cause, "retained_recovery_adoption_failed");
    assert.equal(states(w, "failed").length, 0, "no failed report");
    assert.equal(states(w, "completed").length, 0);
    assert.equal(msgs(sLog, "independent restore seeded").length, 0, "no seed");
    assert.equal(msgs(sLog, "child:executor built").length, 1, "no successor executor was built");
    assert.equal(msgs(sLog, "child:executor admitted").length, 0, "no executor was admitted");
    assert.equal(msgs(sLog, "independent restore refused; predecessor retained").length, 1, JSON.stringify(sLog.filter(l => /restore/i.test(l.msg))));
    assert.deepEqual(await pFacts(w, pPath), before, "P's key, slot, clone, HOME and provision directory are unchanged");
    assert.equal(configText(w), config, "nothing was written to the bare's config");
    assert.equal(fs.existsSync(path.join(w.dataDir, "retained-homes", `${w.runId}.json`)), false, "no retained-home marker");
  });
});
