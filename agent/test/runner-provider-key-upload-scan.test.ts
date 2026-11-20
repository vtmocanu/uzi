import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimProviderBinding } from "../src/protocol.js";
import { LimitReachedError } from "../src/limit.js";
import { providerKeyForms } from "../src/redact.js";
import { api, client, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, assertCustodyRetained, commit, failingWork, heldServer, skipCheckpoints,
  type Work,
} from "./runner-held-publication-fixture.js";

installHarness();

// Owner decision, issue #4505: a provider-bound run carries the gateway key in the agent's Bash env, and
// gitleaks cannot detect a prefixless key. Every upload path therefore scans the exact objects it would
// carry for every encoded form of the key BEFORE any network write, and fails closed when it cannot scan.
// A vendor run (no provider key) does no extra git work and uploads exactly as before. The key is a
// synthetic prefixless value assembled at runtime; it lives only in throwaway fixture repos.

const KEY = ["Zq9x", "Lm/4", "tR+7", "&kQ2", "<w8>", "vN3d"].join("");
const FORMS = providerKeyForms(KEY);
const JSON_SLASH = FORMS.find((f) => f.includes("\\/"))!;
const PERCENT = encodeURIComponent(KEY);

const binding = (): ClaimProviderBinding => ({
  provider_id: "p-1", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/a",
  auth_style: "bearer", credential: KEY, model_map: { haiku: { model: "corp-haiku" } },
});
const bound = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "", provider: binding() });
const vendor = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000" });

let iid = 45050;

function spyPublish(): { uploads: number } {
  const seen = { uploads: 0 };
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk.length);
    seen.uploads++;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return seen;
}

function assertNoKeyForm(label: string, value: unknown): void {
  const text = JSON.stringify(value);
  for (const form of FORMS) assert.ok(!text.includes(form), `${label}: no encoded form of the key appears`);
}

/** A run whose executor commits `body` into a file, checkpoints once, and keeps going. */
async function checkpointRun(secrets: object, body: string | null, opts: { reap?: boolean } = {}) {
  const seen = spyPublish();
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(iid++, { secrets: secrets as never });
  let continued = false;
  const exec: Executor = {
    run: async (ctx) => {
      commit(ctx.worktreePath, "notes.txt", body === null ? "ordinary notes\n" : `leaked: ${body}\n`);
      await ctx.checkpoint!({ reap: opts.reap === true });
      continued = true;
      return { branch: ctx.branch };
    },
  };
  await runner(exec, gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 }).execute(claim);
  return { seen, claim, continued: () => continued };
}

describe("provider key scan: checkpoint publication (tick, milestone, iteration)", () => {
  for (const [label, form] of [["raw", KEY], ["JSON-escaped slash", JSON_SLASH], ["percent-encoded", PERCENT]] as const) {
    it(`a provider-bound run does not upload a checkpoint carrying the ${label} form, and keeps running`, async () => {
      const { seen, continued, claim } = await checkpointRun(bound(), form);
      assert.equal(seen.uploads, 0, "ZERO checkpoint uploads");
      assert.equal(continued(), true, "the skip never stops the executor");
      assertNoKeyForm("run feed", api.messages(claim.run_id));
    });
  }

  it("a vendor run with the very same commit uploads as before", async () => {
    const { seen } = await checkpointRun(vendor(), KEY);
    assert.equal(seen.uploads, 1);
  });

  it("a provider-bound run with a clean commit still uploads", async () => {
    const { seen } = await checkpointRun(bound(), null);
    assert.equal(seen.uploads, 1);
  });

  it("the upload seam alone blocks (the pre-publish scan is bypassed): no upload", async (t) => {
    const real = git.secretScanCheckpointRange.bind(git);
    t.mock.method(git, "secretScanCheckpointRange", (b: string, r: never, o: { deadlineMs?: number }) =>
      real(b, r, { ...o, literals: [] }));
    const { seen } = await checkpointRun(bound(), KEY);
    assert.equal(seen.uploads, 0, "the guard at the pack seam refuses on its own");
  });

  it("a done checkpoint (reap) is also refused", async () => {
    const { seen } = await checkpointRun(bound(), KEY, { reap: true });
    assert.equal(seen.uploads, 0);
  });

  it("FAILS CLOSED: a scanner that throws, or cannot establish a clean verdict, uploads nothing", async (t) => {
    t.mock.method(git, "scanObjectsForLiterals", async () => { throw new Error("scanner exploded"); });
    assert.equal((await checkpointRun(bound(), null)).seen.uploads, 0, "throwing scanner");
    t.mock.restoreAll();
    t.mock.method(git, "scanObjectsForLiterals", async () => ({ trusted: false as const, reason: "read_failed" }));
    assert.equal((await checkpointRun(bound(), null)).seen.uploads, 0, "untrusted scanner");
    t.mock.restoreAll();
    // The same broken scanner never runs for a vendor run, which uploads.
    t.mock.method(git, "scanObjectsForLiterals", async () => { throw new Error("scanner exploded"); });
    assert.equal((await checkpointRun(vendor(), null)).seen.uploads, 1, "vendor run");
  });

  it("a vendor run does no key-scan git work at all", async (t) => {
    const probe = t.mock.method(git, "scanObjectsForLiterals");
    await checkpointRun(vendor(), null);
    assert.equal(probe.mock.callCount(), 0);
  });
});

