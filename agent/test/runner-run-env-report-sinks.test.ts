import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { CAPABILITY_INFERENCE_EVIDENCE_FEATURE, type ClaimRunEnvVar } from "../src/protocol.js";
import type { WorkerClient } from "../src/client.js";
import { makeUziToolHandlers } from "../src/uzi-tools.js";
import { makeTextRedactor } from "../src/redact.js";
import { redactPorcelainLine } from "../src/run-env-scan.js";
import { runEnvClaimForms } from "../src/run-env-forms.js";
import type { Executor, RunContext } from "../src/executor.js";
import { LimitReachedError } from "../src/limit.js";
import { api, client, fakeGitlab, fx, git, installHarness, runner, runnerWith } from "./runner-harness.js";
import { GateExecutor, freshClaim, newApi, row, send } from "./gate-revision-harness.js";
import { nullLogger } from "./helpers.js";

installHarness();

// PRD 4416 review items 1 and 4: worker-to-API report fields that can carry agent or command text pass the
// run's redactor. Dummy values only, assembled from parts.

const UUID = "0b8d7a52-91c4-4e0f-8a36-5d2f9c1e7b44";
const PLAIN_SECRET = ["rv-Qk7", "Zp2m", "Xw9d", "Lt4s"].join("-");
// A character git C-quotes in a path, so the quoted porcelain form splits the value into octal escapes.
const QUOTED_SECRET = ["rv-Ünï", "Hn3b", "Yc6v"].join("-");
const variable = (name: string, value: string): ClaimRunEnvVar =>
  ({ var_id: UUID, scope: "repo", rev: 1, name, value, secret: true, description: "" });
const secrets = () => ({
  forge_pat: "fixture-forge-pat-000000",
  anthropic_oauth_token: "vendor-oauth-token-000000",
  run_env: [variable("DB_PASSWORD", PLAIN_SECRET), variable("OTHER_SECRET", QUOTED_SECRET)],
});
const forms = () => runEnvClaimForms(secrets().run_env, []).redaction;
function assertNoValue(label: string, value: unknown): void {
  const text = JSON.stringify(value);
  for (const form of forms()) assert.ok(!text.includes(form), `${label}: no registered form of a value appears`);
}

describe("redactPorcelainLine", () => {
  const redact = makeTextRedactor(forms());
  it("is the identical string when nothing matches", () => {
    for (const line of [" M src/app.ts", "?? \"caf\\303\\251.md\"", "R  old.txt -> new.txt"]) assert.equal(redactPorcelainLine(line, redact), line);
  });
  it("redacts a value inside a C-quoted path that the quoted form hides", () => {
    const quoted = `?? "tmp/rv-\\303\\234n\\303\\257-Hn3b-Yc6v.json"`;
    assert.ok(!redact(quoted).includes("REDACTED"), "the redactor alone misses the quoted form");
    const out = redactPorcelainLine(quoted, redact);
    assert.ok(out.startsWith("?? ") && out.includes("REDACTED"), out);
    assert.ok(!out.includes(QUOTED_SECRET));
  });
});

