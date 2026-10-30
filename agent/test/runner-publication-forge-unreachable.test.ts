import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { it } from "node:test";
import assert from "node:assert/strict";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { FORGE_RETRY_SCHEDULE, PublicationForgeUnreachableError } from "../src/forge-retry.js";
import { RunRunner } from "../src/runner.js";
import type { Logger } from "../src/log.js";
import { api, client, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";

// A transient forge outage that outlasts the whole retry schedule at a finalize publication hop must
// surface as the typed PublicationForgeUnreachableError through the single runner hook, never as a
// fallback or an untyped failure. The production retry waits real seconds; this file injects the
// runner's private `forgeRetrySleep` seam so no test waits in real time.

installHarness();

type Hooked = { surface: string; step: string; message: string; raw: string | undefined; code: string };

/** Replace the real backoff wait with an instant one on this runner. */
function instantRetries(r: RunRunner): void {
  (r as unknown as { forgeRetrySleep: (ms: number) => Promise<void> }).forgeRetrySleep = async () => {};
}

function hookRecorder(t: { mock: { method: (o: object, n: string, f: (...a: never[]) => unknown) => unknown } }, r: RunRunner): Hooked[] {
  const seen: Hooked[] = [];
  const proto = Object.getPrototypeOf(r) as { onPublicationForgeUnreachable: (runId: string, e: PublicationForgeUnreachableError) => Promise<never> };
  const original = proto.onPublicationForgeUnreachable;
  t.mock.method(r, "onPublicationForgeUnreachable", (async (runId: string, e: PublicationForgeUnreachableError) => {
    seen.push({ surface: e.surface, step: e.step, message: e.message, raw: e.rawDetail, code: e.code });
    return original.call(r, runId, e);
  }) as never);
  return seen;
}

const failedReason = (runId: string) =>
  api.states.find(s => s.runId === runId && s.body.status === "failed")?.body.failure_reason ?? "";

type Seam = { runGit(cwd: string, args: string[], ...rest: unknown[]): Promise<string> };

const ATTEMPTS = FORGE_RETRY_SCHEDULE.length + 1;

function countPreflights(): () => number {
  let preflights = 0;
  const preflight = git.scratchPublicationPreflight.bind(git);
  git.scratchPublicationPreflight = async (...args) => { preflights++; return preflight(...args); };
  return () => preflights;
}

it("floor_unreachable retries with a full proof per attempt, then exhausts as a typed git/floor_refresh error", async t => {
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
  const preflights = countPreflights();
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);

  assert.equal(listings, ATTEMPTS, "every attempt reran the remote floor listing");
  assert.equal(preflights(), ATTEMPTS + 1, "one frozen-candidate preflight plus one per pushBranch attempt");
  assert.equal(seen.length, 1);
  assert.deepEqual([seen[0]!.surface, seen[0]!.step, seen[0]!.code], ["git", "floor_refresh", "publication_forge_unreachable"]);
  assert.equal(seen[0]!.message, "publication_forge_unreachable: floor_refresh");
  assert.ok(!seen[0]!.message.includes("forge.test"), "public message carries no remote text");
  assert.match(failedReason(claim.run_id), /publication_forge_unreachable: floor_refresh/);
});

it("a transient raw push failure reruns the proof per attempt and exhausts as a typed git/push error", async t => {
  const claim = gitlabClaim(4102);
  const seam = git as unknown as Seam;
  const real = seam.runGit.bind(git);
  let pushes = 0;
  seam.runGit = async (cwd, args, ...rest) => {
    if (args[0] === "push") { pushes++; throw new Error("git push failed: fatal: unable to access: connection reset by peer"); }
    return real(cwd, args, ...rest);
  };
  const preflights = countPreflights();
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(pushes, ATTEMPTS);
  assert.equal(preflights(), ATTEMPTS + 1, "one frozen-candidate preflight plus one per pushBranch attempt");
  assert.deepEqual(seen.map(s => [s.surface, s.step]), [["git", "push"]]);
  assert.match(failedReason(claim.run_id), /publication_forge_unreachable: push/);
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
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(pushes, 1);
  assert.equal(seen.length, 0);
  assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
});

it("MR create exhausting on a transient API failure is a typed api/mr_create error", async t => {
  const claim = gitlabClaim(4104);
  let creates = 0;
  const forge = fakeGitlab({
    intercept: (req) => {
      if (req.method !== "POST") return undefined;
      creates++;
      return { status: 503, text: async () => "upstream-body-marker" };
    },
  });
  const preflights = countPreflights();
  const r = runner(new StubExecutor(nullLogger()), forge.gitlab);
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(creates, ATTEMPTS);
  // Only createMergeRequest is retried: the push proof ran exactly once (frozen candidate + one push attempt).
  assert.equal(preflights(), 2, "MR-create retries do not rerun the push proof");
  assert.deepEqual(seen.map(s => [s.surface, s.step, s.message]), [["api", "mr_create", "publication_forge_unreachable: mr_create"]]);
  assert.ok(!seen[0]!.message.includes("upstream-body-marker"));
  assert.match(failedReason(claim.run_id), /publication_forge_unreachable: mr_create/);
});