function parkRun(secrets: object, body: string) {
  const seen = spyPublish();
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-pk-park-"));
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(iid++, { wait_on_limit: true, secrets: secrets as never });
  const exec: Executor = {
    run: async (ctx: RunContext) => {
      commit(ctx.worktreePath, "notes.txt", `leaked: ${body}\n`);
      fs.mkdirSync(path.join(homeRoot, ctx.runId), { recursive: true });
      throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
    },
  };
  const done = runnerWith((runId) => ({ homeDir: path.join(homeRoot, runId), executor: exec }), gitlab).execute(claim)
    .finally(() => fs.rmSync(homeRoot, { recursive: true, force: true }));
  return { seen, claim, done };
}

describe("provider key scan: park and shutdown uploads", () => {
  it("a limit park does not upload a checkpoint carrying the key; the run still parks", async () => {
    const { seen, claim, done } = parkRun(bound(), KEY);
    await done;
    assert.equal(seen.uploads, 0, "ZERO park uploads");
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "limit_wait"), "the run parked");
  });

  it("a vendor park with the same commit uploads", async () => {
    const { seen, done } = parkRun(vendor(), KEY);
    await done;
    assert.equal(seen.uploads, 1);
  });

  it("a graceful shutdown does not upload a checkpoint carrying the key", async () => {
    const seen = spyPublish();
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: bound() as never });
    let ready!: () => void;
    const started = new Promise<void>((resolve) => { ready = resolve; });
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, "notes.txt", `leaked: ${KEY}\n`);
        ready();
        await new Promise<void>((resolve) => ctx.signal!.addEventListener("abort", () => resolve(), { once: true }));
        throw new Error("worker shutdown");
      },
    };
    const r = runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, { checkpointTickIntervalMs: 0 });
    const execution = r.execute(claim);
    try {
      await Promise.race([started, execution.then(() => { throw new Error("run ended before readiness"); })]);
      r.shutdown();
      await execution;
      assert.equal(seen.uploads, 0, "ZERO shutdown uploads");
    } finally {
      r.shutdown();
      await execution;
    }
  });
});

