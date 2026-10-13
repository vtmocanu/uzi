import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";
import { buildClaudeCrossCheckOptions } from "../src/claude-cross-check.js";
import { buildIsolatedSdkOptions, checkIsolatedInit } from "../src/isolated-executor.js";
import { buildJobSdkOptions, laneJobSurface } from "../src/job-runner.js";
import { buildSdkEnv } from "../src/sdk-env.js";
import { nullLogger } from "./helpers.js";

const surface = { tools: ["Read", "Grep", "Glob"], mcpServers: [], plugins: [], skills: [] };
const common = { cwd: "/checkout", log: nullLogger(), secretPaths: ["/worker-secret"], gate: { passed: false } };

it("every isolated lane disables the SDK builtin plugin-authoring plugin", () => {
  const options: SdkOptions[] = [
    ...["plan", "code"].map(stage => buildClaudeCrossCheckOptions({ ...common, homeDir: "/home", oauthToken: "fixture-token", brief: stage })),
    buildIsolatedSdkOptions({ ...common, env: buildSdkEnv("fixture-token", "/home"), fetchServer: {} as never, maxTurns: 1 }),
    ...[[], ["product:approved"]].map(names => buildJobSdkOptions({ env: {}, systemPrompt: "job", workDir: "/checkout",
      log: common.log, secretPaths: common.secretPaths, resultServer: {} as never, toolNames: [],
      ...(names.length ? { productSkills: { pluginPath: "/approved-plugin", names } } : {}),
      lane: { surface: laneJobSurface(names), gate: { passed: false }, fetchServer: {} as never } })),
  ];
  for (const o of options) {
    assert.equal((o.settings as { enabledPlugins: Record<string, boolean> }).enabledPlugins["cc-plugin-plugin-authoring@builtin"], false);
    assert.deepEqual(o.settingSources, []);
  }
});

// Real offline captures with the production checker options and dummy auth, aborted at init.
// Native CLI 2.1.287 (deployed SDK 0.3.287) and 2.1.290 (SDK 0.3.290), 2026-10-13.
// Only surface/version fields are retained. Each version was captured before and after disabling
// cc-plugin-plugin-authoring@builtin. The separate .probe.ts repeats the real CLI experiment.
for (const version of ["2.1.287", "2.1.290"]) {
  const fixtures = JSON.parse(fs.readFileSync(new URL(`./fixtures/claude-checker-init-${version}.json`, import.meta.url), "utf8")) as {
    before: Record<string, unknown>; after: Record<string, unknown>;
  };
  it(`the real CLI ${version} init passes only after builtin plugin suppression`, () => {
    assert.equal(checkIsolatedInit(fixtures.before, surface), "a plugin is loaded");
    assert.equal(checkIsolatedInit(fixtures.after, surface), undefined);
    assert.match(checkIsolatedInit({ ...fixtures.before, plugins: [] }, surface)!, /unexpected skills/);
  });
}
