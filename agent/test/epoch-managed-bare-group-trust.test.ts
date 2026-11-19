import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { PodEpochReclaimScope } from "../templates/pod-epoch-init.cjs";
import { formatAttemptId } from "../src/attempt-path.js";
import { GitCache, type GitCacheOptions } from "../src/git.js";
import { managedBareGroupTrustForTest, type ManagedBareGroupTrust } from "../src/runner-group-trust.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// Issue #4486: the pod-epoch content proof admits a group-writable (2775 / 0775) managed bare only under an issued
// runner-group trust, a safe ancestor chain, a private bare tree, an allowed filesystem and no extended ACL. Every
// case runs the REAL proof (proveEpochWorktreeContent, and the discard that consumes it); only the getfacl launch
// (not installed on every host), statfs and the trust value are injected.

const RUN = "run-managed-bare-trust";
const BRANCH = "agent/issue-4486";
const ENV: NodeJS.ProcessEnv = {
  ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))),
  GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0",
};
const UID = process.getuid!();
const GID = process.getgid!();

function foreignGid(): number {
  const gid = process.getgroups!().find(g => g !== GID);
  assert.ok(gid !== undefined, "this test needs a supplementary group other than the primary one (none found): " +
    "run it as a user with a second group");
  return gid;
}

function statusText(uid: number, gid: number, groups: string): string {
  return ["Name:\tcat", `Uid:\t${uid}\t${uid}\t${uid}\t${uid}`, `Gid:\t${gid}\t${gid}\t${gid}\t${gid}`, `Groups:\t${groups}`,
    "CapInh:\t0000000000000000", "CapPrm:\t0000000000000000", "CapEff:\t0000000000000000",
    "CapBnd:\t00000000000000c0", "CapAmb:\t0000000000000000", "NoNewPrivs:\t1"].join("\n") + "\n";
}
const runnerStatus = (groups = "10002") => statusText(10002, 10002, groups);
const commandStatus = (groups = "10002") => statusText(10003, 10003, groups);
/** A healthy probe result carrying THIS process's group (the production worker is uid/gid 10001, a test is not). */
const trustedForThisProcess = (): ManagedBareGroupTrust => managedBareGroupTrustForTest({
  runnerStatus: runnerStatus(), commandStatus: commandStatus(), anyIdentity: true, gid: GID });

const EXT4 = 0xef53;
const cleanups: Array<() => void> = [];
afterEach(() => { for (const c of cleanups.splice(0)) c(); });

function makeScope(): PodEpochReclaimScope {
  let pending = 0, stopped = false;
  return {
    assertExclusive() { if (stopped) throw new Error("stopped"); },
    get pendingMutations() { return pending; },
    beginMutation() {
      pending++;
      let settled = false;
      return { settle() { if (!settled) { settled = true; pending--; } } };
    },
    inheritStdio(stdio = ["pipe", "pipe", "pipe"]) { return [...stdio]; },
    stop() { stopped = true; },
    stopped() { return stopped; },
    assertSettled() { if (stopped || pending) throw new Error("unsettled"); },
    finish() { this.assertSettled(); },
  };
}

interface Env {
  fx: Fixture;
  git: GitCache;
  bare: string;
  reposRoot: string;
  dataDir: string;
  clonePath: string;
  skills: string;
  scope: PodEpochReclaimScope;
  /** Every ACL chunk the default observer saw. */
  acl: string[][];
  selector: { runId: string; generation: number; attempt: string };
  options: Parameters<GitCache["discardStoppedSource"]>[0];
  input: Parameters<GitCache["createEpochDiscardProof"]>[0];
  raw: string;
}

const bareGit = (bare: string, ...args: string[]) =>
  execFileSync("git", ["-C", bare, ...args], { env: ENV, encoding: "utf8", stdio: "pipe" }).trim();

type CloneShape = "alternate" | "standalone";
const CLONE_SHAPES: readonly CloneShape[] = ["alternate", "standalone"];

