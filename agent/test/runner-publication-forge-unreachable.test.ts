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
type ExecSeam = { execScoped(cmd: string, args: string[], ...rest: unknown[]): Promise<unknown> };

/** Fail one git subcommand at the process boundary with the given stderr, so runGit retains it as it does live. */
function failGit(sub: string, stderr: string, onHit: (args: string[]) => void): void {
  const seam = git as unknown as ExecSeam;
  const real = seam.execScoped.bind(git);
  seam.execScoped = async (cmd, args, ...rest) => {
    if (cmd === "git" && args.includes(sub)) {
      onHit(args);
      throw Object.assign(new Error(`Command failed: git ${args.join(" ")}`), { code: 128, stderr });
    }
    return real(cmd, args, ...rest);
  };
}

const ATTEMPTS = FORGE_RETRY_SCHEDULE.length + 1;

function countPreflights(): () => number {
  let preflights = 0;
  const preflight = git.scratchPublicationPreflight.bind(git);
  git.scratchPublicationPreflight = async (...args) => { preflights++; return preflight(...args); };
  return () => preflights;
}

it("floor_unreachable retries with a full proof per attempt, then exhausts as a typed git/floor_refresh error", async t => {
  const claim = gitlabClaim(4101);
  let listings = 0;
  failGit("ls-remote", "fatal: unable to access: Failed to connect to forge.test port 443: Connection refused", () => { listings++; });
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
  let pushes = 0;
  failGit("push", "fatal: unable to access: connection reset by peer", () => { pushes++; });
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

it("a permanent push stderr is not typed even when the branch name looks like a transient status", async t => {
  // The branch is agent/issue-502 and git's argument list carries it; only git's own stderr may decide.
  const claim = gitlabClaim(502);
  let pushes = 0;
  failGit("push", "fatal: the remote end hung up on policy grounds", () => { pushes++; });
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(pushes, 1, "the branch name must not make a permanent push retryable");
  assert.equal(seen.length, 0);
  assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
});

it("a transient push stderr retries the full schedule even when the branch name looks like a permanent status", async t => {
  // The branch is agent/issue-404; a real outage on it must still retry before it is typed.
  const claim = gitlabClaim(404);
  let pushes = 0;
  failGit("push", "fatal: unable to access: connection reset by peer", () => { pushes++; });
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(pushes, ATTEMPTS);
  assert.deepEqual(seen.map(s => [s.surface, s.step]), [["git", "push"]]);
  assert.match(failedReason(claim.run_id), /publication_forge_unreachable: push/);
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
  let pushes = 0;
  failGit("push", "fatal: unable to access: connection reset by peer", () => {
    pushes++;
    if (pushes === ATTEMPTS) abort.abort();
  });
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
  failGit("push", `fatal: unable to access https://oauth2:${secret}@forge.test/x.git: connection reset by peer`, () => {});
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

// #3929: GitHub's bare `(failed)` remote rejection (no stated reason; cause unproven) is classified transient and
// retries on the schedule; every rejection that states a reason keeps failing fast. The controls below
// must behave identically before and after the change.
const FAKE_SHA = "0123456789abcdef0123456789abcdef01234567";
const bareStatus = (branch: string, reason = "failed") => ` ! [remote rejected] ${FAKE_SHA} -> ${branch} (${reason})\n`;
const framed = (status: string, remote = "") =>
  `To https://github.com/org/repo.git\n${status}${remote}error: failed to push some refs to 'https://github.com/org/repo.git'\n`;

/** Record each push's argv, then fail it with `stderr`; `failFirstOnly` lets later pushes through to the real git. */
function recordPushes(stderr: string, opts: { performRealPush?: boolean; failFirstOnly?: boolean } = {}): string[][] {
  const argvs: string[][] = [];
  const seam = git as unknown as ExecSeam;
  const real = seam.execScoped.bind(git);
  seam.execScoped = async (cmd, args, ...rest) => {
    if (cmd === "git" && args.includes("push")) {
      argvs.push([...args]);
      if (!opts.failFirstOnly || argvs.length === 1) {
        if (opts.performRealPush) await real(cmd, args, ...rest);
        throw Object.assign(new Error(`Command failed: git ${args.join(" ")}`), { code: 128, stderr });
      }
    }
    return real(cmd, args, ...rest);
  };
  return argvs;
}

const refspecOf = (argv: string[]) => argv.find(a => /^[0-9a-f]{40}:refs\/heads\//.test(a));

function assertIdenticalNonForcedPushes(argvs: string[][], branch: string): void {
  const specs = argvs.map(refspecOf);
  assert.ok(specs[0], "a push carried a sha:refs/heads/<branch> refspec");
  assert.match(specs[0]!, new RegExp(`^[0-9a-f]{40}:refs/heads/${branch}$`));
  assert.ok(specs.every(s => s === specs[0]), "every attempt pushed the identical candidate refspec");
  assert.ok(argvs.every(a => a.every(arg => !arg.startsWith("--force") && arg !== "-f" && !arg.startsWith("+"))), "no force flags or forced refspec");
}

const completed = (runId: string) => api.states.some(s => s.runId === runId && s.body.status === "completed");

it("a bare (failed) push rejection on the plain arm retries the whole schedule and is typed git/push", async t => {
  const claim = gitlabClaim(4301);
  const branch = "agent/issue-4301";
  const argvs = recordPushes(framed(bareStatus(branch)));
  const preflights = countPreflights();
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(argvs.length, ATTEMPTS);
  assertIdenticalNonForcedPushes(argvs, branch);
  assert.equal(preflights(), ATTEMPTS + 1, "one frozen-candidate preflight plus one per pushBranch attempt");
  assert.deepEqual(seen.map(s => [s.surface, s.step]), [["git", "push"]]);
  assert.match(failedReason(claim.run_id), /publication_forge_unreachable: push/);
});

it("a bare (failed) push rejection on branch agent/issue-404 is still retried (plain arm)", async t => {
  const claim = gitlabClaim(404);
  const branch = "agent/issue-404";
  const argvs = recordPushes(framed(bareStatus(branch)));
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(argvs.length, ATTEMPTS);
  assertIdenticalNonForcedPushes(argvs, branch);
  assert.deepEqual(seen.map(s => [s.surface, s.step]), [["git", "push"]]);
  assert.match(failedReason(claim.run_id), /publication_forge_unreachable: push/);
});

it("one bare (failed) push rejection followed by a real push completes the run", async t => {
  const claim = gitlabClaim(4302);
  const branch = "agent/issue-4302";
  const argvs = recordPushes(framed(bareStatus(branch)), { failFirstOnly: true });
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(argvs.length, 2);
  assertIdenticalNonForcedPushes(argvs, branch);
  assert.equal(seen.length, 0);
  assert.ok(completed(claim.run_id), "the retry published and the run completed");
});

it("a (failed) rejection thrown AFTER the push really landed is recognised on retry and the run completes", async t => {
  const claim = gitlabClaim(4303);
  const branch = "agent/issue-4303";
  const argvs = recordPushes(framed(bareStatus(branch)), { performRealPush: true, failFirstOnly: true });
  const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(argvs.length, 2);
  assertIdenticalNonForcedPushes(argvs, branch);
  assert.equal(seen.length, 0);
  assert.ok(completed(claim.run_id), "remote already at the candidate; the retry is accepted");
});

const PERMANENT_CONTROLS: Array<[string, (b: string) => string, string?]> = [
  ["protected branch hook declined", b => framed(bareStatus(b, "protected branch hook declined"))],
  ["pre-receive hook declined", b => framed(bareStatus(b, "pre-receive hook declined"))],
  ["failed to update ref", b => framed(bareStatus(b, "failed to update ref"))],
  ["repository rule violations with GH013 remote lines", b => framed(bareStatus(b, "push declined due to repository rule violations"),
    "remote: error: GH013: Repository rule violations found for refs/heads/" + b + ".\nremote: \n"), "push_secret_blocked"],
  ["bare (failed) plus GH006 remote error", b => framed(bareStatus(b), `remote: error: GH006: Protected branch update failed for refs/heads/${b}.\n`)],
  ["bare (failed) plus GH001 remote error", b => framed(bareStatus(b), "remote: error: GH001: Large files detected.\n")],
  ["bare (failed) mixed with a hook-declined status line", b => framed(bareStatus(b) + bareStatus(b + "-2", "pre-receive hook declined"))],
];

PERMANENT_CONTROLS.forEach(([name, build, failOrigin], i) => {
  it(`control: ${name} is pushed exactly once and is not typed publication_forge_unreachable`, async t => {
    const iid = 4310 + i;
    const claim = gitlabClaim(iid);
    const branch = `agent/issue-${iid}`;
    const argvs = recordPushes(build(branch));
    const r = runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab);
    instantRetries(r);
    const seen = hookRecorder(t, r);
    await r.execute(claim);
    assert.equal(argvs.length, 1);
    assertIdenticalNonForcedPushes(argvs, branch);
    assert.equal(seen.length, 0);
    assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
    const failedState = api.states.find(s => s.runId === claim.run_id && s.body.status === "failed");
    assert.ok(failedState, "the run reached failed");
    // Today's outcome, identical before and after #3929: a typed secret block, else an untyped raw push failure.
    assert.equal(failedState.body.fail_origin, failOrigin);
    if (failOrigin) assert.match(failedState.body.failure_reason ?? "", /Push Protection \(GH013\)/);
    else assert.match(failedState.body.failure_reason ?? "", /^git push origin [0-9a-f]{40}:refs\/heads\/agent\/issue-\d+ failed: /);
  });
});

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

/** GitHub claim whose workflows moved ahead during the turn, so finalize takes the base-align overlay arm. */
function githubOverlayRunner(iid: number): { claim: ReturnType<typeof gitlabClaim>; r: RunRunner; strategies: string[] } {
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
  const claim = gitlabClaim(iid, {
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
  const strategies: string[] = [];
  const orig = git.alignBranchWithDefault.bind(git);
  git.alignBranchWithDefault = (async (...args: Parameters<typeof git.alignBranchWithDefault>) => {
    strategies.push(args[4]);
    return orig(...args);
  }) as typeof git.alignBranchWithDefault;
  const r = new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined,
    { pollMs: 5, planApprovalTimeoutMs: 0, github: fakeGitHub().github });
  instantRetries(r);
  return { claim, r, strategies };
}

it("a bare (failed) push rejection on the GitHub overlay arm retries the overlay candidate and is typed git/push", async t => {
  const { claim, r, strategies } = githubOverlayRunner(4304);
  const branch = "agent/issue-4304";
  const argvs = recordPushes(framed(bareStatus(branch)));
  const preflights = countPreflights();
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(argvs.length, ATTEMPTS, "the overlay push was the one retried publication hop");
  assertIdenticalNonForcedPushes(argvs, branch);
  assert.equal(preflights(), ATTEMPTS + 1, "one frozen-candidate preflight plus one per pushBranch attempt");
  assert.ok(strategies.includes("workflow-subtree"), `the overlay arm ran: ${strategies.join(",")}`);
  assert.deepEqual(seen.map(s => [s.surface, s.step]), [["git", "push"]]);
  assert.match(failedReason(claim.run_id), /publication_forge_unreachable: push/);
  assert.ok(!strategies.includes("merge") && !strategies.includes("rebase"), `no merge/rebase fallback: ${strategies.join(",")}`);
});

const WORKFLOW_SCOPE_PLAIN = "refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without workflow scope";

it("control: a workflow-scope push rejection on the overlay arm is not retried at the overlay candidate and falls back to merge", async t => {
  const { claim, r, strategies } = githubOverlayRunner(4305);
  const branch = "agent/issue-4305";
  const argvs = recordPushes(framed(bareStatus(branch, WORKFLOW_SCOPE_PLAIN)));
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  const failed = api.states.find(s => s.runId === claim.run_id && s.body.status === "failed");
  assert.equal(failed?.body.fail_origin, "finalize_base_align_conflict", "merge and rebase pushes were refused too, so the run fails typed");
  assert.ok(strategies.includes("workflow-subtree"), `the overlay arm ran: ${strategies.join(",")}`);
  assert.ok(strategies.includes("merge"), `the merge fallback ran: ${strategies.join(",")}`);
  const first = refspecOf(argvs[0]!);
  assert.equal(argvs.filter(a => refspecOf(a) === first).length, 1, "the overlay candidate was pushed once, not retried");
  assert.ok(argvs.every(a => a.every(arg => !arg.startsWith("--force") && arg !== "-f" && !arg.startsWith("+"))));
  assert.equal(seen.length, 0);
  assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
});

// Pins existing behaviour (unchanged by #3929): GitHub's real wording quotes `workflow` in backticks, which
// isWorkflowScopeRejection (the literal "workflow scope") does not match, so the overlay push error is
// rethrown with no merge/rebase fallback.
it("control: the backticked GitHub workflow-scope wording ends publication after one overlay push (existing behaviour)", async t => {
  const { claim, r, strategies } = githubOverlayRunner(4306);
  const branch = "agent/issue-4306";
  const reason = "refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without `workflow` scope";
  const argvs = recordPushes(framed(bareStatus(branch, reason)));
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  const failedState = api.states.find(s => s.runId === claim.run_id && s.body.status === "failed");
  assert.equal(failedState?.body.fail_origin, undefined);
  assert.match(failedState?.body.failure_reason ?? "", /^git push origin [0-9a-f]{40}:refs\/heads\/agent\/issue-4306 failed: .*without `workflow` scope/);
  assert.ok(failedState, "the run reached failed");
  assert.ok(strategies.includes("workflow-subtree"), `the overlay arm ran: ${strategies.join(",")}`);
  assert.ok(!strategies.includes("merge") && !strategies.includes("rebase"), `no merge/rebase fallback: ${strategies.join(",")}`);
  assert.equal(argvs.length, 1, "exactly one overlay push");
  assert.equal(seen.length, 0);
  assert.doesNotMatch(failedReason(claim.run_id), /publication_forge_unreachable/);
});

// GitHub claim whose workflows moved ahead during the turn: the finalize base-align overlay path pushes
// through the same pushToOrigin. The typed error must cross every inner align catch unchanged and reach
// the single hook exactly once.
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
  failGit("push", "fatal: unable to access: connection reset by peer", () => { pushes++; });
  const r = new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined,
    { pollMs: 5, planApprovalTimeoutMs: 0, github: fakeGitHub().github });
  instantRetries(r);
  const seen = hookRecorder(t, r);
  await r.execute(claim);
  assert.equal(pushes, ATTEMPTS, "the overlay push was the one retried publication hop");
  assert.deepEqual(seen.map(s => [s.surface, s.step]), [["git", "push"]], "exactly one hook invocation");
  assert.match(failedReason(claim.run_id), /publication_forge_unreachable: push/);
});
