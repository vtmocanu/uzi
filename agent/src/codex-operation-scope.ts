import { randomUUID } from "node:crypto";
import type { WorkerClient } from "./client.js";
import { CodexOperationController, type CodexOperationWaitPolicy } from "./codex-operation-controller.js";
import type { CodexOperationIdentity } from "./protocol.js";

/** Worker-owned provider-send inventory (all four turn/start sinks):
 * codex-harness.ts startTurnRpc: lead planning/implementation, resumed roots,
 *   revision/recovery and model fallback turns (CodexExecutor.driveTurnWithWallPark).
 * codex-executor.ts startChildTurn: synchronous spawn_agent and collaboration aliases;
 *   the root awaits this required child, so its admission wait may label the root.
 * codex-advice-harness.ts startTurnRpc: JudgeRunner, ReviewRunner, SummaryRunner
 *   intent/plan/delivery and NowSummaryController via model-pass.ts and
 *   makeProductionCodexAdviceHarnessFactory. Only standalone judge/review work is
 *   required; optional summaries never label the root. plan-contract-review.ts has
 *   a bounded-test-only model-pass caller; live coverage currently makes zero calls.
 * cross-check.ts run: independent plan/code CrossCheckRunner claims.
 * Caller-key pr-description evaluator and offline code-cross-check replay are excluded:
 * they have no worker claim/account authority and do not use the production factory.
 * Native provider retries, compaction and tool continuations within an already sent
 * turn have no fresh worker send boundary; admission cannot intercept those requests.
 * Existing feature negotiation/rollout flags remain the only activation switches.
 */
type Kind = CodexOperationIdentity["kind"];
export type PrepareCodexOperation = (signal: AbortSignal, check: () => void, kind?: Kind) => Promise<() => boolean | void>;
type Client = Partial<Pick<WorkerClient, "hasFeature" | "codexOperationNegotiation" | "checkCodexOperationAdmission" | "retainCodexOperationFlight">>;
interface Scope {
  controller: CodexOperationController;
  prepare?: PrepareCodexOperation;
  users: number;
  release?: () => void;
  ownerClosed: AbortController;
  waiters: Set<(operation: CodexOperationIdentity) => (() => void) | undefined>;
}
const scopes = new WeakMap<Client, Map<string, Scope>>();

/** Leases live for the owning flight or whole advice/checker call. Independent
 * operations share scheduling and negotiation history, never an admitted reply. */
export function acquireCodexOperationScope(client: Client, runId: string, generation: number,
  policy: CodexOperationWaitPolicy = {}, waiting: (operation: CodexOperationIdentity) => (() => void) | undefined = () => undefined,
  requiredKinds: readonly Kind[] = ["root"]) {
  let runs = scopes.get(client);
  if (!runs) { runs = new Map(); scopes.set(client, runs); }
  const key = JSON.stringify([runId, generation]);
  let scope = runs.get(key);
  if (!scope) {
    const created: Scope = { controller: undefined!, users: 0, ownerClosed: new AbortController(), waiters: new Set() };
    created.controller = new CodexOperationController(generation, {
      ...policy,
      supported: () => client.hasFeature?.("codex_account_operation_admission_v1") ?? false,
      negotiation: () => client.codexOperationNegotiation ?? 0,
      read: (request, signal) => client.checkCodexOperationAdmission?.(runId, request, signal) ?? Promise.reject(new Error("operation admission unavailable")),
      engage: () => { created.release = client.retainCodexOperationFlight?.(runId, generation); },
      // Each owner selects required work; optional advice supplies no callback.
      waiting: operation => {
        const clears = [...created.waiters].map(wait => wait(operation));
        return () => { for (const clear of clears) clear?.(); };
      },
    });
    scope = created;
    runs.set(key, scope);
  }
  const owned = scope;
  owned.users++;
  const requiredWait = (operation: CodexOperationIdentity) => requiredKinds.includes(operation.kind) ? waiting(operation) : undefined;
  owned.waiters.add(requiredWait);
  const closed = new AbortController();
  let bound = false;
  let released = false;
  const guard = (): void => { closed.signal.throwIfAborted(); owned.ownerClosed.signal.throwIfAborted(); };
  return {
    controller: owned.controller,
    own(): void { guard(); bound = true; },
    bind(prepare: PrepareCodexOperation): PrepareCodexOperation {
      guard();
      bound = true;
      const guarded: PrepareCodexOperation = async (signal, check, kind) => {
        const current = (): void => { guard(); check(); };
        current();
        const permit = await prepare(AbortSignal.any([signal, closed.signal, owned.ownerClosed.signal]), current, kind);
        // Observe negotiation currency even if a simultaneous local/source guard throws.
        permit();
        current();
        return () => { const valid = permit(); current(); return valid; };
      };
      owned.prepare = guarded;
      return guarded;
    },
    async prepare(kind: Kind, signal: AbortSignal, check: () => void): Promise<() => boolean | void> {
      const current = (): void => { guard(); check(); };
      current();
      const bounded = AbortSignal.any([signal, closed.signal, owned.ownerClosed.signal]);
      const permit = owned.prepare ? await owned.prepare(bounded, current, kind)
        : await owned.controller.admit({ operation_id: randomUUID(), kind }, bounded, current, async () => { current(); });
      permit();
      current();
      return () => { const valid = permit(); current(); return valid; };
    },
    close(): void {
      if (released) return;
      released = true;
      closed.abort(new Error("Codex operation scope closed"));
      owned.waiters.delete(requiredWait);
      if (bound) owned.ownerClosed.abort(new Error("Codex operation owner closed"));
      if (--owned.users === 0) { owned.release?.(); runs!.delete(key); }
    },
  };
}
