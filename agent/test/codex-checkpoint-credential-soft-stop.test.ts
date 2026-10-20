import assert from "node:assert/strict";
import { it } from "node:test";
import { CheckpointSoftDeadlineError } from "../src/checkpoint-stop.js";
import { createRunLaneInstallerReconcile } from "../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { createCodexExecutionSafety } from "../src/codex/safety.js";
import { selectCodexBinding } from "../src/codex/select.js";
import type { BoundaryRequest } from "../src/harness.js";
import { PauseNowSignal } from "../src/steering.js";

for (const authMode of ["subscription", "api_key"] as const) {
  for (const stopAt of ["before exchange", "during exchange", "during cooperative pause"] as const) {
    it(`${authMode} checkpoint soft stop ${stopAt} does not poison credential reconciliation`, async () => {
      const soft = new AbortController(), lifecycle = new AbortController();
      const capturedLifecycle = new AbortController();
      const selected = selectCodexBinding({ codex: {
        auth_mode: authMode, access_token: "fixture", capability: "fixture",
        ...(authMode === "subscription" ? { generation: 3, chatgpt_account_id: "fixture-account", chatgpt_plan_type: null } : {}),
      } });
      if (selected.kind !== "codex") assert.fail("binding");
      const committed = { value: authMode === "subscription" ? 3 : undefined };
      const tokens: string[] = [];
      const exchangeSignals: boolean[] = [];
      let calls = 0, vaultChecks = 0;
      const exchange = async (_runId: string, body: { observed_generation?: number }, _expected: unknown, signal?: AbortSignal) => {
        calls++;
        if (stopAt !== "before exchange" && calls === 1) {
          // The HTTP operation has started. A cooperative scheduling timeout must
          // not cancel it and manufacture an unknown credential outcome.
          soft.abort();
          if (stopAt === "during cooperative pause") capturedLifecycle.abort(new PauseNowSignal());
          await Promise.resolve();
          exchangeSignals.push(signal?.aborted ?? false);
        }
        return { auth_mode: authMode, access_token: "released-fixture", generation: (body.observed_generation ?? 3) + 1 };
      };
      const installer = createRunLaneInstallerReconcile(lifecycle.signal, {
        runId: "credential-soft-stop", binding: selected.binding,
        client: { releaseCodex: exchange, refreshCodex: exchange } as never,
        registerToken: token => tokens.push(token), committed,
        isVaultLocked: () => {
          if (++vaultChecks === 2 && stopAt === "before exchange") soft.abort();
          return false;
        },
      });
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(313203));
      const safety = createCodexExecutionSafety(registry, async () => assert.fail("no root spawn"), installer.reconcile);
      const request: BoundaryRequest = {
        boundary: "checkpoint", sink: "done_checkpoint", deadlineMs: 90_000,
        checkpointStop: { signal: soft.signal, lifecycleSignal: capturedLifecycle.signal,
          cooperativeLifecycleReason: reason => reason instanceof PauseNowSignal },
      };
      await assert.rejects(safety.withBoundary(request, async () => assert.fail("no action after the soft stop")), CheckpointSoftDeadlineError);
      assert.equal(registry.isPoisoned(), false, "a clean scheduling stop leaves the registry reusable");
      if (stopAt === "before exchange") {
        assert.equal(calls, 0, "a pre-exchange soft stop makes no credential call");
        assert.deepEqual(tokens, []);
        assert.equal(committed.value, authMode === "subscription" ? 3 : undefined);
      } else {
        assert.equal(calls, 1);
        assert.deepEqual(exchangeSignals, [false], "only hard/authority cancellation governs a started exchange");
        assert.deepEqual(tokens, ["released-fixture"]);
        if (authMode === "subscription") assert.equal(committed.value, 4, "the validated response commits before the clean stop");
      }
      let continued = false;
      await safety.withBoundary({ boundary: "checkpoint", deadlineMs: 90_000 }, async () => { continued = true; });
      assert.equal(continued, true, "a later boundary can reconcile and run normally");
      assert.equal(registry.isPoisoned(), false);
    });
  }
}