async function setup(over: GitCacheOptions = {}, shape: CloneShape = "alternate"): Promise<Env> {
  const fx = makeFixture({ "dir/inner.txt": "inner\n" });
  cleanups.push(() => { try { fs.chmodSync(fx.dataDir, 0o700); fs.chmodSync(path.join(fx.dataDir, "repos"), 0o700); } catch { /* gone */ } fx.cleanup(); });
  const acl: string[][] = [];
  const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
    managedBareGroupTrust: trustedForThisProcess(),
    managedBareAclObserve: async paths => { acl.push([...paths]); },
    managedBareStatfs: async () => ({ type: EXT4 }),
    ...over,
  }));
  const bare = await git.ensureClone(fx.originPath);
  const attempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0000000000000001");
  const clone = await git.runnerCloneForBranch(bare, BRANCH, "issue-4486", noProofReseed, RUN, false, undefined,
    { attemptId: attempt, isLive: () => false, beforeSeed: async () => {} });
  const clonePath = clone.path;
  await git.markRecoveryCapture(bare, clonePath, BRANCH, RUN, attempt);
  if (shape === "standalone") {
    // A materialized clone: every borrowed object copied in, then the alternate removed.
    execFileSync("git", ["-C", clonePath, "-c", "pack.writeReverseIndex=false", "-c", "repack.updateServerInfo=false",
      "repack", "-a", "-d"], { env: ENV, stdio: "pipe" });
    fs.rmSync(path.join(clonePath, ".git", "objects", "info", "alternates"));
    execFileSync("git", ["-C", clonePath, "cat-file", "-e", "HEAD"], { env: ENV, stdio: "pipe" });
  }
  const raw = bareGit(bare, "config", "--local", `uzi-recovery.${BRANCH}.clone`);
  const selector = { runId: RUN, generation: 1, attempt };
  const holds = { state: "read" as const, holds: [] };
  const lifecycle = { state: "terminal" as const, status: "completed" as const };
  const inspection = await git.inspectRetainedSources({ selector, barePath: bare, holds, lifecycle, epoch: true, isLivePath: () => false });
  assert.equal(inspection.partial, false);
  const scope = makeScope();
  const options = { selector, confirm: inspection.sources[0]!.descriptor, holds, lifecycle, barePath: bare,
    isLivePath: () => false, quiesce: async () => true };
  const input = { decision: { eligible: true as const, attested: true as const }, scope, selector, barePath: bare,
    paths: [clonePath], confirm: options.confirm, expectedRawJournal: raw,
    beforeEffect: async () => {}, checkLive: () => {}, signal: new AbortController().signal, deadline: Date.now() + 60_000 };
  const skills = path.join(path.dirname(clonePath), `.uzi-skills-${path.basename(clonePath)}`);
  fs.mkdirSync(skills);
  fs.writeFileSync(path.join(skills, "payload.md"), "generated skill");
  return { fx, git, bare, reposRoot: path.dirname(bare), dataDir: fx.dataDir, clonePath, skills, scope, acl,
    selector, options, input, raw };
}

/** The production permission shape under uid split with umask 002: 2775 bare, 0775 repos root. */
function widen(e: Env, bareMode = 0o2775, reposMode = 0o775, dataMode = 0o755): void {
  fs.chmodSync(e.dataDir, dataMode);
  fs.chmodSync(e.reposRoot, reposMode);
  fs.chmodSync(e.bare, bareMode);
}

async function prove(e: Env) {
  return e.git.proveEpochWorktreeContent({ scope: makeScope(), barePath: e.bare, selector: e.selector,
    clonePath: e.clonePath, signal: new AbortController().signal, deadline: Date.now() + 60_000 });
}
const discard = (e: Env) => e.git.discardStoppedSource({ ...e.options, epochProof: e.git.createEpochDiscardProof(e.input) });

