import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { deflateSync } from "node:zlib";
import { GitCache, RecoveryBundleTooLargeError } from "../src/git.js";
import {
  archiveFailureReasonSuffix,
  archiveQuarantinedSource,
  type QuarantineArchiveGit,
  type QuarantineArchiveResult,
} from "../src/quarantine-archive.js";
import { RunResidueBlockedError, assertResidueQuarantineOpen, latchResidueQuarantine, residueQuarantine } from "../src/residue-quarantine.js";
import type { ExecutorFactory } from "../src/runner.js";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
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

  it("(i) the tracking ref moves to H' between resolve and bundle: the archive still reproduces H, anchored by its ref", async () => {
    git(topo.work, ["commit", "--allow-empty", "-m", "later"]);
    const hPrime = git(topo.work, ["rev-parse", "HEAD"]);
    git(topo.work, ["push", topo.bare, `HEAD:refs/uzi-runner/other-staging`]);
    const g = wrapped(topo, {
      produceRecoveryBundle: async (b, o) => {
        git(topo.bare, ["update-ref", `refs/uzi-runner/${topo.branch}`, hPrime]);
        return topo.cache.produceRecoveryBundle(b, o);
      },
    });
    const r = await capture(topo, g);
    assert.equal(r.outcome, "archived", r.detail);
    assert.equal(r.head, topo.head, "H as resolved, not H'");
    assert.equal(refOf(topo, archiveRef(topo)), topo.head);
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

  it("an archival crash never masks the typed failure", TIMEOUT, async () => {
    const iid = 22192;
    const afterLatch = { on: false };
    installSpies(afterLatch);
    harnessGit.createQuarantineArchiveRef = async () => {
      throw new Error("the archive exploded");
    };
    const { logger, lines } = recordingLogger();
    const claim = gitlabClaim(iid);
    await runnerWith(latchingExecutor(iid, afterLatch), fakeGitlab().gitlab, undefined, logger).execute(claim);
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.doesNotMatch(String(failed?.failure_reason), /archived/);
    assert.ok((lines as Array<{ msg?: string; outcome?: string }>).some((l) => l.msg === "quarantine archival capture finished" && l.outcome === "incomplete"));
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
