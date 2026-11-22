import { followSuccessfulPush } from "./publication-fixture.js";
import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { SummaryRunner } from "../src/summary-runner.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import type { ClaimProviderBinding } from "../src/protocol.js";
import { providerKeyForms } from "../src/redact.js";
import { FakePrDescApi } from "./fake-pr-desc-api.js";
import { api, fakeGitlab, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

// The lead's pr_summary reaches the forge PR body through the description publisher. For a provider-bound
// run every string of it must be scrubbed of every encoded form of the gateway key first, on both the
// lead_only rung (no editor pass) and the editor rung (the lead's verification entries are rendered beside
// the editor's text). The key is a synthetic prefixless value assembled at runtime.
const KEY = ["Zq9x", "Lm/4", "tR+7", "&kQ2", "<w8>", "vN3d"].join("");
const FORMS = providerKeyForms(KEY);
const JSON_SLASH = FORMS.find((f) => f.includes("\\/"))!;
const PERCENT = encodeURIComponent(KEY);
const SHA = "1111111111111111111111111111111111111111";
const HAIKU = "claude-haiku-4-5-20251001";

const binding = (): ClaimProviderBinding => ({
  provider_id: "p-1", revision: 1, engine: "claude", protocol: "anthropic_messages", base_url: "https://gw.example.test/a",
  auth_style: "bearer", credential: KEY, model_map: { haiku: { model: "corp-haiku" } },
});
const bound = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "", provider: binding() });
const vendor = () => ({ forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "vendor-oauth-token-000000" });

class LeadExecutor extends StubExecutor {
  override async run(ctx: RunContext): Promise<ExecutorResult> {
    const result = await super.run(ctx);
    return {
      ...result,
      prSummary: {
        what: `Rotates ${KEY} everywhere`,
        why: `because ${JSON_SLASH} leaked`,
        changes: [`change one ${PERCENT}`, "plain change"],
        scope_notes: [{ kind: "deferred", text: `later: ${KEY}` }],
        review_pointers: [`see ${JSON_SLASH}`],
        verification: [{ command: `curl -H "Authorization: ${KEY}"`, result: "pass" }],
        verifiedAtSha: SHA,
      },
    };
  }
}

const homes: string[] = [];
after(async () => { await Promise.all(homes.map((d) => fs.rm(d, { recursive: true, force: true }))); });

let iid = 45051;
async function deliver(secrets: object, editor: boolean) {
  const prApi = new FakePrDescApi();
  api.prDescription = prApi;
  const gl = fakeGitlab();
  followSuccessfulPush(git, gl.pr, true);
  const claim = gitlabClaim(iid++, { secrets: secrets as never, summary_model: "haiku" } as never);
  let extra = {};
  if (editor) {
    const queryFn = (async function* () {
      yield {
        type: "assistant",
        message: { id: "m1", model: HAIKU, role: "assistant",
          content: [{ type: "text", text: JSON.stringify({ summary: "Editor wrote this.", changes: ["c"], scope_notes: [], review_pointers: [] }) }],
          usage: { input_tokens: 2, output_tokens: 2 } },
      };
      yield { type: "result", subtype: "success", is_error: false };
    }) as unknown as SdkQueryFn;
    const homeRoot = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-pk-prsum-"));
    homes.push(homeRoot);
    extra = { summaryRunner: new SummaryRunner(nullLogger(), { queryFn, homeRoot }) };
  }
  await runner(new LeadExecutor(nullLogger()), gl.gitlab, undefined, extra).execute(claim);
  return { description: gl.pr.description, staged: JSON.stringify(prApi.calls) };
}

function assertNoKeyForm(label: string, text: string): void {
  for (const form of FORMS) assert.ok(!text.includes(form), `${label}: no encoded form of the key appears`);
  assert.ok(!text.includes(PERCENT), `${label}: no percent-encoded form appears`);
  // The PR body markdown-escapes punctuation, so also look for the key's unescapable alphanumeric ends.
  for (const part of ["Zq9x", "vN3d"]) assert.ok(!text.includes(part), `${label}: no fragment ${part} appears`);
}

describe("provider-bound run: the lead's pr_summary is redacted before the PR description publisher", () => {
  it("lead_only rung (no editor pass): staged fields and the PR body carry no key form", async () => {
    const out = await deliver(bound(), false);
    assert.match(out.description, /Rotates/, "the lead's text still reaches the body");
    assertNoKeyForm("body", out.description);
    assertNoKeyForm("staged", out.staged);
  });

  it("editor rung: the lead's verification entries in the body carry no key form", async () => {
    const out = await deliver(bound(), true);
    assert.match(out.description, /Editor wrote this/, "the editor rung ran");
    assertNoKeyForm("body", out.description);
    assertNoKeyForm("staged", out.staged);
  });

  it("control: a vendor run's lead claim is unchanged by the redaction", async () => {
    const out = await deliver(vendor(), false);
    assert.ok(out.description.includes("Zq9x") && out.description.includes("vN3d"), "no provider key means nothing to scrub");
  });
});