describe("provider key scan: the final branch push (every forge)", () => {
  const branchOnOrigin = (iidN: number): boolean => {
    try {
      execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", "--quiet", `refs/heads/agent/issue-${iidN}`], { stdio: "pipe" });
      return true;
    } catch { return false; }
  };
  async function finalize(secrets: object, body: string) {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: secrets as never });
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, "notes.txt", `leaked: ${body}\n`);
        return { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab, JOIN_TOKEN).execute(claim);
    return claim;
  }
  const failed = (runId: string) => api.states.find((s) => s.runId === runId && s.body.status === "failed")?.body;

  for (const [label, form] of [["raw", KEY], ["JSON-escaped slash", JSON_SLASH], ["percent-encoded", PERCENT]] as const) {
    it(`a provider-bound GitLab run that committed the ${label} form fails push_secret_blocked and pushes nothing`, async () => {
      const claim = await finalize(bound(), form);
      assert.equal(branchOnOrigin(claim.issue_iid!), false, "the branch never reached origin");
      const body = failed(claim.run_id);
      assert.ok(body, "the run failed");
      assert.equal(body.fail_origin, "push_secret_blocked");
      assert.equal(body.preserved_patch, undefined);
      assertNoKeyForm("terminal report", body);
      assertNoKeyForm("run feed", api.messages(claim.run_id));
    });
  }

  it("the push seam alone blocks (the pre-push scans are bypassed), with a fixed typed refusal", async (t) => {
    const real = git.secretScanCheckpointRange.bind(git);
    t.mock.method(git, "secretScanCheckpointRange", (b: string, r: never, o: { deadlineMs?: number }) =>
      real(b, r, { ...o, literals: [] }));
    const claim = await finalize(bound(), KEY);
    assert.equal(branchOnOrigin(claim.issue_iid!), false, "ZERO pushes");
    const body = failed(claim.run_id);
    assert.equal(body?.fail_origin, "push_secret_blocked");
    assert.match(String(body?.failure_reason), /provider key/);
    assertNoKeyForm("terminal report", body);
    assertNoKeyForm("run feed", api.messages(claim.run_id));
  });

  it("FAILS CLOSED when the push-seam scanner throws: nothing is pushed", async (t) => {
    const real = git.secretScanCheckpointRange.bind(git);
    t.mock.method(git, "secretScanCheckpointRange", (b: string, r: never, o: { deadlineMs?: number }) =>
      real(b, r, { ...o, literals: [] }));
    t.mock.method(git, "scanObjectsForLiterals", async () => { throw new Error("scanner exploded"); });
    const claim = await finalize(bound(), "nothing secret here");
    assert.equal(branchOnOrigin(claim.issue_iid!), false);
    assert.equal(failed(claim.run_id)?.fail_origin, "push_secret_blocked");
  });

  it("a vendor run with the same commit pushes as before", async () => {
    const claim = await finalize(vendor(), KEY);
    assert.equal(branchOnOrigin(claim.issue_iid!), true);
  });

  it("a provider-bound run with a clean commit pushes", async () => {
    const claim = await finalize(bound(), "ordinary notes");
    assert.equal(branchOnOrigin(claim.issue_iid!), true);
  });
});

const headHasKey = (worktree: string, form: string): boolean =>
  execFileSync("git", ["-C", worktree, "show", "HEAD:notes.txt"], { stdio: "pipe" }).toString().includes(form);

describe("provider key scan: pause checkpoint upload", () => {
  async function pause(secrets: object, body: string) {
    const seen = spyPublish();
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: secrets as never });
    const out: { parked?: boolean; localKeptKey?: boolean } = {};
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, "notes.txt", `leaked: ${body}\n`);
        await ctx.checkpoint!({ reap: false });
        const at = { completedCount: 1, total: 3 };
        out.parked = await ctx.parkForPause!(at);
        out.localKeptKey = headHasKey(ctx.worktreePath, body);
        return out.parked ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab, JOIN_TOKEN, { checkpointIntervalMs: 0 }).execute(claim);
    return { seen, claim, out };
  }
  const statuses = (runId: string) => api.states.filter((s) => s.runId === runId).map((s) => s.body.status);

  for (const [label, form] of [["raw", KEY], ["JSON-escaped slash", JSON_SLASH]] as const) {
    it(`a provider-bound pause whose range carries the ${label} form uploads nothing, does not park, and keeps the work`, async () => {
      const { seen, claim, out } = await pause(bound(), form);
      assert.equal(seen.uploads, 0, "ZERO pause uploads");
      assert.equal(out.parked, false, "no durable checkpoint, so no park (pause_failed: the run continues)");
      assert.ok(!statuses(claim.run_id).includes("paused"), "never reported paused");
      assert.equal(out.localKeptKey, true, "the committed work stays local, untouched");
      assertNoKeyForm("run feed", api.messages(claim.run_id));
    });
  }

  it("a vendor pause with the very same commit uploads and parks as before", async () => {
    const { seen, claim, out } = await pause(vendor(), KEY);
    assert.equal(seen.uploads, 1);
    assert.equal(out.parked, true);
    assert.ok(statuses(claim.run_id).includes("paused"));
  });
});

