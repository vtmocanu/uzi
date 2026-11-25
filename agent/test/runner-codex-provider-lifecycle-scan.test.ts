import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { ClaimCodexProviderBinding } from "../src/protocol.js";
import { LimitReachedError } from "../src/limit.js";
import { client, fakeForgejo, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, commitInTree, FakeCodexExecutor, GIT_ENV, statuses } from "./runner-codex-sinks-fixture.js";
import { HELD_LINUX_ONLY, JOIN_TOKEN, assertCustodyRetained, failingWork, heldServer, skipCheckpoints, type Work } from "./runner-held-publication-fixture.js";

installHarness();

// Real runner/Git/authority; no app-server or live forge. Expected encodings are
// independent literals, assembled at runtime rather than derived by the scanner.
const KEY = ["Zq9x", "Lm/4", "tR+7", "&kQ2", "<w8>", "vN3d"].join("");
const JSON_SLASH = ["Zq9xLm", "\\/", "4tR+7&kQ2<w8>vN3d"].join("");
const PERCENT = ["Zq9xLm", "%2F4tR%2B7%26kQ2%3Cw8%3EvN3d"].join("");
const binding = (): ClaimCodexProviderBinding => ({
  provider_id: "codex-lifecycle-proof", revision: 1, engine: "codex", protocol: "openai_responses",
  base_url: "https://gateway.example.test/v1", auth_style: "bearer", credential: KEY,
  model_map: { default: { model: "corp-codex" } },
});
const rows = [
  { name: "raw", body: KEY, blocked: true },
  { name: "JSON slash", body: JSON_SLASH, blocked: true },
  { name: "percent", body: PERCENT, blocked: true },
  { name: "clean provider", body: "ordinary notes", blocked: false },
  { name: "vendor contaminated control", body: KEY, blocked: false, vendor: true },
  { name: "scanner throws", body: "ordinary notes", blocked: true, scan: "throws" },
  { name: "scanner untrusted", body: "ordinary notes", blocked: true, scan: "untrusted" },
] as const;
let iid = 45200;

for (const forge of ["github", "gitlab", "forgejo"] as const) {
  for (const sink of ["park", "pause", "shutdown", "held"] as const) {
    describe(`Codex lifecycle exact-key ${forge} ${sink}`, () => {
      for (const row of rows) it(row.name, { skip: sink === "held" ? HELD_LINUX_ONLY : false }, async t => {
        const vendor = "vendor" in row;
        const provider = binding();
        const rig = codexRig(vendor ? { authMode: "api_key" } : { provider });
        const claim = gitlabClaim(iid++, {
          wait_on_limit: true,
          ...(sink === "held" ? { claim_generation: 50, inventory_guarded: true } : {}),
          secrets: { forge_pat: "fixture-forge-pat-000000",
            ...(vendor ? { codex: { auth_mode: "api_key", access_token: "fixture-vendor-key", capability: "fixture-vendor-capability" } } : { provider }) },
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
          for await (const chunk of pack) assert.ok(chunk.length);
          uploads++;
          return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/proof" } };
        });
        if ("scan" in row) t.mock.method(git, "scanObjectsForLiterals", async () => {
          if (row.scan === "throws") throw new Error("fixture scanner failure");
          return { trusted: false as const, reason: "read_failed" };
        });
        const server = sink === "held" ? heldServer(claim) : undefined;
        if (server) skipCheckpoints();
        const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-codex-lifecycle-"));
        t.after(() => fs.rmSync(homeRoot, { recursive: true, force: true }));
        let clone = "", tip = "", paused: boolean | undefined, localBody = "";
        let ready!: () => void;
        const started = new Promise<void>(resolve => { ready = resolve; });
        const work: Partial<Work> = {};
        const failed = failingWork(work, ctx => { commitInTree(ctx.worktreePath, "notes.txt", row.body); });
        const executor = new FakeCodexExecutor(rig.safety, async ctx => {
          clone = ctx.worktreePath;
          fs.mkdirSync(path.join(homeRoot, ctx.runId), { recursive: true });
          if (sink === "held") return failed.run(ctx);
          commitInTree(clone, "notes.txt", row.body);
          tip = execFileSync("git", ["-C", clone, "rev-parse", "HEAD"], { env: GIT_ENV, encoding: "utf8" }).trim();
          if (sink === "park") throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
          if (sink === "shutdown") {
            ready();
            await new Promise<void>(resolve => ctx.signal!.addEventListener("abort", () => resolve(), { once: true }));
            throw new Error("worker shutdown");
          }
          await ctx.checkpoint!({ reap: false });
          const at = { completedCount: 1, total: 3 };
          paused = await ctx.parkForPause!(at);
          localBody = execFileSync("git", ["-C", clone, "show", "HEAD:notes.txt"], { env: GIT_ENV, encoding: "utf8" });
          return paused ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
        }, rig.settle, rig.capture);
        const runner = runnerWith(runId => ({ executor, homeDir: path.join(homeRoot, runId) }), gitlab, JOIN_TOKEN, undefined,
          { github, forgejo, checkpointIntervalMs: 0, checkpointTickIntervalMs: 0 });
        const execution = runner.execute(claim);
        if (sink === "shutdown") {
          try {
            await Promise.race([started, execution.then(() => { throw new Error("ended before readiness"); })]);
            runner.shutdown();
            await execution;
          } finally { runner.shutdown(); await execution; }
        } else await execution;
        if (server) {
          assert.equal(server.heldCalls.length, row.blocked ? 0 : 1, "held upload verdict");
          if (row.blocked) {
            // Private recovery archives intentionally retain key-bearing content.
            assert.equal(server.archiveUploads(), 1, "private archive remains available");
            await assertCustodyRetained(server, claim, work, row.name);
          }
        } else {
          assert.equal(uploads, row.blocked ? 0 : 1, "checkpoint upload verdict");
          if (sink === "park") assert.ok(statuses(claim.run_id).includes("limit_wait"), "run parks despite upload refusal");
          if (sink === "pause") {
            assert.equal(paused, !row.blocked, "pause requires durable checkpoint");
            assert.equal(statuses(claim.run_id).includes("paused"), !row.blocked);
            assert.equal(localBody, row.body, "pause preserves committed work");
          } else {
            assert.equal(execFileSync("git", ["-C", git.barePathFor(fx.originPath), "show", `${tip}:notes.txt`], { env: GIT_ENV, encoding: "utf8" }), row.body,
              "park/shutdown retains committed content in the local bare repository");
          }
        }
        if (!vendor) {
          assert.equal(rig.releaseCalls(), 0, "no vendor release");
          assert.equal(rig.refreshCalls(), 0, "no vendor refresh");
          assert.ok(rig.registeredTokens.every(token => token === KEY), "no vendor credential registered");
        }
      });
    });
  }
}
