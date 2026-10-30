import { it } from "node:test";
import assert from "node:assert/strict";
import { StubExecutor } from "../src/executor.js";
import { FORGE_RETRY_SCHEDULE, PublicationForgeUnreachableError } from "../src/forge-retry.js";
import { RunRunner } from "../src/runner.js";
import { api, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";

// A transient forge outage that outlasts the whole retry schedule at a finalize publication hop must
// surface as the typed PublicationForgeUnreachableError through the single runner hook, never as a
// fallback or an untyped failure. The production retry waits real seconds; this file collapses only the
// exact FORGE_RETRY_SCHEDULE delays so no test waits in real time.

installHarness();

type Hooked = { surface: string; step: string; message: string; raw: string | undefined; code: string };

function collapseRetryDelays(): () => void {
  const real = globalThis.setTimeout;
  const delays = new Set(FORGE_RETRY_SCHEDULE);
  globalThis.setTimeout = ((fn: (...a: unknown[]) => void, ms?: number, ...rest: unknown[]) =>
    real(fn, ms !== undefined && delays.has(ms) ? 0 : ms, ...rest)) as typeof setTimeout;
  return () => { globalThis.setTimeout = real; };
}

function hookRecorder(t: { mock: { method: (o: object, n: string, f: (...a: never[]) => unknown) => unknown } }, r: RunRunner): Hooked[] {
  const seen: Hooked[] = [];
  const proto = Object.getPrototypeOf(r) as { onPublicationForgeUnreachable: (e: PublicationForgeUnreachableError) => Promise<never> };
  const original = proto.onPublicationForgeUnreachable;
  t.mock.method(r, "onPublicationForgeUnreachable", (async (e: PublicationForgeUnreachableError) => {
    seen.push({ surface: e.surface, step: e.step, message: e.message, raw: e.rawDetail, code: e.code });
    return original.call(r, e);
  }) as never);
  return seen;
}

const failedReason = (runId: string) =>
  api.states.find(s => s.runId === runId && s.body.status === "failed")?.body.failure_reason ?? "";

type Seam = { runGit(cwd: string, args: string[], ...rest: unknown[]): Promise<string> };

it("floor_unreachable retries with a full proof per attempt, then exhausts as a typed git/floor_refresh error", async t => {
  const restore = collapseRetryDelays();
  try {
    const claim = gitlabClaim(4101);
    const seam = git as unknown as Seam;
    const real = seam.runGit.bind(git);
    let listings = 0;
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "ls-remote") {
        listings++;
        throw new Error("git ls-remote -- origin failed: fatal: unable to access: Failed to connect to forge.test port 443: Connection refused");
      }
      return real(cwd, args, ...rest);
    };
    let preflights = 0;
    const preflight = git.scratchPublicationPreflight.bind(git);
    git.scratchPublicationPreflight = async (...args) => { preflights++; return preflight(...args); };
    const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
    const seen = hookRecorder(t, r);
    await r.execute(claim);

    const attempts = FORGE_RETRY_SCHEDULE.length + 1;
    assert.equal(listings, attempts, "every attempt reran the remote floor listing");
    assert.equal(preflights, attempts + 1, "one frozen-candidate preflight plus one per pushBranch attempt");
    assert.equal(seen.length, 1);
    assert.deepEqual([seen[0]!.surface, seen[0]!.step, seen[0]!.code], ["git", "floor_refresh", "publication_forge_unreachable"]);
    assert.equal(seen[0]!.message, "publication_forge_unreachable: floor_refresh");
    assert.ok(!seen[0]!.message.includes("forge.test"), "public message carries no remote text");
    assert.match(failedReason(claim.run_id), /publication_forge_unreachable: floor_refresh/);
  } finally { restore(); }
});

it("a transient raw push failure exhausts as a typed git/push error", async t => {
  const restore = collapseRetryDelays();
  try {
    const claim = gitlabClaim(4102);
    const seam = git as unknown as Seam;
    const real = seam.runGit.bind(git);
    let pushes = 0;
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "push") { pushes++; throw new Error("git push failed: fatal: unable to access: connection reset by peer"); }
      return real(cwd, args, ...rest);
    };
    const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
    const seen = hookRecorder(t, r);
    await r.execute(claim);
    assert.equal(pushes, FORGE_RETRY_SCHEDULE.length + 1);
    assert.deepEqual(seen.map(s => [s.surface, s.step]), [["git", "push"]]);
    assert.match(failedReason(claim.run_id), /publication_forge_unreachable: push/);
  } finally { restore(); }
});

it("a permanent push rejection keeps today's behaviour and is not typed", async t => {
  const claim = gitlabClaim(4103);
  const seam = git as unknown as Seam;
  const real = seam.runGit.bind(git);
  let pushes = 0;
  seam.runGit = async (cwd, args, ...rest) => {
    if (args[0] === "push") { pushes++; throw new Error("git push failed: remote: GitLab: You are not allowed to push code to protected branches"); }
    return real(cwd, args, ...rest);
  };
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(pushes, 1);
  assert.equal(seen.length, 0);
  assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
});

it("MR create exhausting on a transient API failure is a typed api/mr_create error", async t => {
  const restore = collapseRetryDelays();
  try {
    const claim = gitlabClaim(4104);
    let creates = 0;
    const forge = fakeGitlab({
      intercept: (req) => {
        if (req.method !== "POST") return undefined;
        creates++;
        return { status: 503, text: async () => "upstream-body-marker" };
      },
    });
    const r = runner(new StubExecutor(nullLogger()), forge.gitlab);
    const seen = hookRecorder(t, r);
    await r.execute(claim);
    assert.equal(creates, FORGE_RETRY_SCHEDULE.length + 1);
    assert.deepEqual(seen.map(s => [s.surface, s.step, s.message]), [["api", "mr_create", "publication_forge_unreachable: mr_create"]]);
    assert.ok(!seen[0]!.message.includes("upstream-body-marker"));
    assert.match(failedReason(claim.run_id), /publication_forge_unreachable: mr_create/);
  } finally { restore(); }
});

it("a permanent MR create rejection (401) is not retried and not typed", async t => {
  const claim = gitlabClaim(4105);
  let creates = 0;
  const forge = fakeGitlab({
    intercept: (req) => {
      if (req.method !== "POST") return undefined;
      creates++;
      return { status: 401, text: async () => "unauthorized" };
    },
  });
  const r = runner(new StubExecutor(nullLogger()), forge.gitlab);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(creates, 1);
  assert.equal(seen.length, 0);
  assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
});

