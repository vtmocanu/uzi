import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Executor, RunContext } from "../src/executor.js";
import type { ClaimRunEnvRedact, ClaimRunEnvVar } from "../src/protocol.js";
import { runEnvRedactionForms } from "../src/run-env-forms.js";
import fs from "node:fs";
import path from "node:path";
import { api, fakeGitlab, fx, gitlabClaim, homeDir, installHarness, runnerWith, secretRecordingLogger } from "./runner-harness.js";

installHarness();

// PRD 4416 M2 (W2b R3): the runner registers every form of a secret run variable with the logger and the
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

    const forms = runEnvRedactionForms(runEnv(), runEnvRedact());
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
    for (const f of runEnvRedactionForms(runEnv(), runEnvRedact())) assert.ok(added.includes(f), "the redaction form is registered for every kind");
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

// PRD 4416 M2 (W2b item 3): run_env values live in memory only. The cheapest credible proof is two-fold:
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
