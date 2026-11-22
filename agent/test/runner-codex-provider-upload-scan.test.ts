import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import type { ClaimCodexProviderBinding } from "../src/protocol.js";
import { providerKeyForms } from "../src/redact.js";
import {
  api, client, fakeForgejo, fakeGitHub, fakeGitlab, fx, git, gitlabClaim,
  installHarness, runnerWith,
} from "./runner-harness.js";
import { JOIN_TOKEN } from "./runner-held-publication-fixture.js";
import { codexRig, commitInTree, FakeCodexExecutor, GIT_ENV } from "./runner-codex-sinks-fixture.js";

installHarness();

// Fake executor, REAL RunRunner/Git/safety/registry/reconcile. This does not run app-server.
// The runtime-assembled prefixless value is deliberately invisible to generic token scanners.
const KEY = ["Zq9x", "Lm/4", "tR+7", "&kQ2", "<w8>", "vN3d"].join("");
const FORMS = providerKeyForms(KEY);
const provider = (): ClaimCodexProviderBinding => ({
  provider_id: "codex-upload-proof", revision: 1, engine: "codex", protocol: "openai_responses",
  base_url: "https://gateway.example.test/v1", auth_style: "bearer", credential: KEY,
  model_map: { default: { model: "corp-codex" } },
});
const vendor = { auth_mode: "api_key", access_token: "fixture-vendor-key", capability: "fixture-vendor-capability" };
let iid = 45100;
const cases = [
  { name: "raw", body: KEY, blocked: true },
  { name: "JSON slash", body: FORMS.find(f => f.includes("\\/"))!, blocked: true },
  { name: "percent encoded", body: encodeURIComponent(KEY), blocked: true },
  { name: "clean provider", body: "ordinary notes", blocked: false },
  { name: "vendor contaminated control", body: KEY, blocked: false, vendor: true },
  { name: "scanner throws", body: "ordinary notes", blocked: true, scan: "throws" },
  { name: "scanner untrusted", body: "ordinary notes", blocked: true, scan: "untrusted" },
] as const;

function branchExists(branch: string): boolean {
  try {
    execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", "--quiet", `refs/heads/${branch}`],
      { env: GIT_ENV, stdio: "pipe" });
    return true;
  } catch { return false; }
}

for (const forge of ["github", "gitlab", "forgejo"] as const) {
  for (const sink of ["checkpoint", "finalize"] as const) {
    describe(`Codex exact-key ${forge} ${sink}`, () => {
      for (const row of cases) it(row.name, async t => {
        assert.ok(row.body, "encoded fixture exists");
        const vendorMode = "vendor" in row;
        const binding = provider();
        const rig = codexRig(vendorMode ? { authMode: "api_key" } : { provider: binding });
        const { gitlab } = fakeGitlab();
        const { github } = fakeGitHub();
        const { forgejo } = fakeForgejo();
        const claim = gitlabClaim(iid++, {
          secrets: { forge_pat: "fixture-forge-pat-000000",
            ...(vendorMode ? { codex: vendor } : { provider: binding }) },
          repo: { id: "r1", url: `https://${forge}.example.test/org/repo`,
            clone_url: fx.originPath, default_branch: "main", forge_type: forge },
        });
        assert.equal(claim.secrets.anthropic_oauth_token, undefined, "exclusive Codex claim");
        assert.equal(vendorMode ? claim.secrets.provider : claim.secrets.codex, undefined);
        let uploads = 0;
        t.mock.method(client, "publishCheckpoint", async (_run: string, _tip: string, pack: Readable) => {
          for await (const chunk of pack) assert.ok(chunk.length);
          uploads++;
          return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/proof" } };
        });
        if ("scan" in row) t.mock.method(git, "scanObjectsForLiterals", async () => {
          if (row.scan === "throws") throw new Error("fixture scanner failure");
          return { trusted: false as const, reason: "read_failed" };
        });
        let continued = false;
        let localBody = "";
        const executor = new FakeCodexExecutor(rig.safety, async ctx => {
          commitInTree(ctx.worktreePath, "notes.txt", row.body);
          if (sink === "checkpoint") {
            await ctx.checkpoint!({ reap: true });
            // Overlay-less Codex milestones deliberately defer scanning out of the permit.
            // Settle that owed publication through the next iteration checkpoint.
            if (forge !== "github") await ctx.checkpoint!({ reap: false });
            continued = true;
            localBody = execFileSync("git", ["-C", ctx.worktreePath, "show", "HEAD:notes.txt"],
              { env: GIT_ENV, encoding: "utf8" });
          }
          return { branch: ctx.branch };
        }, rig.settle, rig.capture);
        await runnerWith(() => ({ executor }), gitlab, JOIN_TOKEN, undefined,
          { github, forgejo, checkpointIntervalMs: 1 }).execute(claim);
        assert.ok(rig.boundaries.includes(sink), "real safety boundary reached");
        if (sink === "checkpoint") {
          assert.equal(uploads, row.blocked ? 0 : 1, "checkpoint upload verdict");
          assert.equal(continued, true, "checkpoint refusal preserves execution");
          assert.equal(localBody, row.body, "checkpoint refusal preserves local committed content");
        }
        assert.equal(branchExists(`agent/issue-${claim.issue_iid}`), !row.blocked, "final branch upload verdict");
        if (row.blocked) {
          const failed = api.states.find(s => s.runId === claim.run_id && s.body.status === "failed");
          assert.equal(failed?.body.fail_origin, "push_secret_blocked");
          const publicText = JSON.stringify({ states: api.states.filter(s => s.runId === claim.run_id),
            feed: api.messages(claim.run_id) });
          for (const form of FORMS) assert.ok(!publicText.includes(form), "no key in public reports");
        }
        if (!vendorMode) {
          assert.equal(rig.releaseCalls(), 0, "provider never releases vendor credentials");
          assert.equal(rig.refreshCalls(), 0, "provider never refreshes vendor credentials");
          assert.ok(rig.registeredTokens.length > 0, "provider key registered for redaction");
          assert.deepEqual([...new Set(rig.registeredTokens)], [KEY], "reconcile registers only the provider key, no vendor credential");
        }
      });
    });
  }
}
