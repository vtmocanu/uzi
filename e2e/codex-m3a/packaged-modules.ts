// Resolve production modules from the worker image at runtime while keeping host
// typechecking tied to the source tree. Type-only imports erase before execution;
// real container calls load /app/src and cannot pass on a host-mounted source copy.

import { pathToFileURL } from "node:url";

export type LauncherModule = typeof import("../../agent/src/codex/launcher.js");
export type ConfigModule = typeof import("../../agent/src/codex/config.js");

export async function loadPackagedLauncher(): Promise<LauncherModule> {
  const path = process.env.M3A_PACKAGED_LAUNCHER ?? "/app/src/codex/launcher.ts";
  return await import(pathToFileURL(path).href) as LauncherModule;
}

export async function loadPackagedConfig(): Promise<ConfigModule> {
  const path = process.env.M3A_PACKAGED_CONFIG ?? "/app/src/codex/config.ts";
  return await import(pathToFileURL(path).href) as ConfigModule;
}