describe("the plan gate report redacts plan_changed_files", () => {
  it("withholds a value in a plain and in a C-quoted path, and leaves other paths alone", async () => {
    newApi();
    const files = [
      ` M src/app.ts`,
      `?? tmp/${PLAIN_SECRET}.json`,
      `?? "tmp/rv-\\303\\234n\\303\\257-Hn3b-Yc6v.json"`,
      `R  old/${PLAIN_SECRET}.txt -> new/ok.txt`,
    ];
    const original = git.planChangedFiles;
    git.planChangedFiles = (async () => files) as typeof git.planChangedFiles;
    try {
      const claim = freshClaim(1, { secrets: secrets() as never });
      api.onState(claim.run_id, (body) => { if (body.status === "awaiting_approval") send(claim.run_id, row("approve_plan")); });
      const { gitlab } = fakeGitlab();
      await runner(new GateExecutor(["# plan"]), gitlab, undefined, { planApprovalTimeoutMs: 4000 }).execute(claim);
      const gate = api.states.find((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval")!;
      assert.ok(gate, "the run reached the plan gate");
      const reported = gate.body.plan_changed_files as string[];
      assert.equal(reported.length, files.length);
      assertNoValue("plan_changed_files", reported);
      assert.equal(reported[0], files[0], "an unrelated path is unchanged");
      assert.ok(reported[1]!.includes("REDACTED") && reported[2]!.includes("REDACTED") && reported[3]!.includes("REDACTED"), JSON.stringify(reported));
    } finally {
      git.planChangedFiles = original;
    }
  });
});

describe("capability inference evidence redacts trigger paths", () => {
  it("a Dockerfile path holding the value reaches the report redacted", async () => {
    newApi();
    client.protocolFeatures = [...client.protocolFeatures, CAPABILITY_INFERENCE_EVIDENCE_FEATURE];
    const dir = path.join(fx.originPath, `svc-${PLAIN_SECRET}`);
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(path.join(dir, "Dockerfile"), "FROM alpine\n");
    const env = { ...process.env };
    for (const key of Object.keys(env)) if (key.startsWith("GIT_")) delete env[key];
    env.GIT_CONFIG_GLOBAL = "/dev/null"; env.GIT_CONFIG_SYSTEM = "/dev/null";
    execFileSync("git", ["-C", fx.originPath, "add", "."], { env, stdio: "pipe" });
    execFileSync("git", ["-C", fx.originPath, "commit", "-m", "detector fixture"], { env, stdio: "pipe" });
    const claim = freshClaim(1, { secrets: secrets() as never });
    api.onState(claim.run_id, (body) => { if (body.status === "awaiting_approval") send(claim.run_id, row("approve_plan")); });
    const { gitlab } = fakeGitlab();
    await runner(new GateExecutor(["# plan"]), gitlab, undefined, { planApprovalTimeoutMs: 4000 }).execute(claim);
    const report = api.states.find((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval")!;
    assert.ok(report, "the run reached the plan gate");
    const evidence = report.body.capability_inference_evidence;
    assert.ok(evidence?.docker?.triggers.length, "the detector saw the Dockerfile");
    assert.ok(evidence.docker.triggers[0]!.path.includes("REDACTED"), JSON.stringify(evidence));
    assertNoValue("every reported state body", api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body));
  });
});

describe("chat propose_issue redacts the repo selector", () => {
  it("repo_path and a string repo_id never carry the value", async () => {
    const sent: Array<Record<string, unknown>> = [];
    const stub = {
      async createProposal(runId: string, body: Record<string, unknown>) {
        sent.push(body);
        return { id: "p1", run_id: runId, title: "t", description: "d", labels: [], status: "pending", created_at: "2026-07-19T00:00:00Z" };
      },
    } as unknown as WorkerClient;
    const redactText = makeTextRedactor([PLAIN_SECRET]);
    const h = makeUziToolHandlers({ client: stub, runId: "chat", emit: () => {}, log: nullLogger(), redactText });
    await h.proposeIssue({ repo_path: `grp/${PLAIN_SECRET}`, title: "t" });
    await h.proposeIssue({ repo_id: PLAIN_SECRET, title: "t" });
    assert.equal(sent.length, 2);
    assert.ok(!JSON.stringify(sent).includes(PLAIN_SECRET));
    assert.ok(String(sent[0]!.repo_path).startsWith("grp/"), "the non-secret part is kept");
  });
});

describe("a park reports no value in any state body", () => {
  it("a limit park whose work and error text carry the value", async () => {
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-re-park-sink-"));
    const { gitlab } = fakeGitlab();
    const claim = freshClaim(1, { wait_on_limit: true, secrets: secrets() as never });
    const exec: Executor = {
      run: async (ctx: RunContext) => {
        fs.mkdirSync(path.join(homeRoot, ctx.runId), { recursive: true });
        throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour", message: `limit while using ${PLAIN_SECRET}` } as never);
      },
    };
    try {
      await runnerWith((runId) => ({ homeDir: path.join(homeRoot, runId), executor: exec }), gitlab).execute(claim);
    } finally {
      fs.rmSync(homeRoot, { recursive: true, force: true });
    }
    const bodies = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    assert.ok(bodies.some((b) => b.status === "limit_wait"), "the run parked");
    assertNoValue("every reported state body", bodies);
  });
});
