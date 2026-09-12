// PRD #1287 C4 — the shared U-layer driver for the Codex lifecycle (delegation & cleanup)
// conformance cases. These exercise the REAL production ExecutionRegistry + CodexExecutionSafety
// facade directly (D5: reuse the M3 primitives, NOT a replacement policy broker). Every process
// is an injected fake root/seam — nothing is spawned or waited on for real, so a denial/timeout
// is proven by a sink-called counter, a spawn-seam counter, a registry terminal or the sticky
// poison witness (D4), never by denial text alone.
//
// It loads the production modules through the SAME src-root resolution as packaged-modules.ts /
// harness-u.ts (host `agent/src` via `dir: agent`, or CODEX_M4_SRC → /app/src on an image/CI run).

import path from "node:path";
import { pathToFileURL } from "node:url";

import type * as SafetyT from "../../agent/src/codex/safety.js";
import type * as RegistryT from "../../agent/src/codex/registry.js";
import type { ReapOutcome, RegisteredRoot, RootKind } from "../../agent/src/codex/registry.js";
import type { SpawnRootSeam, BoundaryActionIdentity } from "../../agent/src/codex/safety.js";
import type { BoundaryRequest } from "../../agent/src/harness.js";

function srcDir(): string {
  const override = process.env.CODEX_M4_SRC;
  if (override && override.trim().length > 0) return override;
  return path.resolve(process.cwd(), "src");
}
async function load<T>(relative: string): Promise<T> {
  return (await import(pathToFileURL(`${srcDir()}/${relative}`).href)) as T;
}

/** The two production modules the lifecycle cases drive directly, resolved once in a `before()`. */
export interface LifecycleModules {
  safety: typeof SafetyT;
  registry: typeof RegistryT;
}

export async function loadLifecycleModules(): Promise<LifecycleModules> {
  const safety = await load<typeof SafetyT>("codex/safety.ts");
  const registry = await load<typeof RegistryT>("codex/registry.ts");
  return { safety, registry };
}

type Registry = InstanceType<typeof RegistryT.ExecutionRegistry>;

/** A minimal deferred promise (the safety unit tests' `defer`). */
export function defer<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

/** One event-loop tick so a fire-and-forget action can advance before we assert. */
export const tick = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 5));

/** A boundary request with a generous default budget. */
export function boundaryReq(boundary: BoundaryRequest["boundary"], deadlineMs = 1000): BoundaryRequest {
  return { boundary, deadlineMs };
}

/** An injected fake supervisor root with reap/dispose counters. Its `reap` defaults to an
 *  observed-empty ECHILD; a caller can inject a hanging/failing reap. */
export class FakeRoot implements RegisteredRoot {
  reapCalls = 0;
  disposeCalls = 0;
  constructor(
    readonly kind: RootKind,
    private readonly reapImpl: (d: number) => Promise<ReapOutcome> = async () => ({ ok: true }),
    private readonly disposeImpl: (d: number) => Promise<void> = async () => {},
  ) {}
  async reap(d: number): Promise<ReapOutcome> {
    this.reapCalls += 1;
    return this.reapImpl(d);
  }
  async dispose(d: number): Promise<void> {
    this.disposeCalls += 1;
    return this.disposeImpl(d);
  }
}

/** Reserve + register a root in one step (mirrors the safety/registry unit helpers). */
export function registerRoot(reg: Registry, root: RegisteredRoot): void {
  const reserved = reg.reserveLaunch(root.kind);
  if (reserved.kind !== "reserved") throw new Error(`reserveLaunch(${root.kind}) denied`);
  const registered = reg.registerRoot(reserved.reservation, root);
  if (!registered.ok) throw new Error("registerRoot rejected");
}

/** A low-level boundary-action spawn seam with a call counter; its produced boundary-action
 *  root reaps per `reapImpl` (defaults to observed-empty). The counter is the negative-effect
 *  oracle for the [R3-2] ordering cases — a refused worker_pat action never reaches it. */
export function spawnCounter(
  reapImpl: (d: number) => Promise<ReapOutcome> = async () => ({ ok: true }),
): { state: { calls: number; lastIdentity: BoundaryActionIdentity | undefined }; seam: SpawnRootSeam } {
  const state = { calls: 0, lastIdentity: undefined as BoundaryActionIdentity | undefined };
  const seam: SpawnRootSeam = async (_argv, identity) => {
    state.calls += 1;
    state.lastIdentity = identity;
    return { kind: "boundary_action", reap: reapImpl, dispose: async () => {} };
  };
  return { state, seam };
}
