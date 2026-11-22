import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "../src/protocol.js";
import { RUN_ENV_FORMS_OVER_BUDGET, runEnvClaimForms } from "../src/run-env-forms.js";
import { execFileSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { api, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, secretRecordingLogger } from "./runner-harness.js";
import { runEnvSeen } from "../src/run-env-seen.js";

installHarness();

// PRD 4416 M2: the runner registers every form of a secret run variable with the logger and the
// flight's redactor, hands the variables to a lead-lane executor, and evicts the forms at terminal.
// Every value is an obvious dummy.

const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const SECRET = "dummy-secret-run-value-000000";
const OLD_SECRET = "dummy-retired-run-value-111111";
const PLAIN = "dummy-plain-run-value-222222";

const runEnv = (): ClaimRunEnvVar[] => [
  { var_id: UUID, scope: "repo", rev: 2, name: "DB_PASSWORD", value: SECRET, secret: true, description: "" },
  { var_id: UUID, scope: "repo", rev: 1, name: "REGION", value: PLAIN, secret: false, description: "" },
];
const runEnvRedact = (): ClaimRunEnvRedact[] => [{ var_id: UUID, scope: "repo", rev: 1, name: "DB_PASSWORD", value: OLD_SECRET }];
const secrets = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan", run_env: runEnv(), run_env_redact: runEnvRedact() });

let iid = 46001;

describe("runner wiring of run variables", () => {
  it("registers each redaction form, scrubs through the flight redactor and evicts at terminal", async () => {
    const { gitlab } = fakeGitlab();
    const { logger, added, removed } = secretRecordingLogger();
    const claim = gitlabClaim(iid++, { secrets: secrets() });
    let seen: RunContext | undefined;
    let scrubbed = "";
    const exec: Executor = {
      run: async (ctx) => {
        seen = ctx;
        const b64 = Buffer.from(`user:${SECRET}`).toString("base64");
        scrubbed = ctx.redactText!(`token ${SECRET} basic ${b64} old ${OLD_SECRET} region ${PLAIN}`);
        return { branch: ctx.branch, reportOnly: true, summary: "ok" };
      },
    };
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger).execute(claim);

    const forms = runEnvClaimForms(runEnv(), runEnvRedact()).redaction;
    assert.ok(forms.length >= 2);
    for (const f of forms) {
      assert.ok(added.includes(f), `form registered: ${f.slice(0, 8)}`);
      assert.ok(removed.includes(f), `form evicted at terminal: ${f.slice(0, 8)}`);
    }
    assert.ok(!added.includes(PLAIN), "a plain variable is never registered");
    assert.ok(!scrubbed.includes(SECRET) && !scrubbed.includes(OLD_SECRET), "the flight redactor scrubs both versions");
    assert.ok(!scrubbed.includes(Buffer.from(`user:${SECRET}`).toString("base64").slice(4, 20)), "and an embedded base64 form");
    assert.ok(scrubbed.includes(PLAIN), "a plain value stays readable");
    assert.deepEqual(seen!.runEnv!.map((v) => v.name), ["DB_PASSWORD", "REGION"], "a lead-lane run carries the variables");
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
  });

  it("a non-lead claim (a review-target task) carries no runEnv although its redaction forms are still registered", async () => {
    const { gitlab } = fakeGitlab();
    const { logger, added } = secretRecordingLogger();
    const n = iid++;
    // Defense in depth: the api never sends variables to this lane, but a claim that did must not reach the executor.
    const claim = gitlabClaim(n, {
      secrets: secrets(), kind: "task", issue_iid: null, open_mr: false, branch: `uzi/task/review-${n}`,
      review_target_run_id: "6c1f0e2a-3b4d-4f5a-8c7e-9d0a1b2c3d4e",
    } as never);
    let seen: RunContext | undefined;
    let scrubbed = "";
    const exec: Executor = {
      run: async (ctx) => {
        seen = ctx;
        scrubbed = ctx.redactText!(`token ${SECRET} old ${OLD_SECRET}`);
        return { branch: ctx.branch, reportOnly: true, summary: "ok" };
      },
    };
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger).execute(claim);
    assert.ok(seen, "the executor ran");
    assert.equal(seen!.runEnv, undefined, "no variables for a non-lead lane");
    for (const f of runEnvClaimForms(runEnv(), runEnvRedact()).redaction) assert.ok(added.includes(f), "the redaction form is registered for every kind");
    assert.ok(!scrubbed.includes(SECRET) && !scrubbed.includes(OLD_SECRET), "and the flight redactor still scrubs both versions");
  });

  it("a run without variables carries no runEnv", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++);
    let seen: RunContext | undefined;
    const exec: Executor = { run: async (ctx) => { seen = ctx; return { branch: ctx.branch, reportOnly: true, summary: "ok" }; } };
    await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);
    assert.equal(seen!.runEnv, undefined);
  });
});

