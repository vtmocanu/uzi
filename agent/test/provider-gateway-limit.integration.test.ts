import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import os from "node:os";
import path from "node:path";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Options } from "@anthropic-ai/claude-agent-sdk";
import { buildSdkEnv } from "../src/sdk-env.js";
import { isGatewayLimitStatus } from "../src/provider-binding.js";

// PRD #4320 D11/D15 characterization: how the CLI (characterized at SDK 0.3.319 / CLI 2.1.319) terminates a turn
// against a gateway that answers a persistent 429 or 529. Observed 2026-11-18 with the CLI's DEFAULT
// retry budget: 10 api_retry frames over roughly 180 s (backoff 0.6 s growing to ~36 s), then the
// terminal frame. This test shortens that with CLAUDE_CODE_MAX_RETRIES=1 (test-only) and asserts the
// terminal shape the worker keys on: a `result` frame with is_error true, terminal_reason "api_error"
// and the numeric api_error_status, and NO rate_limit_event frame (a gateway has no subscription window).
const KEY = ["gw", "key", "do-not-scan", "1010", "abcdefgh"].join("-");

describe("the pinned CLI's terminal frame for a persistent gateway 429 / 529", () => {
  const roots: string[] = [];
  after(() => { for (const r of roots) fs.rmSync(r, { recursive: true, force: true }); });

  for (const status of [429, 529]) {
    it(`${status}: api_retry frames, then an api_error result carrying api_error_status ${status}`, { timeout: 120_000 }, async () => {
      let posts = 0;
      const server = http.createServer((req, res) => {
        req.resume();
        req.on("end", () => {
          if (req.method === "POST" && (req.url ?? "").startsWith("/v1/messages")) posts++;
          res.writeHead(status, { "content-type": "application/json" });
          res.end(JSON.stringify({ type: "error", error: { type: status === 529 ? "overloaded_error" : "rate_limit_error", message: "gateway says no" } }));
        });
      });
      await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
      server.unref();
      const port = (server.address() as AddressInfo).port;
      const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-gwlimit-it-"));
      roots.push(root);
      const home = path.join(root, "home");
      const worktree = path.join(root, "worktree");
      const tmp = path.join(root, "tmp");
      for (const d of [path.join(home, ".claude"), worktree, tmp]) fs.mkdirSync(d, { recursive: true });
      fs.writeFileSync(path.join(home, ".claude", "settings.json"), "{}\n");
      const env = {
        ...buildSdkEnv({
          kind: "provider", baseUrl: `http://127.0.0.1:${port}`, authStyle: "bearer", key: KEY,
          modelMap: { sonnet: { model: "corp-sonnet" }, haiku: { model: "corp-haiku" } },
        }, home),
        // The real CLI creates cc-socks and claude-<uid> under TMPDIR. Keep them
        // inside the test root removed by the suite's existing cleanup.
        TMPDIR: tmp,
        DISABLE_TELEMETRY: "1", DISABLE_AUTOUPDATER: "1", CLAUDE_CODE_MAX_RETRIES: "1",
      } as Record<string, string | undefined>;
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), 60_000);
      const options: Options = {
        cwd: worktree, env, model: "sonnet", permissionMode: "bypassPermissions", allowDangerouslySkipPermissions: true,
        settingSources: [], abortController: controller,
      };
      const frames: Array<Record<string, unknown>> = [];
      try {
        for await (const m of query({ prompt: "hi", options })) frames.push(m as unknown as Record<string, unknown>);
      } catch {
        // the SDK throws after yielding an error result; the frames are what is asserted
      } finally {
        clearTimeout(timer);
        server.closeAllConnections();
        await new Promise<void>((r) => server.close(() => r()));
      }
      const retries = frames.filter((f) => f.type === "system" && f.subtype === "api_retry");
      assert.ok(retries.length >= 1, "the CLI absorbed the first answer as an api_retry");
      assert.ok(retries.every((f) => f.error_status === status), "every retry names the gateway status");
      assert.ok(frames.every((f) => f.type !== "rate_limit_event"), "no subscription rate-limit event exists for a gateway");
      const result = frames.find((f) => f.type === "result");
      assert.ok(result, "the turn terminated with a result frame");
      assert.equal(result.is_error, true);
      assert.equal(result.terminal_reason, "api_error");
      assert.equal(result.api_error_status, status);
      assert.ok(isGatewayLimitStatus(result.api_error_status as number), "the worker classifies it as a gateway limit");
      assert.ok(posts >= 2, `the CLI retried the call (${posts} POSTs)`);
    });
  }
});
