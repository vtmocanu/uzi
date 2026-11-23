import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import type { Readable } from "node:stream";
import type { ClaimCodexProviderBinding } from "../src/protocol.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { providerKeyForms } from "../src/redact.js";
import {
  api, client, fakeForgejo, fakeGitHub, fakeGitlab, fx, git, gitlabClaim,
  installHarness, runnerWith,
} from "./runner-harness.js";
import { JOIN_TOKEN } from "./runner-held-publication-fixture.js";
import { codexRig, commitInTree, FakeCodexExecutor, GIT_ENV } from "./runner-codex-sinks-fixture.js";

installHarness();

// Fake executor; real RunRunner, Git, safety, registry and provider reconcile.
// Capture here means transient-recovery's verified local restore point followed by
// best-effort forge checkpoint publication, NOT the private encrypted archive.
// The archive deliberately retains contaminated bytes and is not scan coverage.
const KEY = ["Zq9x", "Lm/4", "tR+7", "&kQ2", "<w8>", "vN3d"].join("");
const JSON_SLASH = ["Zq9xLm", "\\/", "4tR+7&kQ2<w8>vN3d"].join("");
const PERCENT = ["Zq9xLm", "%2F4tR%2B7%26kQ2%3Cw8%3EvN3d"].join("");
const provider = (): ClaimCodexProviderBinding => ({
  provider_id: "codex-capture-proof", revision: 1, engine: "codex", protocol: "openai_responses",
  base_url: "https://gateway.example.test/v1", auth_style: "bearer", credential: KEY,
  model_map: { default: { model: "corp-codex" } },
});
const rows = [
  { name: "raw", body: KEY, blocked: true },
  { name: "JSON slash", body: JSON_SLASH, blocked: true },
  { name: "percent encoded", body: PERCENT, blocked: true },
  { name: "scanner throws", body: "ordinary notes", blocked: true, scan: "throws" },
  { name: "scanner untrusted", body: "ordinary notes", blocked: true, scan: "untrusted" },
  { name: "clean provider", body: "ordinary notes", blocked: false },
  { name: "vendor contaminated control", body: KEY, blocked: false, vendor: true },
] as const;
let iid = 45300;

for (const sink of ["overlay", "capture"] as const) {
  // Only GitHub's milestone reap swaps the workflow tree. Recovery's actual
  // checkpoint upload exists on every forge, independently of private archives.
  const forges = sink === "overlay" ? ["github"] as const : ["github", "gitlab", "forgejo"] as const;
  for (const forge of forges) describe(`Codex provider ${forge} ${sink} publication`, () => {
    for (const row of rows) it(row.name, async t => {
      const vendor = "vendor" in row;
      const binding = provider();
      const rig = codexRig(vendor ? { authMode: "api_key" } : { provider: binding });
      const claim = gitlabClaim(iid++, {
        secrets: { forge_pat: "fixture-forge-pat-000000",
          ...(vendor ? { codex: { auth_mode: "api_key", access_token: "fixture-vendor-key", capability: "fixture-vendor-capability" } }
            : { provider: binding }) },
        repo: { id: "r1", url: `https://${forge}.example.test/org/repo`,
          clone_url: fx.originPath, default_branch: "main", forge_type: forge },
      });
      assert.equal(claim.secrets.anthropic_oauth_token, undefined, "exclusive Codex claim");
      assert.equal(vendor ? claim.secrets.provider : claim.secrets.codex, undefined);
      const { gitlab } = fakeGitlab();
      const { github } = fakeGitHub();
      const { forgejo } = fakeForgejo();
      let uploads = 0;
      t.mock.method(client, "publishCheckpoint", async (_run: string, _tip: string, pack: Readable) => {
        let bytes = 0;
        for await (const chunk of pack) bytes += chunk.length;
        assert.ok(bytes > 0, "real checkpoint pack reached the forge transport seam");
        uploads++;
        return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/proof" } };
      });
      const overlays: unknown[] = [];
      const pack = git.checkpointPack.bind(git);
      t.mock.method(git, "checkpointPack", (...args: Parameters<typeof pack>) => {
        overlays.push(args[2]);
        return pack(...args);
      });
      // Credentialed default fetch fails soft to the real local tip; no live network.
      t.mock.method(git, "fetchDefaultTip", async () => { throw new Error("fixture: offline default tip"); });
      const scan = git.scanObjectsForLiterals.bind(git);
      let scans = 0;
      t.mock.method(git, "scanObjectsForLiterals", async (...args: Parameters<typeof scan>) => {
        scans++;
        if ("scan" in row) {
          if (row.scan === "throws") throw new Error("fixture scanner failure");
          return { trusted: false as const, reason: "read_failed" };
        }
        return scan(...args);
      });
      let continued = false;
      let localBody = "";
      const executor = new FakeCodexExecutor(rig.safety, async ctx => {
        commitInTree(ctx.worktreePath, "notes.txt", row.body);
        if (sink === "capture") throw new TransientRecoveryError("provider capture fixture");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        continued = true;
        localBody = execFileSync("git", ["-C", ctx.worktreePath, "show", "HEAD:notes.txt"],
          { env: GIT_ENV, encoding: "utf8" });
        return { branch: ctx.branch };
      }, rig.settle, rig.capture);
      await runnerWith(() => ({ executor }), gitlab, JOIN_TOKEN, undefined,
        { github, forgejo, recoveryRetryMs: 1, checkpointIntervalMs: sink === "overlay" ? 1000 : 0,
          checkpointTickIntervalMs: 0 }).execute(claim);
      assert.equal(uploads, row.blocked ? 0 : 1, "forge checkpoint upload verdict");
      assert.ok(overlays.length > 0, "real checkpoint pack path reached");
      if (sink === "overlay") {
        assert.ok(overlays.every(context => context !== undefined), "actual overlay context on every pack");
        assert.equal(continued, true, "upload refusal does not stop execution");
        assert.equal(localBody, row.body, "checkpoint preserves local committed work");
      } else {
        assert.ok(rig.boundaries.includes("shutdown"), "capture publication uses shutdown boundary");
        assert.ok(api.states.some(s => s.runId === claim.run_id && s.body.status === "recovery_wait"),
          "verified capture parks even when publication is refused");
        assert.equal(execFileSync("git", ["-C", git.barePathFor(fx.originPath), "show",
          `refs/uzi-runner/agent/issue-${claim.issue_iid}:notes.txt`], { env: GIT_ENV, encoding: "utf8" }),
          row.body, "verified local restore point retains committed work");
      }
      if (!vendor) {
        assert.ok(scans > 0, "provider exact-key scanner executed");
        const publicText = JSON.stringify({ states: api.states.filter(s => s.runId === claim.run_id),
          feed: api.messages(claim.run_id) });
        for (const form of providerKeyForms(KEY)) assert.ok(!publicText.includes(form), "no key in public report");
        // These are fixture reconcile counters, not live provider API observations.
        assert.equal(rig.releaseCalls(), 0, "provider fixture never releases vendor credentials");
        assert.equal(rig.refreshCalls(), 0, "provider fixture never refreshes vendor credentials");
        assert.deepEqual([...new Set(rig.registeredTokens)], [KEY], "only provider key registered");
      } else assert.equal(scans, 0, "vendor control bypasses provider exact-key policy");
    });
  });
}
