import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Executor } from "../src/executor.js";
import type { ClaimProviderBinding } from "../src/protocol.js";
import { providerKeyForms } from "../src/redact.js";
import { api, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

// The report-only completion carries the lead's `summary` (persisted as report_md) and, on a prompt
// run, a structured proposal. Both are model-authored and POSTed to the api, whose pattern scrub
// cannot catch a prefixless provider key, so the worker redacts them first. The key is synthetic.
const KEY = ["Hd6y", "Nc/3", "uV+9", "&pB1", "<z5>", "eT8m"].join("");
const FORMS = providerKeyForms(KEY);

const binding = (): ClaimProviderBinding => ({
  provider_id: "p-1", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/a",
  auth_style: "bearer", credential: KEY, model_map: { haiku: { model: "corp-haiku" } },
});
const bound = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "", provider: binding() });
const vendor = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000" });

function assertNoKey(label: string, text: string): void {
  for (const f of FORMS) assert.ok(!text.includes(f), `${label}: form leaked: ${f}`);
  for (const part of ["Hd6y", "eT8m"]) assert.ok(!text.includes(part), `${label}: fragment ${part} leaked`);
}

let iid = 45053;
async function complete(secrets: object, kind: "issue" | "prompt") {
  const { gitlab } = fakeGitlab();
  if (kind === "prompt") git.changedFiles = (async () => []) as typeof git.changedFiles;
  const claim = gitlabClaim(iid++, { secrets: secrets as never, ...(kind === "prompt" ? { kind: "prompt" } : {}) } as never);
  const exec: Executor = {
    run: async (ctx) => ({
      branch: ctx.branch, ...(kind === "issue" ? { reportOnly: true } : {}), summary: `findings: header ${KEY} was echoed`,
      ...(kind === "prompt" ? { proposal: { title: `rotate ${KEY}`, body: `body ${FORMS[FORMS.length - 1]!}` } } : {}),
    }),
  };
  await runner(exec, gitlab).execute(claim);
  return api.states.find((s) => s.runId === claim.run_id && s.body.status === "completed")!.body;
}

describe("provider-bound run: report-only completion redacts report_md and the proposal", () => {
  it("issue run report_md", async () => {
    const body = await complete(bound(), "issue");
    assert.match(String(body.report_md), /^findings: header /);
    assertNoKey("report_md", String(body.report_md));
  });

  it("prompt run report_md and proposal", async () => {
    const body = await complete(bound(), "prompt");
    assertNoKey("report_md", String(body.report_md));
    assert.ok(body.proposal, "the proposal still reaches the api");
    assertNoKey("proposal", JSON.stringify(body.proposal));
  });

  it("control: a vendor run's summary is unchanged", async () => {
    const body = await complete(vendor(), "issue");
    assert.equal(body.report_md, `findings: header ${KEY} was echoed`);
  });
});
