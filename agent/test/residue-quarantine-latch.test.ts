import { describe, it, afterEach, beforeEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ProcessQuiescence } from "../src/run-quiescence.js";
import {
  ResidueQuarantinedError,
  RunResidueBlockedError,
  assertResidueQuarantineOpen,
  latchOnUnattributedUnreadable,
  latchResidueQuarantine,
  residueQuarantine,
} from "../src/residue-quarantine.js";
import { classifyForgeError } from "../src/forge-retry.js";
import { failOriginForReason } from "../src/runner.js";
import { GitCache, gitEnv } from "../src/git.js";
import { defaultQueryFn } from "../src/sdk-messages.js";
import { spawnDetached } from "../src/sdk-spawn.js";
import { TickSpawner } from "../src/tick-spawner.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, recordingLogger, testGitCacheOptions } from "./helpers.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";

resetResidueQuarantineAfterEach();

// issue #2213 — the latch module and the funnels that consult it. Layer: residue-quarantine.ts plus
// the synchronous checks at the git env/spawn funnel (git.ts), the forge-retry classifier, and the
// two provider belts (defaultQueryFn, spawnDetached). The runner-, worker- and executor-level proofs
// are in the sibling residue-quarantine-*.test.ts files.

const UNREADABLE: ProcessQuiescence = {
  state: "unverified",
  processes: [{ pid: 9, uid: 1, comm: "x", cwd: "/", reason: "unreadable_unattributed" }],
  killed: [],
  detail: 'runner-uid pid 9 "x" could not be attributed (env/cwd unreadable)',
};

