import { it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { runReadOnlyModelPass, type ReadOnlyModelPassOpts } from "../src/model-pass.js";
import { decodeProviderBinding, ProviderLaneUnsupportedError } from "../src/provider-binding.js";
import type { Logger } from "../src/log.js";

const scratch = fileURLToPath(new URL("../../.uzi/scratch/", import.meta.url));

for (const engine of ["codex", "unknown", "claude"]) {
  for (const withCodex of [false, true]) {
    if (engine === "claude" && !withCodex) continue;
    it(`refuses ${engine} provider before side effects (native Codex options: ${withCodex})`, async (t) => {
      const provider = decodeProviderBinding({ provider: {
        provider_id: "held-provider",
        revision: 1,
        engine: engine === "claude" ? "claude" : "codex",
        protocol: engine === "claude" ? "anthropic_messages" : "openai_responses",
        base_url: "https://gateway.example/v1",
        auth_style: "bearer",
        credential: "held-provider-fixture-key",
        model_map: engine === "claude" ? { haiku: { model: "gateway-model" } } : { default: { model: "gateway-model" } },
      } });
      assert.ok(provider);
      // Exercise the runtime boundary even for callers that bypass the claim decoder.
      if (engine === "unknown") Object.assign(provider, { engine });
      await fs.mkdir(scratch, { recursive: true });
      const root = await fs.mkdtemp(path.join(scratch, "model-pass-hold-"));
      const effects: string[] = [];
      const record = (name: string) => () => { effects.push(name); };
      const log: Logger = {
        debug: record("debug"), info: record("info"), warn: record("warn"), error: record("error"),
        addSecret: record("addSecret"), removeSecret: record("removeSecret"),
        child: () => { effects.push("child"); return log; },
        redactDiagnostic: (text) => { effects.push("redactDiagnostic"); return text; },
      };
      const mkdtemp = fs.mkdtemp.bind(fs);
      t.mock.method(fs, "mkdtemp", (...args: Parameters<typeof fs.mkdtemp>) => {
        effects.push("home");
        return mkdtemp(...args);
      });
      // In the unfixed path the timer is outside cleanup. Keep the regression hermetic
      // even when it fails, without letting a leaked timer reject after the test ends.
      t.mock.method(globalThis, "setTimeout", () => {
        effects.push("timer");
        return { unref() {} };
      });
      t.mock.method(globalThis, "clearTimeout", record("clearTimer"));
      const opts: ReadOnlyModelPassOpts = {
        provider, token: "unused-vendor-token",
        homeRoot: root, homePrefix: "pass-", label: "review",
        timeoutMs: 5000, systemPrompt: "system", prompt: "prompt", denyReason: "no tools",
        queryFn: () => { effects.push("query"); throw new Error("query must not run"); },
        onUsage: record("usage"), onResult: record("result"), log,
        ...(withCodex ? { codex: {
          runId: "held-pass",
          binding: { authMode: "api_key", accessToken: "native-fixture", capability: "cap" } as never,
          buildHarness: async () => { effects.push("harness"); throw new Error("harness must not run"); },
        } } : {}),
      };
      try {
        const error = await runReadOnlyModelPass(opts).then(
          () => undefined,
          (err: unknown) => err,
        );
        assert.ok(error instanceof ProviderLaneUnsupportedError);
        assert.equal(error.name, "ProviderLaneUnsupportedError");
        assert.equal(error.reason, "provider_lane_unsupported");
        assert.equal(error.message, "provider_lane_unsupported");
        assert.deepEqual(effects, [], "refusal must precede allocations, callbacks and secret registration");
        assert.deepEqual(await fs.readdir(root), []);
      } finally {
        t.mock.restoreAll();
        await fs.rm(root, { recursive: true, force: true });
      }
    });
  }
}