describe("provider key scan: overlay (GitHub milestone) checkpoint upload", () => {
  const githubRepo = () => ({ id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" as const });
  async function overlayMilestone(t: TestContext, secrets: object, body: string) {
    const seen = spyPublish();
    const { github } = fakeGitHub();
    const { gitlab } = fakeGitlab();
    const overlays: unknown[] = [];
    const realPack = git.checkpointPack.bind(git);
    t.mock.method(git, "checkpointPack", ((...args: Parameters<typeof realPack>) => {
      overlays.push(args[2]);
      return realPack(...args);
    }) as typeof realPack);
    // The overlay's default-tip fetch is credentialed; stubbing it makes the overlay fail soft to the real tip.
    t.mock.method(git, "fetchDefaultTip", async () => { throw new Error("stub: no network"); });
    const claim = gitlabClaim(iid++, { secrets: secrets as never, repo: githubRepo() });
    let continued = false;
    const exec: Executor = {
      run: async (ctx) => {
        commit(ctx.worktreePath, "notes.txt", `leaked: ${body}\n`);
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        continued = true;
        return { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab, JOIN_TOKEN, { github, checkpointIntervalMs: 1000 }).execute(claim);
    return { seen, claim, overlays, continued: () => continued };
  }

  it("a provider-bound overlay publish carrying the key uploads nothing (the overlay path skips the pre-publish scan)", async (t) => {
    const { seen, overlays, continued, claim } = await overlayMilestone(t, bound(), JSON_SLASH);
    assert.ok(overlays.length > 0, "the publish went through checkpointPack");
    assert.ok(overlays.every((o) => o !== undefined), "every pack was built with an overlay context");
    assert.equal(seen.uploads, 0, "ZERO overlay uploads");
    assert.equal(continued(), true, "the skip never stops the executor");
    assertNoKeyForm("run feed", api.messages(claim.run_id));
  });

  it("a vendor overlay publish with the same commit uploads as before", async (t) => {
    const { seen, overlays } = await overlayMilestone(t, vendor(), JSON_SLASH);
    assert.ok(overlays.length > 0 && overlays.every((o) => o !== undefined));
    assert.equal(seen.uploads, 1);
  });
});

describe("provider key scan: held publication", () => {
  async function held(secrets: object, body: string) {
    const claim = gitlabClaim(iid++, { claim_generation: 50, inventory_guarded: true, secrets: secrets as never });
    const { gitlab } = fakeGitlab();
    const server = heldServer(claim);
    skipCheckpoints();
    const work: Partial<Work> = {};
    const r = runner(failingWork(work, (ctx) => { commit(ctx.worktreePath, "leak.txt", `leaked: ${body}\n`); }), gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1 });
    await r.execute(claim);
    return { claim, server, work };
  }

  it("a provider-bound run's held range carrying the key is not uploaded and the work stays archived", { skip: HELD_LINUX_ONLY }, async () => {
    const { claim, server, work } = await held(bound(), JSON_SLASH);
    assert.equal(server.heldCalls.length, 0, "ZERO held uploads");
    assert.equal(server.archiveUploads(), 1, "the work is archived instead");
    await assertCustodyRetained(server, claim, work, "provider key in held range");
  });

  it("a vendor run with the same commit is published held, as before", { skip: HELD_LINUX_ONLY }, async () => {
    const { server } = await held(vendor(), JSON_SLASH);
    assert.equal(server.heldCalls.length, 1);
  });
});
