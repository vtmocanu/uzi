// The only callers of the lazily loaded federation chunk (PRD #4686, "Loader"): each wrapper
// imports the chunk, hands it the host's shared libraries (see federation.ts for why it must
// not import them itself) and forwards the call. registry.tsx registers through here after
// the list resolves; loader.ts loads and retries through here. Importing this file does not
// fetch the federation chunk.
//
// The import is written out in each wrapper (rather than in a shared helper that returns the
// module) so the dead-code check can trace each federation.ts export to its use.

import type { ModuleEntry } from "./manifest";
import { HOST_SHARED } from "./hostShared";

export async function registerModuleRemotes(list: ModuleEntry[]): Promise<void> {
  const federation = await import("./federation");
  federation.setHostShared(HOST_SHARED);
  federation.registerModuleRemotes(list);
}

export async function reRegisterModule(id: string, retry: number): Promise<void> {
  const federation = await import("./federation");
  federation.setHostShared(HOST_SHARED);
  federation.reRegisterModule(id, retry);
}

export async function loadModuleRemote(request: string): Promise<unknown> {
  const federation = await import("./federation");
  federation.setHostShared(HOST_SHARED);
  return federation.loadModuleRemote(request);
}
