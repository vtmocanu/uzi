// PRD #1171 m5 — resolve the PACKAGED Codex adapter modules from the worker image at
// runtime while keeping host typechecking tied to the source tree. Type-only imports erase
// before execution; the real container calls load the IMAGE-BAKED /app/src and cannot pass
// on a host-mounted source copy.
//
// The one image runs BOTH legs of lifecycle.test.ts: the host node --test run leaves
// CODEX_M3B_SRC unset and loads the SOURCE tree next to this file (so the suite runs with
// zero setup via `cd agent && node --import tsx --test ../e2e/codex-m3b/lifecycle.test.ts`);
// run-lifecycle.sh sets CODEX_M3B_SRC=/app/src so the in-image run loads the baked adapter.

import path from "node:path";
import { pathToFileURL } from "node:url";

/** The packaged production CodexExecutor + the DARK selection seam's Codex branch. */
export type CodexExecutorModule = typeof import("../../agent/src/codex/codex-executor.js");
/** The claim → binding selector (the first half of main.ts's makeExecutor decision). */
export type SelectModule = typeof import("../../agent/src/codex/select.js");
/** The credential-free session store used across real provider-root recreation. */
export type SessionStateModule = typeof import("../../agent/src/codex/session-state.js");

/** The image-baked (or host source-tree) `src` dir the packaged modules load from.
 *
 *  Resolution order: an explicit `CODEX_M3B_SRC` (run-lifecycle.sh sets it to `/app/src`),
 *  else `<cwd>/src`. Both the host feasibility command (`cd agent && node --import tsx --test
 *  ../e2e/codex-m3b/lifecycle.test.ts` → cwd `agent` → `agent/src`) and the in-image run
 *  (cwd `/app` → `/app/src`) resolve correctly with no `import.meta` — the e2e tree has no
 *  `package.json`, so its `.ts` files are CommonJS-typed and `import.meta` is disallowed. */
function srcDir(): string {
  const override = process.env.CODEX_M3B_SRC;
  if (override && override.trim().length > 0) return override;
  return path.resolve(process.cwd(), "src");
}

async function load<T>(relative: string): Promise<T> {
  return (await import(pathToFileURL(`${srcDir()}/${relative}`).href)) as T;
}

export function loadPackagedCodexExecutor(): Promise<CodexExecutorModule> {
  return load<CodexExecutorModule>("codex/codex-executor.ts");
}

export function loadPackagedSelect(): Promise<SelectModule> {
  return load<SelectModule>("codex/select.ts");
}

export function loadPackagedSessionState(): Promise<SessionStateModule> {
  return load<SessionStateModule>("codex/session-state.ts");
}

// NOTE (main.ts): the DARK selection FACTORY lives in main.ts as a private const
// `makeExecutor` — it is NOT exported, and importing main.ts is impossible for this harness
// because main.ts self-invokes the worker on import (`main().catch(...)` at module scope),
// which would boot a runner. So the seam is loaded through its exported CONSTITUENTS instead:
// `selectCodexBinding` (SelectModule) decides claude-vs-codex and validates the binding, and
// `CodexExecutor` / `FailClosedExecutor` (CodexExecutorModule) are the two executors
// makeExecutor constructs. Composing those here reproduces makeExecutor's decision exactly —
// the same composition the packaged codex-executor unit suite's "dark selection seam (the
// makeExecutor decision)" block already pins.