it("a deadline abort during the last MR create attempt stays permanent and is not typed", async t => {
  const claim = gitlabClaim(4106);
  const abort = new AbortController();
  let creates = 0;
  const forge = fakeGitlab({
    intercept: (req) => {
      if (req.method !== "POST") return undefined;
      creates++;
      if (creates === ATTEMPTS) abort.abort();
      return { status: 503, text: async () => "upstream" };
    },
  });
  const r = runner(new StubExecutor(nullLogger()), forge.gitlab);
  instantRetries(r);
  // Give phasePublish a boundary signal this test controls.
  const original = (Object.getPrototypeOf(r) as { phasePublish: (...a: unknown[]) => Promise<void> }).phasePublish;
  const mockMethod = t.mock.method.bind(t.mock) as unknown as (o: object, n: string, f: (...a: never[]) => unknown) => unknown;
  mockMethod(r, "phasePublish", (async (c: unknown, f: unknown, _signal: unknown, ...rest: unknown[]) =>
    original.call(r, c, f, abort.signal, ...rest)) as never);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(creates, ATTEMPTS);
  assert.ok(abort.signal.aborted);
  assert.equal(seen.length, 0, "an aborted publication is never typed as an outage");
  assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
});

it("a deadline abort during the last push attempt stays permanent and is not typed", async t => {
  const claim = gitlabClaim(4108);
  const abort = new AbortController();
  const seam = git as unknown as Seam;
  const real = seam.runGit.bind(git);
  let pushes = 0;
  seam.runGit = async (cwd, args, ...rest) => {
    if (args[0] === "push") {
      pushes++;
      if (pushes === ATTEMPTS) abort.abort();
      throw new Error("git push failed: fatal: unable to access: connection reset by peer");
    }
    return real(cwd, args, ...rest);
  };
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  instantRetries(r);
  const original = (Object.getPrototypeOf(r) as { phasePublish: (...a: unknown[]) => Promise<void> }).phasePublish;
  const mockMethod = t.mock.method.bind(t.mock) as unknown as (o: object, n: string, f: (...a: never[]) => unknown) => unknown;
  mockMethod(r, "phasePublish", (async (c: unknown, f: unknown, _signal: unknown, ...rest: unknown[]) =>
    original.call(r, c, f, abort.signal, ...rest)) as never);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(pushes, ATTEMPTS);
  assert.ok(abort.signal.aborted);
  assert.equal(seen.length, 0, "an aborted push is never typed as an outage");
  assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
});

it("the hook logs one warn carrying run id, surface, step and a redacted raw detail", async () => {
  const claim = gitlabClaim(4109);
  const secret = claim.secrets.forge_pat;
  const seam = git as unknown as Seam;
  const real = seam.runGit.bind(git);
  seam.runGit = async (cwd, args, ...rest) => {
    if (args[0] === "push") throw new Error(`git push failed: fatal: unable to access https://oauth2:${secret}@forge.test/x.git: connection reset by peer`);
    return real(cwd, args, ...rest);
  };
  const warns: Array<{ msg: string; fields: Record<string, unknown> | undefined }> = [];
  const base = nullLogger();
  const log: Logger = {
    ...base,
    warn(msg: string, fields?: Record<string, unknown>) { warns.push({ msg, fields }); },
    child() { return log; },
  };
  const r = runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), fakeGitlab().gitlab, undefined, log);
  instantRetries(r);
  await r.execute(claim);
  const hits = warns.filter(w => w.msg === "publication forge unreachable after retries");
  assert.equal(hits.length, 1);
  const f = hits[0]!.fields!;
  assert.equal(f.run_id, claim.run_id);
  assert.equal(f.surface, "git");
  assert.equal(f.step, "push");
  assert.equal(typeof f.raw_detail, "string");
  assert.ok(!String(f.raw_detail).includes(secret), "raw detail is redacted");
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
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(creates, 1);
  assert.equal(seen.length, 0);
  assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
});

// GitHub claim whose workflows moved ahead during the turn: the finalize base-align overlay path pushes
// through the same pushToOrigin. The typed error must cross every inner align catch unchanged and reach
// the single hook exactly once.
const CI_V1 = "name: ci\non: [push]\njobs: {}\n";
const CI_V2 = "name: ci\non: [pull_request]\njobs: {}\n";
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const gitIn = (dir: string, args: string[]) => execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
function commitToOriginMain(files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(fx.originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", msg]);
}

it("a GitHub align-path push outage invokes the hook once and surfaces the typed error", async t => {
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
  const claim = gitlabClaim(4107, {
    repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
  });
  const executor: Executor = {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      fs.writeFileSync(path.join(ctx.worktreePath, "impl.ts"), "task content\n");
      execFileSync("git", ["-C", ctx.worktreePath, "add", "."], { env: ENV });
      execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "agent work"], { env: ENV });
      commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "main advances");
      return { branch: ctx.branch };
    },
  };
  let pushes = 0;
  git.pushBranch = (async () => { pushes++; throw new Error("git push failed: fatal: unable to access: connection reset by peer"); }) as typeof git.pushBranch;
  const r = new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined,
    { pollMs: 5, planApprovalTimeoutMs: 0, github: fakeGitHub().github });
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(pushes, ATTEMPTS, "the overlay push was the one retried publication hop");
  assert.deepEqual(seen.map(s => [s.surface, s.step]), [["git", "push"]], "exactly one hook invocation");
  assert.match(failedReason(claim.run_id), /publication_forge_unreachable: push/);
});