describe("the latch (issue #2213)", () => {
  it("is open until latched, then synchronously refuses with a typed error naming the cause", () => {
    assert.equal(residueQuarantine(), undefined);
    assert.doesNotThrow(() => assertResidueQuarantineOpen("git"));
    latchResidueQuarantine({ cause: "pid 9 unattributed", runId: "r1", site: "pre_clone" }, nullLogger(), new Date("2026-10-06T16:42:57Z"));
    assert.deepEqual(residueQuarantine(), {
      cause: "pid 9 unattributed",
      runId: "r1",
      site: "pre_clone",
      latchedAt: "2026-10-06T16:42:57.000Z",
    });
    for (const site of ["git", "provider_turn", "claim"] as const) {
      assert.throws(
        () => assertResidueQuarantineOpen(site),
        (err: unknown) => {
          assert.ok(err instanceof ResidueQuarantinedError);
          assert.equal(err.site, site);
          assert.equal(
            err.message,
            "worker_residue_blocked: this worker is quarantined (pid 9 unattributed); no credentialed git or provider turn may start until the worker restarts",
          );
          return true;
        },
      );
    }
  });

  it("the first latch wins; a later one is only logged, and the first logs once at error level", () => {
    const { logger, lines } = recordingLogger();
    latchResidueQuarantine({ cause: "first", runId: "r1", site: "pre_clone" }, logger);
    latchResidueQuarantine({ cause: "second", runId: "r2", site: "finalize" }, logger);
    assert.equal(residueQuarantine()?.cause, "first");
    assert.equal(residueQuarantine()?.runId, "r1");
    const errors = (lines as Array<{ level: string }>).filter((l) => l.level === "error");
    assert.equal(errors.length, 1, "logged once at error");
    assert.equal((lines as Array<{ level: string }>).filter((l) => l.level === "warn").length, 1, "the later one is logged, not stored");
  });

  it("sanitizes and caps the cause: no control or bidi character, at most 160 characters", () => {
    latchResidueQuarantine({ cause: `a\u001b[0m\u202eb\n${"x".repeat(400)}`, site: "t" }, nullLogger());
    const cause = residueQuarantine()?.cause ?? "";
    // eslint-disable-next-line no-control-regex
    assert.doesNotMatch(cause, /[\u0000-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/);
    assert.ok(cause.startsWith("a?[0m?b?"));
    assert.ok(cause.length <= 163, `capped (got ${cause.length})`);
  });

  it("the error is a RunResidueBlockedError and maps to fail_origin worker_residue_blocked", () => {
    latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
    let caught: unknown;
    try {
      assertResidueQuarantineOpen("claim");
    } catch (err) {
      caught = err;
    }
    assert.ok(caught instanceof RunResidueBlockedError);
    assert.equal(failOriginForReason((caught as Error).message), "worker_residue_blocked");
  });

  it("latchOnUnattributedUnreadable latches only an unreadable_unattributed verdict, on a single-uid Linux worker", { skip: process.platform !== "linux" }, () => {
    assert.equal(latchOnUnattributedUnreadable(undefined, { site: "s" }), false);
    assert.equal(
      latchOnUnattributedUnreadable(
        { ...UNREADABLE, processes: [{ ...UNREADABLE.processes[0]!, reason: "own_attempt:kill_unconfirmed" }] },
        { site: "s" },
      ),
      false,
    );
    assert.equal(latchOnUnattributedUnreadable({ ...UNREADABLE, processes: [{ ...UNREADABLE.processes[0]!, reason: "status_unreadable" }] }, { site: "s" }), false);
    assert.equal(residueQuarantine(), undefined);
    assert.equal(latchOnUnattributedUnreadable(UNREADABLE, { runId: "r9", site: "finalize", log: nullLogger() }), true);
    assert.deepEqual([residueQuarantine()?.site, residueQuarantine()?.runId, residueQuarantine()?.cause], ["finalize", "r9", UNREADABLE.detail]);
  });

  it("under an active uid split the same verdict does not latch (behaviour as today)", { skip: process.platform !== "linux" }, () => {
    const before = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    try {
      assert.equal(latchOnUnattributedUnreadable(UNREADABLE, { site: "finalize" }), false);
      assert.equal(residueQuarantine(), undefined);
    } finally {
      if (before === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = before;
    }
  });
});

describe("forge retry classification (issue #2213)", () => {
  it("a residue block is permanent, even when its text reads like a transient network error", () => {
    assert.equal(classifyForgeError(new RunResidueBlockedError("connection reset by peer 502")), "permanent");
    latchResidueQuarantine({ cause: "stream error: connection reset", site: "t" }, nullLogger());
    assert.equal(classifyForgeError(new ResidueQuarantinedError("git", "connection reset")), "permanent");
  });

  it("control: a transient network error is still transient", () => {
    assert.equal(classifyForgeError(new Error("fatal: unable to access: connection reset by peer")), "transient");
  });
});

describe("the git funnel (issue #2213)", () => {
  it("gitEnv(pat) throws while latched; gitEnv() does not; gitEnv(pat) is fine when open", () => {
    assert.doesNotThrow(() => gitEnv("a-forge-pat-value"));
    latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
    assert.throws(() => gitEnv("a-forge-pat-value"), ResidueQuarantinedError);
    const env = gitEnv();
    assert.ok(!Object.values(env).some((v) => typeof v === "string" && v.startsWith("Authorization: Basic")));
  });

  describe("with a real GitCache", () => {
    let fx: Fixture;
    let git: GitCache;
    let sleeps: number[];
    beforeEach((t) => {
      fx = makeFixture({}, { testName: t.fullName ?? t.name });
      sleeps = [];
      git = new GitCache(fx.dataDir, nullLogger(), { schedule: [1, 1, 1], sleep: async (ms) => void sleeps.push(ms) }, testGitCacheOptions());
    });
    afterEach(() => fx.cleanup());

    it("ensureClone with a PAT while latched makes one attempt: typed, unwrapped, never retried", async () => {
      latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
      await assert.rejects(git.ensureClone(fx.originPath, "a-forge-pat-value"), (err: unknown) => err instanceof ResidueQuarantinedError);
      assert.deepEqual(sleeps, [], "the retry schedule was never entered");
    });

    it("the execScoped check refuses a PAT-marked env and starts no child (the second line, independent of gitEnv's)", async () => {
      // Build the PAT env BEFORE latching, as a future path that held an env across an await would.
      const env = { ...gitEnv("a-forge-pat-value"), PATH: process.env.PATH };
      const marker = path.join(fx.dataDir, "child-ran");
      const script = path.join(fx.dataDir, "mark.sh");
      fs.writeFileSync(script, `#!/bin/sh\necho ran > '${marker}'\n`, { mode: 0o755 });
      const exec = (git as unknown as {
        execScoped(command: string, args: string[], options: { env: NodeJS.ProcessEnv }): Promise<unknown>;
      }).execScoped.bind(git);
      await exec(script, [], { env });
      assert.equal(fs.existsSync(marker), true, "control: unlatched, the child runs");
      fs.rmSync(marker);
      latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
      await assert.rejects(exec(script, [], { env }), ResidueQuarantinedError);
      assert.equal(fs.existsSync(marker), false, "no child was started");
      // A credential-free env is untouched by the latch (checkpoint ticks, bare reads, archival).
      await exec(script, [], { env: { ...gitEnv(), PATH: process.env.PATH } });
      assert.equal(fs.existsSync(marker), true, "credential-free git still runs");
    });

    it("the Codex boundary path: no boundary spawn is invoked for a PAT-marked env while latched", async () => {
      const env = { ...gitEnv("a-forge-pat-value"), PATH: process.env.PATH };
      const spawned: unknown[] = [];
      const exec = (git as unknown as {
        execScoped(command: string, args: string[], options: { env: NodeJS.ProcessEnv }): Promise<unknown>;
      }).execScoped.bind(git);
      latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
      const ac = new AbortController();
      await git.withBoundaryProcessSpawner(
        async (req) => {
          spawned.push(req);
          throw new Error("must not spawn");
        },
        ac.signal,
        async () => {
          await assert.rejects(exec("git", ["--version"], { env }), ResidueQuarantinedError);
        },
      );
      assert.deepEqual(spawned, [], "no supervisor/boundary spawn was requested");
    });
  });
});

describe("the Claude provider belts (issue #2213)", () => {
  it("defaultQueryFn throws before the SDK query is created", () => {
    latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
    assert.throws(() => defaultQueryFn({ prompt: (async function* () {})(), options: {} }), ResidueQuarantinedError);
  });

  it("spawnDetached refuses a spawn whose env carries a provider credential, and spawns nothing", () => {
    latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
    const marker = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "uzi-2213-spawn-")), "ran");
    try {
      for (const key of ["CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"]) {
        assert.throws(
          () =>
            spawnDetached({
              command: "/bin/sh",
              args: ["-c", `echo ran > '${marker}'`],
              cwd: "/",
              env: { PATH: "/usr/bin:/bin", [key]: "a-provider-credential-value" },
              signal: new AbortController().signal,
            }),
          ResidueQuarantinedError,
          key,
        );
      }
      assert.equal(fs.existsSync(marker), false);
    } finally {
      fs.rmSync(path.dirname(marker), { recursive: true, force: true });
    }
  });

  it("control: while open the same spawn starts; and a credential-free spawn is not a turn", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-2213-spawn-"));
    const marker = path.join(dir, "ran");
    try {
      const child = spawnDetached({
        command: "/bin/sh",
        args: ["-c", `echo ran > '${marker}'`],
        cwd: "/",
        env: { PATH: "/usr/bin:/bin", CLAUDE_CODE_OAUTH_TOKEN: "a-provider-credential-value" },
        signal: new AbortController().signal,
      });
      await new Promise((r) => child.once("exit", r));
      assert.equal(fs.existsSync(marker), true);
      fs.rmSync(marker);
      latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
      const free = spawnDetached({
        command: "/bin/sh",
        args: ["-c", `echo ran > '${marker}'`],
        cwd: "/",
        env: { PATH: "/usr/bin:/bin" },
        signal: new AbortController().signal,
      });
      await new Promise((r) => free.once("exit", r));
      assert.equal(fs.existsSync(marker), true, "a credential-free spawn is untouched");
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});

describe("the mid-turn tick spawner (issue #2213)", () => {
  const credentialedEnv = { PATH: "/usr/bin:/bin", GIT_CONFIG_VALUE_0: "Authorization: Basic ZmFrZTpmYWtl" };

  it("a latch set while the lock snapshot awaits spawns no credentialed child", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-2213-tick-"));
    try {
      const bare = path.join(dir, "bare.git");
      fs.mkdirSync(path.join(bare, "refs", "uzi-runner", "agent"), { recursive: true });
      fs.mkdirSync(path.join(bare, "objects", "info"), { recursive: true });
      const marker = path.join(dir, "ran");
      const ac = new AbortController();
      const sp = new TickSpawner({ signal: ac.signal, barePath: bare, branch: "agent/issue-1" });
      const pending = sp.spawn({ argv: ["/bin/sh", "-c", `echo ran > '${marker}'`], cwd: dir, env: credentialedEnv, identity: "worker_pat" });
      // spawnWith is suspended in its snapshotLocks await: the latch lands inside that window.
      latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
      await assert.rejects(pending, ResidueQuarantinedError);
      await new Promise((r) => setTimeout(r, 100));
      assert.equal(fs.existsSync(marker), false, "no child was spawned");
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });

  it("control: a credential-free tick child still starts while latched", async () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-2213-tick-"));
    try {
      latchResidueQuarantine({ cause: "c", site: "t" }, nullLogger());
      const sp = new TickSpawner({ signal: new AbortController().signal });
      const h = await sp.spawn({ argv: ["/bin/true"], cwd: dir, env: { PATH: "/usr/bin:/bin" }, identity: "worker_pat" });
      assert.deepEqual(await h.completed, { code: 0 });
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});
