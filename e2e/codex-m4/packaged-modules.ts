// PRD #1287 C1 — resolve the Codex adapter modules the real-binary P smoke composes,
// at runtime, while keeping host typechecking tied to the source tree. Type-only imports
// erase before execution; the runtime loaders dynamic-import the modules from the src tree
// (host: `agent/src` via `dir: agent`; an image/CI run may point CODEX_M4_SRC at /app/src).
//
// Adapted from e2e/codex-m3b/packaged-modules.ts. The M4 P smoke drives the REAL launch
// primitive + transport + app-server auth + a REAL callback broker over the pinned Codex
// binary, so it loads exactly those production modules (launcher, broker, config,
// transport, appserver-auth, dynamic-tools, registry). C3/C4 extend the smoke; a new
// module they need is one more loader here, never a tsconfig edit.

import path from "node:path";
import { pathToFileURL } from "node:url";

/** The isolated per-root Codex launch primitive + its pinned binary/argv constants. */
export type LauncherModule = typeof import("../../agent/src/codex/launcher.js");
/** The fail-closed worker callback broker (the sole run-tool authority). */
export type BrokerModule = typeof import("../../agent/src/codex/broker.js");
/** The native-disabled config builders (incl. the loopback TEST builder). */
export type ConfigModule = typeof import("../../agent/src/codex/config.js");
/** The app-server JSON-RPC-over-stdio transport factory. */
export type TransportModule = typeof import("../../agent/src/codex/transport.js");
/** The pinned app-server authentication owner (initialize/login handshake). */
export type AppServerAuthModule = typeof import("../../agent/src/codex/appserver-auth.js");
/** The model-visible dynamic-tool registration builder. */
export type DynamicToolsModule = typeof import("../../agent/src/codex/dynamic-tools.js");
/** The pure execution registry (idempotency/poison + root/epoch bookkeeping). */
export type RegistryModule = typeof import("../../agent/src/codex/registry.js");

/** The src dir the modules load from: an explicit `CODEX_M4_SRC` (an image/CI run points it
 *  at `/app/src`), else `<cwd>/src` — which resolves to `agent/src` when the M4 target runs
 *  via `dir: agent`, matching test:codex-m3b's host resolution. No `import.meta`: the e2e
 *  tree has no `package.json`, so its `.ts` files are CommonJS-typed. */
function srcDir(): string {
  const override = process.env.CODEX_M4_SRC;
  if (override && override.trim().length > 0) return override;
  return path.resolve(process.cwd(), "src");
}

async function load<T>(relative: string): Promise<T> {
  return (await import(pathToFileURL(`${srcDir()}/${relative}`).href)) as T;
}

export function loadPackagedLauncher(): Promise<LauncherModule> {
  return load<LauncherModule>("codex/launcher.ts");
}

export function loadPackagedBroker(): Promise<BrokerModule> {
  return load<BrokerModule>("codex/broker.ts");
}

export function loadPackagedConfig(): Promise<ConfigModule> {
  return load<ConfigModule>("codex/config.ts");
}

export function loadPackagedTransport(): Promise<TransportModule> {
  return load<TransportModule>("codex/transport.ts");
}

export function loadPackagedAppServerAuth(): Promise<AppServerAuthModule> {
  return load<AppServerAuthModule>("codex/appserver-auth.ts");
}

export function loadPackagedDynamicTools(): Promise<DynamicToolsModule> {
  return load<DynamicToolsModule>("codex/dynamic-tools.ts");
}

export function loadPackagedRegistry(): Promise<RegistryModule> {
  return load<RegistryModule>("codex/registry.ts");
}