async function assertAdmitted(e: Env): Promise<void> {
  assert.deepEqual({ proven: (await prove(e)).proven }, { proven: true });
}
async function assertDiscarded(e: Env): Promise<void> {
  assert.equal((await discard(e)).kind, "done");
  assert.equal(fs.existsSync(e.clonePath), false);
  assert.equal(fs.existsSync(e.skills), false);
}
/** The proof reports UNKNOWN, and the discard takes nothing over: no clone or skills deletion, journal intact. */
async function assertRefused(e: Env): Promise<void> {
  const verdict = await prove(e);
  assert.equal(verdict.proven, false);
  assert.equal(verdict.reason, "completeness_unknown", JSON.stringify(verdict));
  const result = await discard(e);
  assert.equal(result.kind, "unknown", JSON.stringify(result));
  assert.equal(e.scope.stopped(), true, "an UNKNOWN stops the epoch scope");
  assert.ok(fs.existsSync(e.clonePath), "the clone is kept");
  assert.ok(fs.existsSync(path.join(e.skills, "payload.md")), "the skills sibling is kept");
  assert.equal(bareGit(e.bare, "config", "--local", `uzi-recovery.${BRANCH}.clone`), e.raw);
}

for (const shape of ["alternate", "standalone"] as const) {
  describe(`a group-writable managed bare, clone ${shape === "alternate" ? "with" : "without"} an alternate`, () => {
    for (const mode of [0o775, 0o2775]) {
      it(`${mode.toString(8)} is proven under an issued trust, and the observer sees the ancestors in every chunk`, async () => {
        const e = await setup({}, shape);
        widen(e, mode);
        await assertAdmitted(e);
        // Every full snapshot (the proof's observation and its re-checks) observes the bare tree afresh.
        assert.ok(e.acl.length >= 2, `full snapshots: ${e.acl.length}`);
        for (const chunk of e.acl) {
          assert.deepEqual(chunk.slice(0, 2), [e.reposRoot, e.dataDir]);
          assert.ok(chunk.includes(e.bare) && chunk.includes(path.join(e.bare, "config")) && chunk.includes(path.join(e.bare, "HEAD")));
        }
      });
      it(`${mode.toString(8)} is discarded through the same proof`, async () => {
        const e = await setup({}, shape);
        widen(e, mode);
        await assertDiscarded(e);
      });
    }

    it("a non-group-writable bare keeps proving, 0755 and 0700, with or without trust and whatever its group", async () => {
      const foreign = foreignGid();
      for (const trust of [trustedForThisProcess(), undefined]) {
        for (const [mode, gid] of [[0o755, GID], [0o700, GID], [0o755, foreign], [0o700, foreign]] as const) {
          const e = await setup({ managedBareGroupTrust: trust }, shape);
          fs.chownSync(e.bare, UID, gid);
          fs.chmodSync(e.bare, mode);
          await assertAdmitted(e);
          assert.equal(e.acl.length, 0, "no ACL observation for a store that is not group-writable");
        }
      }
    });

    it("the default with no trust refuses a 2775 bare, as before", async () => {
      const e = await setup({ managedBareGroupTrust: undefined }, shape);
      widen(e);
      await assertRefused(e);
      assert.equal(e.acl.length, 0);
    });
  });
}