// PRD 4416 M2: a claim that carries variables leaves a durable marker before any executor exists, and a run that
// cannot write it never starts with variables.
describe("the run variable marker", () => {
  it("is written before the executor runs, and only for a claim that carries variables", async () => {
    const { gitlab } = fakeGitlab();
    const withVars = gitlabClaim(iid++, { secrets: secrets() });
    const without = gitlabClaim(iid++);
    const during: Array<boolean | "unknown"> = [];
    const exec: Executor = {
      run: async (ctx) => {
        during.push(await runEnvSeen(git.runEnvSeenRoot, ctx.runId));
        return { branch: ctx.branch, reportOnly: true, summary: "ok" };
      },
    };
    await runnerWith(() => ({ executor: exec }), gitlab).execute(withVars);
    await runnerWith(() => ({ executor: exec }), gitlab).execute(without);
    assert.deepEqual(during, [true, false]);
    assert.equal(await runEnvSeen(git.runEnvSeenRoot, withVars.run_id), true, "and it stays after the run ends");
  });

  it("a marker that cannot be written stops the run before any executor is built", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: secrets() });
    const original = git.runEnvSeenRoot;
    const blocker = path.join(fx.dataDir, "not-a-directory");
    fs.writeFileSync(blocker, "x");
    Object.defineProperty(git, "runEnvSeenRoot", { value: path.join(blocker, "run-env-seen"), configurable: true, writable: true });
    let built = 0;
    let outcome: unknown;
    try {
      outcome = await runnerWith(() => { built++; return { executor: { run: async () => { throw new Error("must not run"); } } }; }, gitlab).execute(claim).then(() => "resolved", (e: unknown) => e);
    } finally {
      Object.defineProperty(git, "runEnvSeenRoot", { value: original, configurable: true, writable: true });
    }
    assert.equal(built, 0, "no executor was built");
    assert.ok(outcome instanceof Error, "execute rejects with the marker write error");
    assert.equal((outcome as NodeJS.ErrnoException).code, "ENOTDIR");
    assert.deepEqual(api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status), [], "no state is reported for the run");
  });
});

// PRD 4416 M2: run_env values live in memory only. The cheapest credible proof is two-fold:
// the durable-state writers take typed records that cannot carry the claim's secrets object (a source
// pin: none of them names the claim types, `secrets` or run_env), and a run executed with a plaintext
// secret variable leaves no file under the worker's data directory or the run home that contains it.
describe("run variables are never written to durable worker state", () => {
  const srcDir = new URL("../src/", import.meta.url).pathname;
  const WRITERS = fs.readdirSync(srcDir).filter((f) =>
    /^(outbox|publication-intent|restore-journal|recovery.*|hibernation-.*|active-run-registry|retained-homes|terminal-.*)\.ts$/.test(f));

  it("the durable-state writers exist and none of them can see the claim secrets", () => {
    assert.ok(WRITERS.length >= 15, `found ${WRITERS.join(",")}`);
    for (const f of WRITERS) {
      const text = fs.readFileSync(path.join(srcDir, f), "utf8");
      assert.doesNotMatch(text, /run_env|runEnv|ClaimRunEnv|ClaimResponse|ClaimSecrets|claim\.secrets/, f);
    }
  });

  const walk = (dir: string): string[] => {
    const out: string[] = [];
    for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
      const p = path.join(dir, e.name);
      if (e.isDirectory()) out.push(...walk(p));
      else if (e.isFile()) out.push(p);
    }
    return out;
  };
  const filesContaining = (needle: string): string[] =>
    [fx.dataDir, homeDir].flatMap((root) => walk(root)).filter((f) => fs.readFileSync(f).includes(needle));

  for (const outcome of ["completed", "failed"] as const) {
    it(`a ${outcome} run leaves no file carrying the value`, async () => {
      const { gitlab } = fakeGitlab();
      const claim = gitlabClaim(iid++, { secrets: secrets() });
      const exec: Executor = {
        run: async (ctx) => {
          if (outcome === "failed") throw new Error("boom");
          return { branch: ctx.branch, reportOnly: true, summary: "ok" };
        },
      };
      await runnerWith(() => ({ executor: exec }), gitlab).execute(claim);
      assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === outcome));
      assert.ok(walk(fx.dataDir).length > 0, "the scan covers real state");
      for (const needle of [SECRET, OLD_SECRET, PLAIN]) assert.deepEqual(filesContaining(needle), []);
    });
  }
});

