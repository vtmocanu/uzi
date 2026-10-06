import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { deflateSync } from "node:zlib";

const fsp = fs.promises;
import { GitCache, RecoveryBundleTooLargeError } from "../src/git.js";
import {
  archiveFailureReasonSuffix,
  archiveQuarantinedSource,
  type QuarantineArchiveGit,
  type QuarantineArchiveResult,
} from "../src/quarantine-archive.js";
import { ResidueQuarantinedError, RunResidueBlockedError, assertResidueQuarantineOpen, latchResidueQuarantine, residueQuarantine } from "../src/residue-quarantine.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import type { RunRunner } from "../src/runner.js";
import type { ExecutorFactory } from "../src/runner.js";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { RecoveryCoordinator, type RecoveryArchiveClient } from "../src/recovery.js";
import { api, client, fakeGitlab, fx, git as harnessGit, gitlabClaim, homeDir, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

// issue #2213 — the additive, credential-free archival capture of a quarantined run's committed work.
// Layer: quarantine-archive.ts over a REAL GitCache and real git bares (part 1), and the runner's
// failure path that invokes it (part 2). The runner part runs through the shared runner harness.

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const TIMEOUT = { timeout: 60_000 };

function git(cwd: string, args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
}

// ─── part 1: the capture over a real bare ──────────────────────────────────────────────────

interface Topo {
  base: string;
  work: string;
  bare: string;
  branch: string;
  runId: string;
  head: string;
  baseSha: string;
  blobPath: string;
  cache: GitCache;
  archiveRoot: string;
}

function makeTopo(): Topo {
  const base = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-2213-archive-"));
  const work = path.join(base, "work");
  const bare = path.join(base, "worker.git");
  const branch = "agent/issue-9";
  fs.mkdirSync(work, { recursive: true });
  execFileSync("git", ["init", "-b", "main", work], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["init", "--bare", "-b", "main", bare], { env: GIT_ENV, stdio: "pipe" });
  for (const [k, v] of [["user.email", "f@uzi.local"], ["user.name", "f"], ["commit.gpgsign", "false"], ["gc.auto", "0"], ["maintenance.auto", "false"]] as const) {
    git(work, ["config", k, v]);
  }
  fs.writeFileSync(path.join(work, "README.md"), "# fixture\n");
  git(work, ["add", "."]);
  git(work, ["commit", "-m", "base"]);
  const baseSha = git(work, ["rev-parse", "HEAD"]);
  git(work, ["checkout", "-b", branch]);
  fs.mkdirSync(path.join(work, "src"), { recursive: true });
  fs.writeFileSync(path.join(work, "src", "impl.txt"), "the committed implementation, long enough to be a distinct blob\n");
  git(work, ["add", "."]);
  git(work, ["commit", "-m", "feat: implementation"]);
  const head = git(work, ["rev-parse", "HEAD"]);
  git(work, ["push", bare, `${branch}:refs/uzi-runner/${branch}`]);
  git(work, ["push", bare, "main:main"]);
  const dataDir = path.join(base, "data");
  const cache = new GitCache(dataDir, nullLogger(), undefined, testGitCacheOptions());
  return { base, work, bare, branch, runId: randomUUID(), head, baseSha, blobPath: "src/impl.txt", cache, archiveRoot: cache.recoveryArchiveRoot };
}

/** Wrap a real GitCache as the capture's git, letting a test intercept one method. */
function wrapped(topo: Topo, hooks: Partial<QuarantineArchiveGit> = {}): QuarantineArchiveGit {
  const c = topo.cache;
  return {
    recoveryArchiveRoot: c.recoveryArchiveRoot,
    trackingTip: (b, br) => c.trackingTip(b, br),
    createQuarantineArchiveRef: (b, r, g, s) => c.createQuarantineArchiveRef(b, r, g, s),
    quarantineArchiveRefTip: (b, r, g) => c.quarantineArchiveRefTip(b, r, g),
    produceRecoveryBundle: (b, o) => c.produceRecoveryBundle(b, o),
    verifyBundleReproduces: (p, s, r) => c.verifyBundleReproduces(p, s, r),
    ...hooks,
  };
}

const refOf = (topo: Topo, ref: string): string | undefined => {
  try {
    return git(topo.bare, ["rev-parse", "--verify", "-q", ref]);
  } catch {
    return undefined;
  }
};
const archiveRef = (topo: Topo, gen = 1): string => `refs/uzi-archive/${topo.runId}/g${gen}`;
const runDir = (topo: Topo): string => path.join(topo.archiveRoot, topo.runId);
const listRunDir = (topo: Topo): string[] => (fs.existsSync(runDir(topo)) ? fs.readdirSync(runDir(topo)).sort() : []);
const capture = (topo: Topo, g: QuarantineArchiveGit, extra: Partial<Parameters<typeof archiveQuarantinedSource>[2]> = {}): Promise<QuarantineArchiveResult> =>
  archiveQuarantinedSource(g, nullLogger(), { runId: topo.runId, generation: 1, barePath: topo.bare, branch: topo.branch, ...extra });

describe("the archival capture over a real bare (issue #2213)", () => {
  let topo: Topo;
  beforeEach(() => {
    topo = makeTopo();
  });
  afterEach(() => fs.rmSync(topo.base, { recursive: true, force: true }));

  it("success: anchors H at the archive ref, writes a verified 0600 bundle and manifest in a 0700 dir, and changes nothing else", async () => {
    // Existing pins and the tracking ref must stay exactly as they are.
    git(topo.bare, ["update-ref", `refs/uzi-recovery-pin/${topo.runId}/1`, topo.baseSha]);
    const refsBefore = git(topo.bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-recovery-pin", "refs/uzi-runner"]);
    const r = await capture(topo, wrapped(topo));
    assert.equal(r.outcome, "archived", r.detail);
    assert.equal(r.head, topo.head);
    assert.equal(refOf(topo, archiveRef(topo)), topo.head, "the archive ref is H");
    assert.equal(git(topo.bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-recovery-pin", "refs/uzi-runner"]), refsBefore);
    assert.equal(refOf(topo, "refs/heads/recovered-source"), undefined, "the producer's transient ref is gone");

    assert.deepEqual(listRunDir(topo), ["g1.bundle", "g1.manifest.json"], "no temp or verify leftovers");
    assert.equal(fs.statSync(runDir(topo)).mode & 0o777, 0o700);
    assert.equal(fs.statSync(path.join(runDir(topo), "g1.bundle")).mode & 0o777, 0o600);
    const bytes = fs.readFileSync(path.join(runDir(topo), "g1.bundle"));
    const manifest = JSON.parse(fs.readFileSync(path.join(runDir(topo), "g1.manifest.json"), "utf8")) as Record<string, unknown>;
    assert.equal(manifest.head, topo.head);
    assert.equal(manifest.generation, 1);
    assert.equal(manifest.size, bytes.length);
    assert.equal(manifest.sha256, createHash("sha256").update(bytes).digest("hex"));
    assert.equal(r.sha256, manifest.sha256);
    assert.equal(r.size, bytes.length);

    // The bundle is independently usable: exactly H, importable into a clean repo.
    const heads = git(topo.base, ["bundle", "list-heads", path.join(runDir(topo), "g1.bundle")]).split("\n");
    assert.equal(heads.length, 1);
    assert.equal(heads[0]!.split(" ")[0], topo.head);
    const clean = path.join(topo.base, "clean");
    execFileSync("git", ["init", "-q", clean], { env: GIT_ENV });
    git(clean, ["fetch", path.join(runDir(topo), "g1.bundle"), `${heads[0]!.split(" ")[1]}:refs/heads/imported`]);
    assert.equal(git(clean, ["rev-parse", "refs/heads/imported"]), topo.head);
    assert.equal(git(clean, ["show", `${topo.head}:${topo.blobPath}`]), "the committed implementation, long enough to be a distinct blob");

    assert.match(archiveFailureReasonSuffix(r), new RegExp(`head ${topo.head}.*sha256 ${r.sha256}`));
  });

  it("no tracking ref: no_source, and nothing is created", async () => {
    git(topo.bare, ["update-ref", "-d", `refs/uzi-runner/${topo.branch}`]);
    const r = await capture(topo, wrapped(topo));
    assert.equal(r.outcome, "no_source");
    assert.equal(refOf(topo, archiveRef(topo)), undefined);
    assert.deepEqual(listRunDir(topo), []);
    assert.equal(archiveFailureReasonSuffix(r), "");
  });

  it("no bare or branch (a pre-clone failure): no_source", async () => {
    assert.equal((await capture(topo, wrapped(topo), { barePath: undefined })).outcome, "no_source");
    assert.equal((await capture(topo, wrapped(topo), { branch: undefined })).outcome, "no_source");
  });

  it("(i) the tracking ref moves to H' between resolve and bundle: the archive still reproduces H, anchored only by its ref", async () => {
    // H' is unrelated to H (it branches off the base), so once the tracking ref leaves H nothing but
    // the archive ref reaches H, and a prune in the bare deletes it unless that ref anchors it.
    git(topo.work, ["checkout", "-q", "-b", "other", topo.baseSha]);
    git(topo.work, ["-c", "user.email=f@uzi.local", "-c", "user.name=f", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "unrelated"]);
    const hPrime = git(topo.work, ["rev-parse", "HEAD"]);
    git(topo.work, ["push", topo.bare, `HEAD:refs/uzi-runner/other-staging`]);
    const g = wrapped(topo, {
      produceRecoveryBundle: async (b, o) => {
        git(topo.bare, ["update-ref", `refs/uzi-runner/${topo.branch}`, hPrime]);
        git(topo.bare, ["update-ref", "-d", "refs/uzi-runner/other-staging"]);
        git(topo.bare, ["prune", "--expire=now"]);
        return topo.cache.produceRecoveryBundle(b, o);
      },
    });
    const r = await capture(topo, g);
    assert.equal(r.outcome, "archived", r.detail);
    assert.equal(r.head, topo.head, "H as resolved, not H'");
    assert.equal(refOf(topo, archiveRef(topo)), topo.head);
    assert.equal(git(topo.bare, ["cat-file", "-t", topo.head]), "commit", "H survived the prune");
    const heads = git(topo.base, ["bundle", "list-heads", path.join(runDir(topo), "g1.bundle")]);
    assert.equal(heads.split(" ")[0], topo.head);
  });

  it("(ii) the archive ref is force-moved during the capture: incomplete, the temp bundle is deleted, nothing is published", async () => {
    const g = wrapped(topo, {
      produceRecoveryBundle: async (b, o) => {
        const out = await topo.cache.produceRecoveryBundle(b, o);
        git(topo.bare, ["update-ref", archiveRef(topo), topo.baseSha]);
        return out;
      },
    });
    const r = await capture(topo, g);
    assert.equal(r.outcome, "incomplete");
    assert.match(String(r.detail), /archive ref no longer names/);
    assert.deepEqual(listRunDir(topo), [], "no bundle, manifest or temp");
    assert.equal(archiveFailureReasonSuffix(r), "");
  });

  it("(iii) a forged loose object under an OID in H's closure is not reproduced: incomplete, nothing published", async () => {
    // Substitute the blob's loose object with different content under the SAME path (so its OID is
    // unchanged): `git cat-file` serves it without re-hashing it.
    const blob = git(topo.bare, ["rev-parse", `${topo.head}:${topo.blobPath}`]);
    const loose = path.join(topo.bare, "objects", blob.slice(0, 2), blob.slice(2));
    assert.ok(fs.existsSync(loose), "the fixture's blob is a loose object");
    const forged = Buffer.from("forged content, a different length than the original blob\n");
    fs.chmodSync(loose, 0o644);
    fs.writeFileSync(loose, deflateSync(Buffer.concat([Buffer.from(`blob ${forged.length}\0`), forged])));
    const r = await capture(topo, wrapped(topo));
    assert.equal(r.outcome, "incomplete", `${r.outcome}: ${r.detail}`);
    assert.deepEqual(listRunDir(topo), [], "no partial bundle is left as if verified");
    assert.equal(archiveFailureReasonSuffix(r), "");
  });

  it("the verification itself rejects a bundle that does not carry exactly H", async () => {
    const out = path.join(topo.base, "other.bundle");
    git(topo.bare, ["update-ref", "refs/heads/recovered-source", topo.baseSha]);
    git(topo.bare, ["bundle", "create", out, "refs/heads/recovered-source"]);
    git(topo.bare, ["update-ref", "-d", "refs/heads/recovered-source"]);
    fs.mkdirSync(topo.archiveRoot, { recursive: true });
    const reason = await topo.cache.verifyBundleReproduces(out, topo.head, topo.archiveRoot);
    assert.match(String(reason), /heads are not exactly/);
    assert.equal(await topo.cache.verifyBundleReproduces(out, topo.baseSha, topo.archiveRoot), undefined, "control: the right head verifies");
    assert.deepEqual(fs.readdirSync(topo.archiveRoot), [], "the scratch repo is removed");
  });

  describe("failures during the transfer: the outcome is incomplete or too_large, never a published archive", () => {
    const cases: Array<{ name: string; hooks: (t: Topo) => Partial<QuarantineArchiveGit>; outcome: QuarantineArchiveResult["outcome"]; extra?: Record<string, unknown> }> = [
      {
        name: "the producer throws",
        hooks: () => ({ produceRecoveryBundle: async () => { throw new Error("boom"); } }),
        outcome: "incomplete",
      },
      {
        name: "the producer reports the source commit absent",
        hooks: () => ({ produceRecoveryBundle: async () => { throw new Error("produceRecoveryBundle: source commit is not present in the trusted bare"); } }),
        outcome: "incomplete",
      },
      {
        name: "RecoveryBundleTooLargeError (a thrown one)",
        hooks: () => ({ produceRecoveryBundle: async () => { throw new RecoveryBundleTooLargeError(99, 1); } }),
        outcome: "too_large",
      },
      { name: "the real size cap", hooks: () => ({}), outcome: "too_large", extra: { maxBytes: 1 } },
      {
        name: "the verification step fails",
        hooks: () => ({ verifyBundleReproduces: async () => "bundle verification failed: injected" }),
        outcome: "incomplete",
      },
      {
        name: "the verification step throws",
        hooks: () => ({ verifyBundleReproduces: async () => { throw new Error("verify exploded"); } }),
        outcome: "incomplete",
      },
      {
        name: "the archive ref cannot be created (a conflicting ref)",
        hooks: (t) => ({ createQuarantineArchiveRef: async () => { git(t.bare, ["update-ref", archiveRef(t), t.baseSha]); return t.cache.createQuarantineArchiveRef(t.bare, t.runId, 1, t.head); } }),
        outcome: "incomplete",
      },
    ];
    for (const c of cases) {
      it(c.name, async () => {
        const r = await capture(topo, wrapped(topo, c.hooks(topo)), c.extra ?? {});
        assert.equal(r.outcome, c.outcome, r.detail);
        assert.ok(!listRunDir(topo).some((f) => f === "g1.bundle" || f === "g1.manifest.json"), listRunDir(topo).join(","));
        assert.deepEqual(listRunDir(topo).filter((f) => f.endsWith(".tmp")), [], "no temp left");
        assert.equal(archiveFailureReasonSuffix(r), "");
      });
    }

    it("the rename cannot complete: incomplete, no partial g1.bundle or manifest", async () => {
      fs.mkdirSync(path.join(runDir(topo), "g1.bundle", "blocker"), { recursive: true });
      const r = await capture(topo, wrapped(topo));
      assert.equal(r.outcome, "incomplete", r.detail);
      assert.ok(!fs.existsSync(path.join(runDir(topo), "g1.manifest.json")));
      assert.ok(!listRunDir(topo).some((f) => f.endsWith(".tmp")));
    });

    it("the capture overruns its deadline: incomplete, and a late-finishing capture publishes nothing afterwards", async () => {
      const g = wrapped(topo, {
        produceRecoveryBundle: async (b, o) => {
          await new Promise((r) => setTimeout(r, 400));
          return topo.cache.produceRecoveryBundle(b, o);
        },
      });
      const r = await capture(topo, g, { deadlineMs: 50 });
      assert.equal(r.outcome, "incomplete");
      assert.match(String(r.detail), /did not finish within 50 ms/);
      await new Promise((resolve) => setTimeout(resolve, 1500)); // let the late work finish
      assert.ok(!listRunDir(topo).includes("g1.bundle"), "nothing is published after the deadline");
    });

    for (const step of ["chmod", "rename"] as const) {
      it(`the deadline passes during a slow publish ${step}: incomplete, and no g1.bundle or manifest appears afterwards`, async () => {
        const real = fsp[step] as (...a: unknown[]) => Promise<unknown>;
        let delayed = false;
        (fsp as unknown as Record<string, unknown>)[step] = async (...a: unknown[]) => {
          const target = String(a[0]);
          if (!delayed && target.endsWith(".bundle.tmp")) {
            delayed = true;
            await new Promise((r) => setTimeout(r, 1000));
          }
          return real(...a);
        };
        try {
          const r = await capture(topo, wrapped(topo), { deadlineMs: 700 });
          assert.equal(r.outcome, "incomplete");
          assert.match(String(r.detail), /did not finish within 700 ms/);
          await new Promise((resolve) => setTimeout(resolve, 1500)); // let the late work run to its end
          assert.equal(delayed, true, "the slow step was reached");
          assert.deepEqual(listRunDir(topo).filter((f) => f === "g1.bundle" || f === "g1.manifest.json" || f.endsWith(".tmp")), []);
        } finally {
          (fsp as unknown as Record<string, unknown>)[step] = real;
        }
      });
    }
  });
});

// ─── part 2: the runner's failure path ─────────────────────────────────────────────────────

describe("a run that fails quarantined archives its committed work and releases nothing (issue #2213)", () => {
  const lastFailed = (runId: string) => api.states.filter((s) => s.runId === runId && s.body.status === "failed").at(-1)?.body;
  const journalOf = (iid: number): string | undefined => {
    try {
      return git(harnessGit.barePathFor(fx.originPath), ["config", "--local", "--get", `uzi-recovery.agent/issue-${iid}.clone`]) || undefined;
    } catch {
      return undefined;
    }
  };

  interface Spy {
    clientCalls: string[];
    deletedPins: unknown[];
    afterLatchGit: Array<{ args: string[]; credentialed: boolean }>;
  }
  function installSpies(afterLatch: { on: boolean }): Spy {
    const spy: Spy = { clientCalls: [], deletedPins: [], afterLatchGit: [] };
    for (const m of ["reserveRecoveryCapture", "uploadRecoveryBundle", "releaseRecoveryCustody"] as const) {
      (client as unknown as Record<string, unknown>)[m] = async () => {
        spy.clientCalls.push(m);
        throw new Error(`${m} must not be called`);
      };
    }
    const realDelete = harnessGit.deleteRecoveryPin.bind(harnessGit);
    harnessGit.deleteRecoveryPin = (async (...a: Parameters<typeof realDelete>) => {
      spy.deletedPins.push(a);
      return realDelete(...a);
    }) as typeof harnessGit.deleteRecoveryPin;
    const priv = harnessGit as unknown as { execScoped: (c: string, a: string[], o: { env: NodeJS.ProcessEnv }, i?: unknown) => Promise<unknown> };
    const realExec = priv.execScoped.bind(harnessGit);
    priv.execScoped = (c, a, o, i) => {
      if (afterLatch.on) {
        const credentialed = Object.entries(o.env).some(([k, v]) => k.startsWith("GIT_CONFIG_VALUE_") && String(v).startsWith("Authorization: Basic"));
        spy.afterLatchGit.push({ args: a, credentialed });
      }
      return realExec(c, a, o, i);
    };
    return spy;
  }

  /** An executor that commits work, lets a checkpoint fetch it into the bare, then hits the latch at a turn boundary. */
  function latchingExecutor(iid: number, afterLatch: { on: boolean }): ExecutorFactory {
    return (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "committed work\n");
          execFileSync("git", ["-C", ctx.worktreePath, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
          execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
          await harnessGit.fetchAgentBranch(harnessGit.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, runId);
          latchResidueQuarantine({ cause: `runner-uid pid 4242 "ssh-agent" could not be attributed (env/cwd unreadable)`, runId, site: "pre_clone" }, nullLogger());
          afterLatch.on = true;
          assertResidueQuarantineOpen("provider_turn"); // the boundary check at the loop top
          throw new Error(`unreachable ${iid}`);
        },
      },
    });
  }

  it("fails worker_residue_blocked, anchors and verifies H, appends H and the sha256 to the reason, and releases/deletes/retires nothing", TIMEOUT, async () => {
    const iid = 22190;
    const bare = harnessGit.barePathFor(fx.originPath);
    const afterLatch = { on: false };
    const spy = installSpies(afterLatch);
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const claim = gitlabClaim(iid, { claim_generation: 3 });
    await runnerWith(latchingExecutor(iid, afterLatch), gitlab, undefined, undefined, { checkpointIntervalMs: 0, recoveryRetryMs: 1 }).execute(claim);

    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    const reason = String(failed?.failure_reason);
    assert.match(reason, /this worker is quarantined/);
    const head = git(bare, ["rev-parse", `refs/uzi-runner/agent/issue-${iid}`]);
    assert.equal(git(bare, ["rev-parse", `refs/uzi-archive/${claim.run_id}/g3`]), head, "the archive ref is H");
    const dir = path.join(harnessGit.recoveryArchiveRoot, claim.run_id);
    const bundle = path.join(dir, "g3.bundle");
    const sha = createHash("sha256").update(fs.readFileSync(bundle)).digest("hex");
    assert.match(reason, new RegExp(`head ${head}`), "H is in the failure reason");
    assert.match(reason, new RegExp(`sha256 ${sha}`), "the bundle sha256 is in the failure reason");
    assert.ok(reason.length <= 512);
    assert.equal(JSON.parse(fs.readFileSync(path.join(dir, "g3.manifest.json"), "utf8")).sha256, sha);
    assert.equal(git(path.dirname(bundle), ["bundle", "list-heads", bundle]).split(" ")[0], head);

    // Nothing released, deleted or retired.
    assert.deepEqual(spy.clientCalls, [], "no reserve, upload or release client call");
    assert.deepEqual(spy.deletedPins, [], "no recovery pin was deleted");
    assert.ok(journalOf(iid), "the journal still points at the kept clone");
    assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
    assert.equal(mrCalls.length, 0, "nothing was published");
    // After the latch, the only git the failure path ran carried no credential.
    assert.deepEqual(spy.afterLatchGit.filter((g) => g.credentialed), [], "no credential-marked git child");
    const runnerCloneGit = spy.afterLatchGit.filter((g) => g.args[0] === "-C" && String(g.args[1]).startsWith(worktreeDirFor(iid)));
    assert.deepEqual(runnerCloneGit, [], "no git ran in the runner clone after the latch");
  });

  it("with no committed work in the bare (no_source) the typed failure is unchanged and nothing is appended", TIMEOUT, async () => {
    const iid = 22191;
    const afterLatch = { on: false };
    const spy = installSpies(afterLatch);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (): Promise<ExecutorResult> => {
          latchResidueQuarantine({ cause: "c", runId, site: "pre_clone" }, nullLogger());
          assertResidueQuarantineOpen("provider_turn");
          throw new Error("unreachable");
        },
      },
    });
    const claim = gitlabClaim(iid);
    await runnerWith(factory, fakeGitlab().gitlab).execute(claim);
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.doesNotMatch(String(failed?.failure_reason), /archived/);
    assert.equal(fs.existsSync(path.join(harnessGit.recoveryArchiveRoot, claim.run_id)), false);
    assert.deepEqual(spy.clientCalls, []);
  });

  /** Everything the failure path must leave exactly as it was. */
  function retained(iid: number, runId: string) {
    const bare = harnessGit.barePathFor(fx.originPath);
    const readTree = (dir: string): Record<string, string> => {
      const out: Record<string, string> = {};
      const walk = (d: string): void => {
        if (!fs.existsSync(d)) return;
        for (const e of fs.readdirSync(d, { withFileTypes: true })) {
          const f = path.join(d, e.name);
          if (e.isDirectory()) walk(f);
          else out[path.relative(dir, f)] = fs.readFileSync(f, "utf8");
        }
      };
      walk(dir);
      return out;
    };
    return {
      pins: git(bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-recovery-pin"]),
      journalClone: journalOf(iid),
      recoveryFiles: readTree(path.join(harnessGit.recoveryRoot, runId)),
      settlementFiles: readTree(path.join(harnessGit.recoverySettlementRoot, runId)),
    };
  }

  /** Spies the runner's reap and settle: neither may run on a quarantined worker. */
  function spyReapSettle(runner: RunRunner): { reaps: number; settles: number } {
    const counts = { reaps: 0, settles: 0 };
    const priv = runner as unknown as Record<string, unknown>;
    priv.reapRecoveryProviderForSettle = async () => {
      counts.reaps++;
      return false;
    };
    priv.settleRecoveryGeneration = async () => {
      counts.settles++;
    };
    return counts;
  }

  /** Commits work, checkpoints it into the bare, plants a recovery pin, latches, snapshots, then `fail`s. */
  function failingAfterLatch(iid: number, afterLatch: { on: boolean }, snap: { before?: ReturnType<typeof retained> }, fail: () => never): ExecutorFactory {
    return (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "committed work\n");
          execFileSync("git", ["-C", ctx.worktreePath, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
          execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
          await harnessGit.fetchAgentBranch(harnessGit.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, runId);
          const bare = harnessGit.barePathFor(fx.originPath);
          git(bare, ["update-ref", `refs/uzi-recovery-pin/${runId}/3`, git(bare, ["rev-parse", `refs/uzi-runner/${ctx.branch}`])]);
          latchResidueQuarantine({ cause: `runner-uid pid 4242 "ssh-agent" could not be attributed (env/cwd unreadable)`, runId, site: "pre_clone" }, nullLogger());
          afterLatch.on = true;
          snap.before = retained(iid, runId);
          return fail();
        },
      },
    });
  }

  function assertNothingReleased(iid: number, runId: string, spy: Spy, counts: { reaps: number; settles: number }, before: ReturnType<typeof retained> | undefined): void {
    assert.ok(before, "the snapshot was taken");
    assert.deepEqual(spy.clientCalls, [], "no reserve, upload or release client call");
    assert.deepEqual(spy.deletedPins, [], "no recovery pin was deleted");
    assert.deepEqual(counts, { reaps: 0, settles: 0 }, "no reap and no custody settle ran");
    assert.deepEqual(retained(iid, runId), before, "pins, the journal and the recovery files are exactly as they were");
    assert.ok(journalOf(iid), "the journal still points at the kept clone");
    assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
  }

  for (const c of [
    {
      name: "incomplete (the archive ref is force-moved during the capture)",
      iid: 22194,
      tweak: () => {
        const real = harnessGit.quarantineArchiveRefTip.bind(harnessGit);
        harnessGit.quarantineArchiveRefTip = async (b, r, g) => {
          await real(b, r, g);
          return "0".repeat(40);
        };
      },
      outcome: /incomplete/,
    },
    {
      name: "too_large",
      iid: 22195,
      tweak: () => {
        harnessGit.produceRecoveryBundle = async () => {
          throw new RecoveryBundleTooLargeError(99, 1);
        };
      },
      outcome: /too_large/,
    },
  ]) {
    it(`a ${c.name} archive keeps the typed failure and releases, deletes and retires nothing`, TIMEOUT, async () => {
      const afterLatch = { on: false };
      const spy = installSpies(afterLatch);
      c.tweak();
      const { logger, lines } = recordingLogger();
      const snap: { before?: ReturnType<typeof retained> } = {};
      const claim = gitlabClaim(c.iid, { claim_generation: 3 });
      const factory = failingAfterLatch(c.iid, afterLatch, snap, () => {
        assertResidueQuarantineOpen("provider_turn");
        throw new Error("unreachable");
      });
      const runner = runnerWith(factory, fakeGitlab().gitlab, undefined, logger, { checkpointIntervalMs: 0, recoveryRetryMs: 1 });
      const counts = spyReapSettle(runner);
      await runner.execute(claim);
      const failed = lastFailed(claim.run_id);
      assert.equal(failed?.fail_origin, "worker_residue_blocked");
      assert.match(String(failed?.failure_reason), /this worker is quarantined/);
      assert.doesNotMatch(String(failed?.failure_reason), /archived on the worker/);
      assert.ok(
        (lines as Array<{ msg?: string; outcome?: string }>).some((l) => l.msg === "quarantine archival capture finished" && c.outcome.test(String(l.outcome))),
        "the capture ran and reported its non-archived outcome",
      );
      assert.equal(fs.existsSync(path.join(harnessGit.recoveryArchiveRoot, claim.run_id, "g3.bundle")), false, "nothing was published");
      assertNothingReleased(c.iid, claim.run_id, spy, counts, snap.before);
    });
  }

  it("a non-quarantine failure on an already-latched worker reaps and settles nothing and keeps the clone", TIMEOUT, async () => {
    const iid = 22196;
    const afterLatch = { on: false };
    const spy = installSpies(afterLatch);
    const snap: { before?: ReturnType<typeof retained> } = {};
    const claim = gitlabClaim(iid, { claim_generation: 3 });
    const factory = failingAfterLatch(iid, afterLatch, snap, () => {
      throw new Error("the agent failed for an unrelated reason");
    });
    const runner = runnerWith(factory, fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0, recoveryRetryMs: 1 });
    const counts = spyReapSettle(runner);
    await runner.execute(claim);
    const failed = lastFailed(claim.run_id);
    assert.ok(failed, "the run failed");
    assert.doesNotMatch(String(failed?.failure_reason), /quarantined/, "an unrelated failure, not the quarantine one");
    assertNothingReleased(iid, claim.run_id, spy, counts, snap.before);
  });

  it("control: the same unrelated failure on an UNLATCHED worker does reap and settle", TIMEOUT, async () => {
    const iid = 22197;
    const afterLatch = { on: false };
    installSpies(afterLatch);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (): Promise<ExecutorResult> => {
          throw new Error("the agent failed for an unrelated reason");
        },
      },
    });
    const runner = runnerWith(factory, fakeGitlab().gitlab);
    const counts = spyReapSettle(runner);
    await runner.execute(gitlabClaim(iid));
    assert.ok(counts.reaps >= 1, "the ordinary failure path reaps for the settle");
  });

  /** The harness leaves recovery disabled (no worker token); these tests need the real coordinator so a missing gate reaches the client. */
  function realRecovery(): RecoveryCoordinator {
    return new RecoveryCoordinator({
      client: client as unknown as RecoveryArchiveClient,
      git: harnessGit,
      log: nullLogger(),
      recoveryRoot: harnessGit.recoveryRoot,
      workerToken: "issue-2213-worker-token-0123456789",
    });
  }

  /** An executor that commits work, lets a checkpoint fetch it into the bare, plants a pin and fails (NOT latched). */
  function failingUnlatched(iid: number, snap: { before?: ReturnType<typeof retained> }): ExecutorFactory {
    return (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "committed work\n");
          execFileSync("git", ["-C", ctx.worktreePath, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
          execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
          await harnessGit.fetchAgentBranch(harnessGit.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, runId);
          const bare = harnessGit.barePathFor(fx.originPath);
          git(bare, ["update-ref", `refs/uzi-recovery-pin/${runId}/3`, git(bare, ["rev-parse", `refs/uzi-runner/${ctx.branch}`])]);
          snap.before = retained(iid, runId);
          throw new Error("the agent failed for an unrelated reason");
        },
      },
    });
  }

  it("a latch that lands inside the failure path's reap (after the entry snapshot) still settles nothing and retires nothing", TIMEOUT, async () => {
    const iid = 22201;
    const afterLatch = { on: false };
    const spy = installSpies(afterLatch);
    const snap: { before?: ReturnType<typeof retained> } = {};
    const claim = gitlabClaim(iid, { claim_generation: 3 });
    const runner = runnerWith(failingUnlatched(iid, snap), fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0, recoveryRetryMs: 1, recovery: realRecovery() });
    (runner as unknown as Record<string, unknown>).reapRecoveryProviderForSettle = async () => {
      latchResidueQuarantine({ cause: `runner-uid pid 4242 "ssh-agent" could not be attributed (env/cwd unreadable)`, runId: claim.run_id, site: "reap" }, nullLogger());
      afterLatch.on = true;
      return true;
    };
    await runner.execute(claim);
    assert.ok(lastFailed(claim.run_id), "the run failed");
    assert.deepEqual(spy.clientCalls, [], "no reserve, upload or release client call");
    assert.deepEqual(spy.deletedPins, [], "no recovery pin was deleted");
    assert.deepEqual(retained(iid, claim.run_id), snap.before, "pins, the journal and the recovery files are exactly as they were");
    assert.ok(journalOf(iid), "the journal still points at the kept clone");
    assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
    const bare = harnessGit.barePathFor(fx.originPath);
    assert.equal(git(bare, ["show", `refs/uzi-runner/agent/issue-${iid}:WORK.txt`]).trim(), "committed work", "the commit is still recoverable");
  });

  it("control: the same failure whose reap does not latch settles (the capture reaches the client)", TIMEOUT, async () => {
    const iid = 22202;
    const afterLatch = { on: false };
    const spy = installSpies(afterLatch);
    const snap: { before?: ReturnType<typeof retained> } = {};
    const claim = gitlabClaim(iid, { claim_generation: 3 });
    const runner = runnerWith(failingUnlatched(iid, snap), fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0, recoveryRetryMs: 1, recovery: realRecovery() });
    (runner as unknown as Record<string, unknown>).reapRecoveryProviderForSettle = async () => true;
    await runner.execute(claim);
    assert.ok(spy.clientCalls.length > 0, "unlatched, the settle ran and reached the recovery client");
  });

  it("a latch landing during a finalization failure's terminal drive uploads and releases nothing and keeps its journal record", TIMEOUT, async () => {
    const iid = 22203;
    const afterLatch = { on: false };
    const spy = installSpies(afterLatch);
    // The credentialed forge-tip fetch is refused while latched; hand the bundle producer a tip so a
    // missing gate would reach the client reserve.
    (harnessGit as unknown as Record<string, unknown>).fetchDefaultTip = async () =>
      git(harnessGit.barePathFor(fx.originPath), ["rev-parse", "--verify", "refs/remotes/origin/main"]).trim();
    // An undeclared empty-diff finish fails the finalization through the terminal drive (the head
    // is already on the forge tip, so an ungated drive would RELEASE its hold); the latch lands in
    // the fetch-back that precedes the finalization pin.
    const realFetchBack = harnessGit.fetchAgentBranch.bind(harnessGit);
    harnessGit.fetchAgentBranch = (async (...a: Parameters<typeof realFetchBack>) => {
      const ref = await realFetchBack(...a);
      latchResidueQuarantine({ cause: `runner-uid pid 4242 "ssh-agent" could not be attributed (env/cwd unreadable)`, site: "terminal_drive" }, nullLogger());
      afterLatch.on = true;
      return ref;
    }) as typeof harnessGit.fetchAgentBranch;
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch, summary: "done" }) as unknown as ExecutorResult,
      },
    });
    const claim = gitlabClaim(iid, { claim_generation: 3 });
    await runnerWith(factory, fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0, recoveryRetryMs: 1, recovery: realRecovery() }).execute(claim);
    assert.match(String(lastFailed(claim.run_id)?.failure_reason), /no changes were committed/, "the finalization failed through the terminal drive");
    assert.deepEqual(spy.clientCalls, [], "no reserve, upload or release client call");
    assert.deepEqual(spy.deletedPins, [], "no recovery pin was deleted");
    assert.ok(fs.readdirSync(path.join(harnessGit.recoveryRoot, claim.run_id)).some((f) => f.endsWith(".json")), "the journal record is kept");
  });

  it("a latch during the recovery-exhausted capture fails the run worker_residue_blocked instead of retrying forever", TIMEOUT, async () => {
    const iid = 22198;
    const afterLatch = { on: false };
    const spy = installSpies(afterLatch);
    let captureCalls = 0;
    harnessGit.commitWipMarker = async () => {
      captureCalls++;
      latchResidueQuarantine({ cause: `runner-uid pid 4242 "ssh-agent" could not be attributed (env/cwd unreadable)`, site: "recovery_capture" }, nullLogger());
      afterLatch.on = true;
      throw new ResidueQuarantinedError("git", "pid 4242");
    };
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "uncommitted work\n");
          throw new TransientRecoveryError();
        },
      },
    });
    const claim = gitlabClaim(iid);
    const runner = runnerWith(factory, fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0, recoveryRetryMs: 1 });
    const counts = spyReapSettle(runner);
    await runner.execute(claim);
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked", String(failed?.failure_reason));
    assert.equal(captureCalls, 1, "the capture was not retried");
    assert.deepEqual(spy.clientCalls, []);
    assert.deepEqual(counts, { reaps: 0, settles: 0 });
    assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
  });

  for (const c of [
    { name: "shutdown", iid: 22204, act: (r: RunRunner) => r.shutdown() },
    {
      name: "cancel",
      iid: 22205,
      act: (r: RunRunner, runId: string) => {
        const flightActive = (r as unknown as { activeRuns: Map<string, { steering: unknown }> }).activeRuns.get(runId);
        (flightActive!.steering as { cancelled: boolean }).cancelled = true;
      },
    },
  ]) {
    it(`a ${c.name} that arrives with the capture's latch refusal takes precedence over the worker_residue_blocked failure`, TIMEOUT, async () => {
      const afterLatch = { on: false };
      installSpies(afterLatch);
      const claim = gitlabClaim(c.iid);
      let runner: RunRunner | undefined;
      let calls = 0;
      harnessGit.commitWipMarker = async () => {
        calls++;
        latchResidueQuarantine({ cause: "pid 4242", site: "recovery_capture" }, nullLogger());
        afterLatch.on = true;
        c.act(runner!, claim.run_id);
        throw new ResidueQuarantinedError("git", "pid 4242");
      };
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeDir, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "uncommitted work\n");
            throw new TransientRecoveryError();
          },
        },
      });
      runner = runnerWith(factory, fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0, recoveryRetryMs: 1 });
      await runner.execute(claim);
      assert.ok(calls >= 1, "the capture ran");
      const failed = lastFailed(claim.run_id);
      assert.notEqual(failed?.fail_origin, "worker_residue_blocked", String(failed?.failure_reason));
      if (c.name === "shutdown") {
        assert.equal(fs.existsSync(path.join(worktreeDirFor(c.iid), "WORK.txt")), true, "the retained posture keeps the clone");
      } else {
        assert.match(String(failed?.failure_reason), /cancel/i, "the cancel is what is reported");
      }
    });
  }

  it("an archival crash never masks the typed failure, and still releases, deletes and retires nothing", TIMEOUT, async () => {
    const iid = 22192;
    const afterLatch = { on: false };
    const spy = installSpies(afterLatch);
    harnessGit.createQuarantineArchiveRef = async () => {
      throw new Error("the archive exploded");
    };
    const { logger, lines } = recordingLogger();
    const snap: { before?: ReturnType<typeof retained> } = {};
    const claim = gitlabClaim(iid);
    const factory = failingAfterLatch(iid, afterLatch, snap, () => {
      assertResidueQuarantineOpen("provider_turn");
      throw new Error("unreachable");
    });
    const runner = runnerWith(factory, fakeGitlab().gitlab, undefined, logger);
    const counts = spyReapSettle(runner);
    await runner.execute(claim);
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.doesNotMatch(String(failed?.failure_reason), /archived/);
    assert.ok((lines as Array<{ msg?: string; outcome?: string }>).some((l) => l.msg === "quarantine archival capture finished" && l.outcome === "incomplete"));
    assertNothingReleased(iid, claim.run_id, spy, counts, snap.before);
  });

  it("control: a plain residue block (not quarantined) runs no archival capture", TIMEOUT, async () => {
    const iid = 22193;
    const afterLatch = { on: false };
    installSpies(afterLatch);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "w\n");
          execFileSync("git", ["-C", ctx.worktreePath, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
          execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
          await harnessGit.fetchAgentBranch(harnessGit.barePathFor(fx.originPath), ctx.worktreePath, ctx.branch, runId);
          throw new RunResidueBlockedError("a clone survivor");
        },
      },
    });
    const claim = gitlabClaim(iid);
    await runnerWith(factory, fakeGitlab().gitlab).execute(claim);
    assert.equal(lastFailed(claim.run_id)?.fail_origin, "worker_residue_blocked");
    assert.equal(fs.existsSync(path.join(harnessGit.recoveryArchiveRoot, claim.run_id)), false);
    assert.equal(residueQuarantine(), undefined);
  });
});