describe("refused group-writable shapes (the clone and skills are never touched)", () => {
  for (const clone of CLONE_SHAPES) {
    it(`a group-writable bare of a foreign group (${clone})`, async () => {
      const e = await setup({}, clone);
      widen(e);
      fs.chownSync(e.bare, UID, foreignGid());
      fs.chmodSync(e.bare, 0o2775);
      await assertRefused(e);
    });

    for (const target of ["reposRoot", "dataDir"] as const) {
      for (const shape of ["world-writable", "sticky world-writable", "group-writable foreign group"] as const) {
        it(`an unsafe ancestor: ${target} ${shape} (${clone})`, async () => {
          const e = await setup({}, clone);
          widen(e);
          const dir = target === "reposRoot" ? e.reposRoot : e.dataDir;
          if (shape === "group-writable foreign group") { fs.chownSync(dir, UID, foreignGid()); fs.chmodSync(dir, 0o775); }
          else fs.chmodSync(dir, shape === "world-writable" ? 0o777 : 0o1777);
          await assertRefused(e);
        });
      }
    }

    it(`an ancestor carrying a named ACL, observed through the ancestor paths of every chunk (${clone})`, async () => {
      const seen: string[][] = [];
      let repos = "";
      const e = await setup({ managedBareAclObserve: async list => {
        seen.push([...list]);
        if (list.includes(repos)) throw new Error("named ACL entry on the repos root");
      } }, clone);
      repos = e.reposRoot;
      widen(e);
      await assertRefused(e);
      assert.ok(seen.length >= 1);
      assert.deepEqual(seen[0]!.slice(0, 2), [e.reposRoot, e.dataDir]);
    });

    it(`a world-writable bare (${clone})`, async () => {
      for (const mode of [0o777, 0o2777, 0o1777, 0o772]) {
        const e = await setup({}, clone);
        widen(e, mode);
        await assertRefused(e);
      }
    });
  }

  it("a symlinked store", async () => {
    const e = await setup();
    widen(e);
    const real = e.bare + ".real";
    fs.renameSync(e.bare, real);
    fs.symlinkSync(real, e.bare);
    const verdict = await prove(e);
    assert.equal(verdict.proven, false);
    assert.ok(fs.existsSync(e.clonePath));
    const result = await discard(e);
    assert.notEqual(result.kind, "done");
    assert.ok(fs.existsSync(e.clonePath));
    assert.ok(fs.existsSync(path.join(e.skills, "payload.md")));
  });

  it("an unmanaged alternate", async () => {
    const e = await setup();
    widen(e);
    const other = path.join(path.dirname(e.dataDir), "unmanaged.git");
    execFileSync("git", ["init", "--bare", "-q", other], { env: ENV, stdio: "pipe" });
    fs.appendFileSync(path.join(e.clonePath, ".git", "objects", "info", "alternates"), path.join(other, "objects") + "\n");
    await assertRefused(e);
  });

  for (const clone of CLONE_SHAPES) {
    it(`an ACL observed on a path inside the store (${clone})`, async () => {
      const e = await setup({ managedBareAclObserve: async paths => {
        if (paths.some(p => p.endsWith(`${path.sep}HEAD`))) throw new Error("named ACL entry on HEAD");
      } }, clone);
      widen(e);
      await assertRefused(e);
    });
  }

  it("a disallowed filesystem type, on the bare or on any ancestor", async () => {
    for (const bad of ["bare", "reposRoot", "dataDir"] as const) {
      const asked: string[] = [];
      let target = "";
      const e = await setup({ managedBareStatfs: async p => { asked.push(p); return { type: p === target ? 0x6969 : EXT4 }; } });
      target = { bare: e.bare, reposRoot: e.reposRoot, dataDir: e.dataDir }[bad];
      widen(e);
      await assertRefused(e);
      // The first snapshot asks for the bare, then each ancestor, in that order, until the disallowed one.
      const order = [e.bare, e.reposRoot, e.dataDir];
      assert.deepEqual(asked.slice(0, order.indexOf(target) + 1), order.slice(0, order.indexOf(target) + 1));
    }
  });

  it("each allowed filesystem type is proven", async () => {
    for (const type of [0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x794c7630]) {
      const e = await setup({ managedBareStatfs: async () => ({ type }) });
      widen(e);
      await assertAdmitted(e);
    }
  });

  it("a real statfs of the test host is allowed or refuses cleanly", async () => {
    const e = await setup({ managedBareStatfs: undefined });
    widen(e);
    const type = Number(BigInt.asUintN(32, BigInt(fs.statfsSync(e.bare).type)));
    const allowed = [0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x794c7630].includes(type);
    if (allowed) await assertAdmitted(e); else await assertRefused(e);
  });

  describe("trust values that are not a probe's positive result", () => {
    const refusedWith = async (trust: ManagedBareGroupTrust | undefined, observe = true, clone: CloneShape = "alternate") => {
      const e = await setup({ managedBareGroupTrust: trust }, clone);
      widen(e);
      await assertRefused(e);
      if (observe) assert.equal(e.acl.length, 0);
    };
    for (const clone of CLONE_SHAPES) {
      it(`P1 contamination: the runner groups contain the worker group (${clone})`, async () => {
        const trust = managedBareGroupTrustForTest({ runnerStatus: runnerStatus("10002 10001"), commandStatus: commandStatus() });
        assert.equal(trust.state, "off");
        await refusedWith(trust, true, clone);
      });
      it(`a failed probe (${clone})`, async () => {
        const trust = managedBareGroupTrustForTest({ runnerStatus: "garbage", commandStatus: commandStatus() });
        assert.equal(trust.state, "off");
        await refusedWith(trust, true, clone);
      });
    }
    it("a literal that no probe issued", async () => {
      await refusedWith({ state: "trusted", gid: GID } as unknown as ManagedBareGroupTrust);
    });
    it("single-uid trust", async () => {
      const trust = managedBareGroupTrustForTest({ runnerStatus: runnerStatus(), commandStatus: commandStatus(), split: false });
      assert.equal(trust.state, "off");
      await refusedWith(trust);
    });
    it("trust for a group that is not this process's group", async () => {
      const trust = managedBareGroupTrustForTest({ runnerStatus: runnerStatus(), commandStatus: commandStatus(),
        anyIdentity: true, gid: GID + 1 });
      assert.equal(trust.state, "trusted");
      await refusedWith(trust);
    });
    it("the process group is re-read at every proof", async t => {
      const e = await setup();
      widen(e);
      await assertAdmitted(e);
      t.mock.method(process as { getgid: () => number }, "getgid", () => GID + 1);
      assert.equal((await prove(e)).reason, "completeness_unknown");
    });
  });

  describe("the bare tree behind the object directories", () => {
    it("a group-writable file of a foreign group", async () => {
      const e = await setup();
      widen(e);
      const file = path.join(e.bare, "description");
      fs.chownSync(file, UID, foreignGid());
      fs.chmodSync(file, 0o664);
      await assertRefused(e);
    });
    it("a world-writable ref directory", async () => {
      const e = await setup();
      widen(e);
      fs.chmodSync(path.join(e.bare, "refs"), 0o777);
      await assertRefused(e);
    });
    it("a symlink inside the bare", async () => {
      const e = await setup();
      widen(e);
      fs.symlinkSync("HEAD", path.join(e.bare, "alias"));
      await assertRefused(e);
    });
    it("a FIFO inside the bare", async () => {
      const e = await setup();
      widen(e);
      execFileSync("mkfifo", [path.join(e.bare, "pipe")]);
      await assertRefused(e);
    });
    it("a group-writable file of the trusted group is fine", async () => {
      const e = await setup();
      widen(e);
      fs.chmodSync(path.join(e.bare, "description"), 0o664);
      await assertAdmitted(e);
    });
    it("a bare tree past the entry limit is UNKNOWN, never a keep", async () => {
      const e = await setup();
      widen(e);
      const dir = path.join(e.bare, "hooks");
      for (let i = 0; i < 40; i++) fs.writeFileSync(path.join(dir, `extra-${i}`), "");
      const proof = e.git.createEpochDiscardProof(e.input);
      const state = e.git["epochDiscardProofs"].get(proof)!;
      await assert.rejects(e.git["epochRetained"].run(e.git["epochLimits"]({ maxDirectoryEntries: 30 }), () =>
        e.git["withEpochNativeReads"](proof, () => e.git["automaticContentPreserved"](state))),
      error => (error as Error).constructor.name === "AutomaticCompletenessUnknown");
    });
  });

  describe("core.sharedRepository", () => {
    for (const clone of CLONE_SHAPES) {
      it(`is refused on a group-writable store, however it is spelled (${clone})`, async () => {
        for (const line of ["[core]\n\tSharedRepository = group\n", "[CORE]\n\tsharedrepository = 0660\n"]) {
          const e = await setup({}, clone);
          fs.appendFileSync(path.join(e.bare, "config"), line);
          widen(e);
          await assertRefused(e);
        }
      });
    }
    it("is still proven on a store that is not group-writable", async () => {
      const e = await setup();
      fs.appendFileSync(path.join(e.bare, "config"), "[core]\n\tSharedRepository = group\n");
      fs.chmodSync(e.bare, 0o755);
      await assertAdmitted(e);
    });
  });
});