// PRD 4416 M2: a claim whose forms pass the budget fails the run before the clone and the executor, names no value,
// and still registers the raw values so any text is masked.
describe("a claim over the form budget", () => {
  const multiLine = (seed: string, bytes: number): string => {
    let text = "";
    for (let i = 0; text.length < bytes; i++) text += crypto.createHash("sha512").update(`${seed}-${i}`).digest("base64").replace(/=+$/, "") + "\r\n";
    return text;
  };
  const OVER_ENTRIES = 40;
  const overBudget = () => ({
    forge_pat: "fixture-forge-pat-000000",
    anthropic_oauth_token: "dummy-oauth-do-not-scan",
    run_env_redact: Array.from({ length: OVER_ENTRIES }, (_, i) =>
      ({ var_id: UUID, scope: "repo" as const, rev: i + 1, name: `OLD_${i % 5}`, value: multiLine(`over-${i}`, 65536) })),
  });

  it("fails with run_env_forms_over_budget, runs no executor and pushes nothing", async () => {
    const { gitlab } = fakeGitlab();
    const { logger, added } = secretRecordingLogger();
    const claim = gitlabClaim(iid++, { secrets: overBudget() as never });
    let ran = 0;
    const exec: Executor = { run: async (ctx) => { ran++; return { branch: ctx.branch, reportOnly: true, summary: "ok" }; } };
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger).execute(claim);

    assert.equal(ran, 0, "the executor never ran");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.ok(failed, "the run failed");
    assert.match(String(failed.failure_reason), new RegExp(RUN_ENV_FORMS_OVER_BUDGET));
    const first = overBudget().run_env_redact[0]!.value;
    assert.ok(!JSON.stringify(failed).includes(first.slice(0, 40)), "the report carries no value");
    assert.ok(added.includes(first), "the raw value is registered so any text is masked");
    assert.equal(api.states.filter((s) => s.runId === claim.run_id && s.body.status === "completed").length, 0);
    let pushed = true;
    try {
      execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", "--quiet", `refs/heads/agent/issue-${claim.issue_iid}`], { stdio: "pipe" });
    } catch { pushed = false; }
    assert.equal(pushed, false, "nothing reached origin");
  });

  it("a lane with no flight resolves needles for the live over-budget run as a withhold, never an empty set", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(iid++, { secrets: overBudget() as never });
    const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch, reportOnly: true, summary: "ok" }) };
    const runner = runnerWith(() => ({ executor: exec }), gitlab);
    const internals = runner as unknown as {
      liveScanNeedles: Map<string, unknown>;
      resolveRunScanNeedles(runId: string): Promise<{ kind: string; reason?: string; needles?: unknown[] }>;
    };
    const resolutions: Array<Promise<{ kind: string; reason?: string; needles?: unknown[] }>> = [];
    const set = internals.liveScanNeedles.set.bind(internals.liveScanNeedles);
    internals.liveScanNeedles.set = (runId: string, value: unknown) => {
      const out = set(runId, value);
      resolutions.push(internals.resolveRunScanNeedles(runId));
      return out;
    };
    await runner.execute(claim);
    assert.equal(resolutions.length, 1, "the live set was published once for the run");
    assert.deepEqual(await resolutions[0], { kind: "withhold", reason: "forms_over_budget" });
    assert.equal(internals.liveScanNeedles.has(claim.run_id), false, "and it is evicted at terminal");
  });

  it("a hand-built over-budget flight is withheld by the upload scan and the checkpoint range scan, never read as clean", async () => {
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor: { run: async () => ({ branch: "b" }) } }), gitlab);
    const warnings: string[] = [];
    const flight = {
      runId: "run-over-budget-direct", scanOverBudget: true, scanNeedles: [] as unknown[], branch: undefined,
      runLog: { warn: (m: string) => { warnings.push(m); }, info: () => undefined },
    };
    const internals = runner as unknown as {
      requireNoRunEnv(f: unknown, bare: string, tip: string, excludes: string[], what: string): Promise<unknown>;
      scanCheckpointForPublish(f: unknown, bare: string, branch: string): Promise<{ kind: string; outcome?: string }>;
    };
    await assert.rejects(internals.requireNoRunEnv(flight, "/nonexistent-bare", "a".repeat(40), [], "direct"),
      (e: Error) => e.constructor.name === "SecretRemediationPendingError");
    assert.ok(warnings.length > 0, "the withhold is logged");
    git.resolveCheckpointRange = (async () => ({ tipSha: "a".repeat(40), excludeSha: "b".repeat(40) })) as unknown as typeof git.resolveCheckpointRange;
    const outcome = await internals.scanCheckpointForPublish({ ...flight, publishedRealTips: [], reportedPublishOutcomes: new Set(["secret:untrusted"]) }, "/nonexistent-bare", "agent/issue-1");
    assert.deepEqual(outcome, { kind: "blocked", outcome: "secret_scan_untrusted" });
  });
});
