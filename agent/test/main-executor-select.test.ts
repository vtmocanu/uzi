import { describe, it } from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import path from "node:path";

import { nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { StubExecutor } from "../src/executor.js";
import { SdkExecutor } from "../src/sdk-executor.js";
import { CodexExecutor, FailClosedExecutor } from "../src/codex/codex-executor.js";
import { buildRunExecutor, type BuildRunExecutorDeps } from "../src/main.js";
import type { ClaimCodexSecrets } from "../src/protocol.js";

// PRD #1429 M6 — the harness-neutral e2e stub seam (`agent/src/main.ts`).
//
// Before this milestone, `makeExecutor` checked `selection.kind === "codex"` (building
// the REAL production `CodexExecutor`) *before* it ever looked at
// `config.executor === "stub"` — so `UZI_EXECUTOR=stub` had no effect on a Codex-bound
// claim, and a stub-configured e2e run would still construct and drive the real
// CodexExecutor. The fix reorders the stub short-circuit ahead of the Codex branch;
// this file pins that ordering and proves production (non-stub) is unaffected.
//
// `buildRunExecutor` is the construction logic factored out of `main()`'s closure
// (exported ONLY so this test can call it without importing `main.ts`'s
// side-effecting `main()` — guarded by the argv[1] entrypoint check at the bottom of
// that file, so importing it here does NOT start the worker).

const VALID_SUBSCRIPTION_CODEX: ClaimCodexSecrets = {
  auth_mode: "subscription",
  // Recognizable, fixture-shaped (not provider-token-shaped) — planted so a test can
  // assert the stub path never forwards it anywhere, and it's never a real secret.
  access_token: "fixture-codex-access-token-DO-NOT-USE",
  capability: "fixture-codex-capability-DO-NOT-USE",
  generation: 3,
  chatgpt_account_id: "fixture-account-DO-NOT-USE",
  chatgpt_plan_type: null,
};

const SDK_HOME_ROOT = path.join(os.tmpdir(), "uzi-main-executor-select-test");

function baseDeps(overrides: Partial<BuildRunExecutorDeps> = {}): BuildRunExecutorDeps {
  return {
    log: nullLogger(),
    client: new WorkerClient("http://127.0.0.1:1", "fixture-worker-join-token", "0.1.0-test", nullLogger(), {
      httpTimeoutMs: 1_000,
    }),
    sdkHomeRoot: SDK_HOME_ROOT,
    executorKind: "sdk",
    stubPlanGate: false,
    workerTokenFile: undefined,
    dockerWiring: {},
    codexCommandSandbox: "required",
    codexSandboxDegraded: false,
    ...overrides,
  };
}

describe("buildRunExecutor — the stub-before-codex reorder (PRD #1429 M6)", () => {
  it("stub executor + a codex-bound claim returns StubExecutor, NOT CodexExecutor", () => {
    const result = buildRunExecutor("run-1", VALID_SUBSCRIPTION_CODEX, baseDeps({ executorKind: "stub" }));

    assert.ok(result.executor instanceof StubExecutor, "expected the StubExecutor for a stub-configured codex claim");
    assert.ok(!(result.executor instanceof CodexExecutor), "must NEVER construct the real CodexExecutor under the stub");
    assert.ok(!(result.executor instanceof FailClosedExecutor), "a VALID codex block must not fail closed");
    // The stub is harness-neutral: it gets no per-run homeDir (same as the legacy
    // Claude/stub path), proving it took the shared-root stub branch and never
    // entered the Codex per-run-HOME branch that would release/bind the credential.
    assert.equal(result.homeDir, undefined, "the stub path must not allocate a per-run Codex HOME");
  });

  it("stub executor + an ORDINARY (non-codex) claim still returns StubExecutor (unchanged legacy behavior)", () => {
    const result = buildRunExecutor("run-2", undefined, baseDeps({ executorKind: "stub" }));

    assert.ok(result.executor instanceof StubExecutor);
    assert.ok(!(result.executor instanceof CodexExecutor));
  });

  it("non-stub (sdk) executor + a codex-bound claim STILL selects the real CodexExecutor", () => {
    const result = buildRunExecutor("run-3", VALID_SUBSCRIPTION_CODEX, baseDeps({ executorKind: "sdk" }));

    assert.ok(result.executor instanceof CodexExecutor, "production must still build the real CodexExecutor for a codex claim");
    assert.ok(!(result.executor instanceof StubExecutor));
    assert.equal(result.homeDir, path.join(SDK_HOME_ROOT, "run-3"), "codex path keeps its per-run owned HOME");
  });

  it("non-stub (sdk) executor + an ordinary claim takes the EXACT legacy Claude/SdkExecutor path", () => {
    const result = buildRunExecutor("run-4", undefined, baseDeps({ executorKind: "sdk" }));

    assert.ok(result.executor instanceof SdkExecutor, "the claude control path is untouched by the reorder");
    assert.ok(!(result.executor instanceof CodexExecutor));
    assert.ok(!(result.executor instanceof StubExecutor));
  });

  it("a PRESENT but malformed codex block still fails closed under EITHER executor kind (never a Claude/stub fallback)", () => {
    const malformed = { auth_mode: "bogus" } as unknown as ClaimCodexSecrets;

    for (const executorKind of ["stub", "sdk"] as const) {
      const result = buildRunExecutor("run-5", malformed, baseDeps({ executorKind }));
      assert.ok(
        result.executor instanceof FailClosedExecutor,
        `a broken codex block must fail closed under executorKind=${executorKind}, never silently fall back`,
      );
    }
  });
});