describe("a store that changes after the first observation", () => {
  it("an ACL that appears for the final re-check refuses the proof", async () => {
    let calls = 0;
    const e = await setup({ managedBareAclObserve: async () => { if (++calls >= 2) throw new Error("ACL appeared"); } });
    widen(e);
    await assertRefused(e);
    assert.ok(calls >= 2, "the first snapshot passed and the re-check observed again");
  });

  it("an ancestor opened up between the two snapshots refuses the proof", async () => {
    let calls = 0, repos = "";
    const e = await setup({ managedBareAclObserve: async () => { if (++calls === 1) fs.chmodSync(repos, 0o777); } });
    repos = e.reposRoot;
    widen(e);
    await assertRefused(e);
    assert.ok(calls >= 1);
  });

  it("one cache: proven, then the bare gains world write, then it is refused; restored, it proves again", async () => {
    const e = await setup();
    widen(e);
    await assertAdmitted(e);
    fs.chmodSync(e.bare, 0o2777);
    assert.equal((await prove(e)).reason, "completeness_unknown");
    fs.chmodSync(e.bare, 0o2775);
    await assertAdmitted(e);
    fs.chownSync(e.bare, UID, foreignGid());
    fs.chmodSync(e.bare, 0o2775);
    assert.equal((await prove(e)).reason, "completeness_unknown");
  });
});

