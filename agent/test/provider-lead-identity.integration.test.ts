import { after, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Options } from "@anthropic-ai/claude-agent-sdk";
import { buildSdkEnv } from "../src/sdk-env.js";
import { resolveProviderModel } from "../src/provider-binding.js";
import { decodeObservedModel } from "../src/runtime-observation.js";
import { FakeAnthropicApi } from "./fake-anthropic-api.js";

// PRD #4574 M2 (riskiest assumption): with ANTHROPIC_DEFAULT_*_MODEL set from a frozen provider model
// map (buildSdkEnv, the production env), does the REAL bundled CLI both REPORT (init frame, which is what
// the lead-config reporter reads as observed_model) and SEND (the `model` the gateway receives) the map's
// TARGET for an alias request and for a blank request (which resolves through `sonnet`)?

const MODEL_MAP = {
  opus: { model: "corp-opus-target" },
  sonnet: { model: "corp-sonnet-target" },
  haiku: { model: "corp-haiku-target" },
};

describe("provider lead identity: the CLI reports and sends the frozen-map target (PRD #4574 M2)", () => {
  const roots: string[] = [];
  after(() => {
    for (const r of roots) fs.rmSync(r, { recursive: true, force: true });
  });

  async function drive(requested: string | undefined): Promise<{ resolved: string; initModel: string; wireModels: string[] }> {
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-leadid-it-"));
    roots.push(root);
    const home = path.join(root, "home");
    const worktree = path.join(root, "worktree");
    for (const d of [path.join(home, ".claude"), worktree]) fs.mkdirSync(d, { recursive: true });
    fs.writeFileSync(path.join(home, ".claude", "settings.json"), "{}\n");
    const fake = new FakeAnthropicApi(() => ({ kind: "text", text: "ok" }));
    const baseUrl = await fake.listen();
    const prod = buildSdkEnv(
      { kind: "provider", baseUrl, authStyle: "bearer", key: ["dummy", "token"].join("-"), modelMap: MODEL_MAP },
      home,
    );
    const env: Record<string, string> = {};
    for (const [k, v] of Object.entries(prod)) if (typeof v === "string") env[k] = v;
    env.PATH = process.env.PATH ?? "";
    env.TMPDIR = root;
    env.DISABLE_TELEMETRY = "1";
    env.DISABLE_AUTOUPDATER = "1";
    const resolved = resolveProviderModel(requested, MODEL_MAP);
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 60_000);
    const options: Options = {
      cwd: worktree,
      env,
      model: resolved,
      permissionMode: "bypassPermissions",
      allowDangerouslySkipPermissions: true,
      settingSources: [],
      abortController: controller,
    };
    let initModel = "";
    try {
      for await (const m of query({ prompt: "say ok", options })) {
        const mm = m as { type: string; subtype?: string; model?: string };
        if (mm.type === "system" && mm.subtype === "init") initModel = decodeObservedModel(mm.model) ?? "";
      }
    } finally {
      clearTimeout(timer);
      await fake.close();
    }
    const wireModels = fake.messageBodies.map((b) => b.model ?? "");
    return { resolved, initModel, wireModels };
  }

  it("alias request (opus): init model and gateway model are the mapped target, not the alias", { timeout: 120_000 }, async () => {
    const o = await drive("opus");
    assert.equal(o.resolved, "opus");
    assert.equal(o.initModel, MODEL_MAP.opus.model, `init model; wire=${JSON.stringify(o.wireModels)}`);
    assert.ok(o.wireModels.includes(MODEL_MAP.opus.model), `gateway saw ${JSON.stringify(o.wireModels)}`);
    assert.ok(!o.wireModels.includes("opus"), `the alias must not reach the gateway: ${JSON.stringify(o.wireModels)}`);
  });

  it("blank request: resolves through sonnet; init model and gateway model are the sonnet target", { timeout: 120_000 }, async () => {
    const o = await drive("");
    assert.equal(o.resolved, "sonnet");
    assert.equal(o.initModel, MODEL_MAP.sonnet.model, `init model; wire=${JSON.stringify(o.wireModels)}`);
    assert.ok(o.wireModels.includes(MODEL_MAP.sonnet.model), `gateway saw ${JSON.stringify(o.wireModels)}`);
    assert.ok(!o.wireModels.includes("sonnet"), `the alias must not reach the gateway: ${JSON.stringify(o.wireModels)}`);
  });
});
