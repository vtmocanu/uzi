// The Module Federation runtime wiring (PRD #4686, "Loader"). The host uses the RUNTIME
// only (no federation build plugin), and this file is loaded lazily by registry.tsx once
// /modules.json lists at least one module, so with zero modules the federation chunk is
// never fetched. It is also the only place that calls the runtime's registerRemotes and
// loadRemote.
//
// Shared singletons: a module built with the federation plugin shares the host's React,
// React DOM, router and jsx-runtime instead of bundling its own (two Reacts break hooks
// and the router context). `strictVersion` makes a module built against an incompatible
// range fail loudly with a version-refusal error instead of loading a second copy.

import { init, loadRemote, registerRemotes } from "@module-federation/runtime";
import type { ModuleEntry } from "./manifest";
import { countModuleStat } from "./stats";

// The host's shared libraries, handed in by the entry chunk (hostShared.ts) through
// the wrappers in federationEntry.ts (registerModuleRemotes, reRegisterModule, loadModuleRemote). This file must NOT import react, react-dom,
// react-router-dom or the package.json files itself: a static import of any module that
// lives in the entry chunk gives this lazy chunk an edge back to the entry, and rolldown
// then moves its CommonJS interop helpers into a separate runtime chunk that the entry
// preloads on every page load, an extra request even with zero modules configured.
export interface HostSharedLib {
  version: string;
  lib: unknown;
  requiredVersion: string;
}
export type HostShared = Record<"react" | "react-dom" | "react-router-dom" | "react/jsx-runtime", HostSharedLib>;

let hostShared: HostShared | null = null;

export function setHostShared(shared: HostShared): void {
  hostShared = shared;
}

function shareOf({ version, lib, requiredVersion }: HostSharedLib) {
  return { version, lib: () => lib, shareConfig: { singleton: true, requiredVersion, strictVersion: true } };
}

let initialised = false;

function ensureInit(): void {
  if (initialised) return;
  if (hostShared === null) throw new Error("uzi modules: the host shared libraries were not provided to the federation chunk");
  const shared = hostShared;
  initialised = true;
  init({
    name: "uzi_host",
    remotes: [],
    shareStrategy: "loaded-first",
    shared: {
      react: shareOf(shared.react),
      "react-dom": shareOf(shared["react-dom"]),
      "react-router-dom": shareOf(shared["react-router-dom"]),
      "react/jsx-runtime": shareOf(shared["react/jsx-runtime"]),
    },
  });
}

function entryUrl(id: string, retry?: number): string {
  return `/modules/${id}/remoteEntry.js${retry === undefined ? "" : `?retry=${retry}`}`;
}

// Registers every configured module. Fetches no remote entry: that happens on the first
// loadRemote of a module route.
export function registerModuleRemotes(list: ModuleEntry[]): void {
  ensureInit();
  registerRemotes(
    list.map((m) => ({ name: m.id, entry: entryUrl(m.id), type: "module" as const })),
    { force: true },
  );
  countModuleStat("registerRemotes");
}

// Retry for a transient first-load failure: re-register the remote with a fresh entry URL
// and `force`. The fresh URL only clears a failed fetch of the entry itself. A child chunk
// keeps its URL: Chromium re-fetches a failed chunk on Retry, but an engine that keeps the
// failure for the document cannot recover by Retry, hence Reload page after a failed Retry.
export function reRegisterModule(id: string, retry: number): void {
  ensureInit();
  registerRemotes([{ name: id, entry: entryUrl(id, retry), type: "module" as const }], { force: true });
  countModuleStat("registerRemotes");
}

export function loadModuleRemote(request: string): Promise<unknown> {
  ensureInit();
  countModuleStat("loadRemote");
  return loadRemote<unknown>(request);
}