describe("the getfacl launch", () => {
  for (const clone of CLONE_SHAPES) {
    it(`an absent executable is UNKNOWN through the audited shape: no deletion, no unhandled rejection (${clone})`, async () => {
      const rejections: unknown[] = [];
      const listener = (reason: unknown) => { rejections.push(reason); };
      process.on("unhandledRejection", listener);
      try {
        const absent = path.join(os.tmpdir(), `absent-getfacl-${process.pid}`);
        assert.equal(fs.existsSync(absent), false);
        const e = await setup({ managedBareAclObserve: undefined, managedBareGetfaclBin: absent }, clone);
        widen(e);
        // The census's proof, and the discard that consumes the same proof.
        await assertRefused(e);
        for (let i = 0; i < 3; i++) await new Promise<void>(resolve => setImmediate(resolve));
      } finally { process.off("unhandledRejection", listener); }
      assert.deepEqual(rejections, []);
    });
  }

  it("the production binary path is used when none is configured", async () => {
    const launched: string[] = [];
    const e = await setup({ managedBareAclObserve: undefined });
    widen(e);
    const internal = e.git as unknown as { execScoped: (command: string, ...rest: unknown[]) => Promise<unknown> };
    const exec = internal.execScoped.bind(e.git);
    internal.execScoped = async (command, ...rest) => {
      if (command.endsWith("getfacl")) { launched.push(command); return { stdout: "", stderr: "" }; }
      return exec(command, ...rest);
    };
    await assertAdmitted(e);
    assert.ok(launched.length >= 2 && launched.every(command => command === "/usr/bin/getfacl"), JSON.stringify(launched));
  });

  it("any stdout or stderr of the launch is UNKNOWN", async () => {
    for (const out of [{ stdout: "# file: x\nuser:foo:rwx\n", stderr: "" }, { stdout: "", stderr: "warning" }]) {
      const e = await setup({ managedBareAclObserve: undefined });
      widen(e);
      const internal = e.git as unknown as { execScoped: (command: string, ...rest: unknown[]) => Promise<unknown> };
      const exec = internal.execScoped.bind(e.git);
      internal.execScoped = async (command, ...rest) => command.endsWith("getfacl") ? out : exec(command, ...rest);
      await assertRefused(e);
    }
  });

  it("chunks the tree paths by 512 and repeats the ancestors in every chunk", async () => {
    const e = await setup();
    widen(e);
    for (let i = 0; i < 1100; i++) fs.writeFileSync(path.join(e.bare, "hooks", `extra-${i}`), "");
    await assertAdmitted(e);
    // 1100 extra files plus the bare's own tree: at least three chunks per snapshot, each full chunk 512 paths.
    assert.ok(e.acl.length >= 6, `chunks: ${e.acl.length}`);
    assert.ok(e.acl.filter(chunk => chunk.length === 2 + 512).length >= 4);
    for (const chunk of e.acl) {
      assert.deepEqual(chunk.slice(0, 2), [e.reposRoot, e.dataDir]);
      assert.ok(chunk.length <= 2 + 512);
    }
  });
});
