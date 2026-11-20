import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Executor } from "../src/executor.js";
import type { ClaimProviderBinding } from "../src/protocol.js";
import { providerKeyForms } from "../src/redact.js";
import { api, fakeGitlab, gitlabClaim, input, installHarness, runner } from "./runner-harness.js";

installHarness();

// The plan and its milestone titles are model-authored and ride the awaiting_approval (human gate) or
// running (autopilot) report to the api, whose scrub is pattern-based and cannot catch a prefixless
// provider key. The worker must therefore redact them before the report. The key is a synthetic
// prefixless value assembled at runtime.
const KEY = ["Qm4x", "Lp/8", "sT+2", "&hN5", "<v3>", "wR7k"].join("");
const FORMS = providerKeyForms(KEY);

const binding = (): ClaimProviderBinding => ({
  provider_id: "p-1", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/a",
  auth_style: "bearer", credential: KEY, model_map: { haiku: { model: "corp-haiku" } },
});
const bound = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "", provider: binding() });
const vendor = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000" });

const PLAN = `# Plan\n- use ${KEY} as the header\n- escaped ${FORMS.find((f) => f.includes("\\/")) ?? KEY}`;
const MILESTONES = [{ id: "m1", title: `Rotate ${KEY}` }, { id: "m2", title: "Plain milestone" }];

function assertNoKey(label: string, text: string): void {
  for (const f of FORMS) assert.ok(!text.includes(f), `${label}: form leaked: ${f}`);
  for (const part of ["Qm4x", "wR7k"]) assert.ok(!text.includes(part), `${label}: fragment ${part} leaked`);
}

let iid = 45052;
async function runGate(secrets: object, autoApprove: boolean) {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(iid++, { secrets: secrets as never, auto_approve: autoApprove } as never);
  if (!autoApprove) api.setInputs(claim.run_id, [input("approve_plan")]);
  const persisted: string[] = [];
  const executor: Executor = {
    run: async (ctx) => {
      await ctx.gatePlan!(PLAN, MILESTONES, async (md) => { persisted.push(md); });
      return { branch: ctx.branch };
    },
  };
  await runner(executor, gitlab).execute(claim).catch(() => undefined);
  const reports = api.states.filter((s) => s.runId === claim.run_id && s.body.plan_md !== undefined).map((s) => s.body);
  return { reports, persisted, messages: api.messages(claim.run_id) };
}

describe("provider-bound run: the plan gate redacts plan_md and milestones before the api report", () => {
  for (const [label, auto] of [["human gate (awaiting_approval)", false], ["autopilot (running report)", true]] as const) {
    it(label, async () => {
      const out = await runGate(bound(), auto);
      assert.ok(out.reports.length >= 1, "the gate reported the plan");
      for (const body of out.reports) {
        assertNoKey("report", JSON.stringify({ plan_md: body.plan_md, milestones: body.milestones }));
        assert.match(String(body.plan_md), /^# Plan/, "the plan text still reaches the api");
        assert.ok((body.milestones ?? []).some((m) => m.title === "Plain milestone"), "plain milestone unchanged");
      }
      assertNoKey("plan message", JSON.stringify(out.messages.filter((m) => m.kind === "plan")));
      assertNoKey("persisted hook", out.persisted.join("\n"));
    });
  }

  it("control: a vendor run's plan text is unchanged", async () => {
    const out = await runGate(vendor(), true);
    assert.equal(out.reports[0]!.plan_md, PLAN);
    assert.deepEqual(out.reports[0]!.milestones, MILESTONES);
  });
});
