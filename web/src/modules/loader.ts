// Loading a module's exposed page (PRD #4686, "Loader" and "Failure isolation").
//
// Uses the federation runtime's loadRemote, never React.lazy: a rejected lazy promise is
// cached forever, which would defeat Retry. Each load is raced against a timeout; a late
// arrival is discarded. Failures are classified so the error card can offer the right
// action:
//   transient    network/HTTP error or timeout before the module ever loaded in this
//                document -> Retry (re-register with a fresh entry URL, load again). A
//                Retry that fails transient again also offers Reload page: the module may still be down,
//                or an engine may keep a failed module file for the life of the document.
//   incompatible shared-version refusal, missing expose, contract major mismatch ->
//                no action (Retry cannot fix it)
//   stale        a chunk load failing AFTER the module already loaded in this document
//                (a rollout replaced the files) -> Reload page. A browser keeps a
//                successfully imported remoteEntry.js for the life of the document, so
//                re-registering can never swap v1 for v2 in place.

import type { ComponentType } from "react";
import { HOST_CONTRACT, type HostSdkV1 } from "./sdk-contract";
import type { ModuleEntry } from "./manifest";
import { loadModuleRemote, reRegisterModule } from "./federationEntry";
import { describeThrown } from "./thrown";

export type LoadFailureKind = "transient" | "incompatible" | "stale";

export const MODULE_LOAD_TIMEOUT_MS = 15000;

// The runtime's shared-version refusal carries this text in the error (or its cause).
const SHARE_REFUSAL = "does not satisfy the requirement of";
// A remote that loads but does not expose the requested module. Pinned from observation
// (browser harness `noexpose` fixture, @module-federation/vite 1.25.0 remote against the
// 2.9.3 runtime): the remote container's own get() throws, and the runtime passes it through
// with no RUNTIME-xxx code:
//   "[Module Federation] Module ./Missing does not exist in container."
const MISSING_EXPOSE = /\[Module Federation\] Module \S+ does not exist in container/;
// The three engines word a failed dynamic import differently. Chromium and Firefox name
// the URL; Safari names nothing.
const DYNAMIC_IMPORT_FAILURE =
  /Failed to fetch dynamically imported module|error loading dynamically imported module|Importing a module script failed/i;
// The runtime's own script load failure, pinned from observation (a module whose
// remoteEntry.js request fails, in the browser harness):
//   "[ Federation Runtime ]: Failed to load script resources. #RUNTIME-008 args: {...}
//    Original Error Message: Failed to fetch dynamically imported module: <url>"
const MF_SCRIPT_FAILURE = /RUNTIME-008|Failed to load script resources/;

export class ModuleLoadError extends Error {
  kind: LoadFailureKind;
  cause?: unknown;
  constructor(kind: LoadFailureKind, message: string, options?: { cause?: unknown }) {
    super(message);
    this.name = "ModuleLoadError";
    this.kind = kind;
    this.cause = options?.cause;
  }
}

export type ModulePageComponent = ComponentType<{ sdk: HostSdkV1 }>;

// Modules that completed a load in THIS document, and per-module counters.
const loadedOnce = new Set<string>();
const attemptToken = new Map<string, number>();
const inflight = new Map<string, Promise<ModulePageComponent>>();
// Modules whose Retry failed transient in THIS document (cleared by a successful load).
const retryFailed = new Set<string>();
let retryCounter = 0;

export function hasModuleLoaded(id: string): boolean {
  return loadedOnce.has(id);
}

export function hasRetryFailed(id: string): boolean {
  return retryFailed.has(id);
}

// Test helper.
export function resetModuleLoaderForTests(): void {
  loadedOnce.clear();
  retryFailed.clear();
  attemptToken.clear();
  inflight.clear();
  retryCounter = 0;
}

function timeoutMs(): number {
  const override = (globalThis as { __UZI_MODULE_LOAD_TIMEOUT_MS__?: unknown }).__UZI_MODULE_LOAD_TIMEOUT_MS__;
  return typeof override === "number" && Number.isFinite(override) && override > 0 ? override : MODULE_LOAD_TIMEOUT_MS;
}

function messageOf(err: unknown): string {
  const parts: string[] = [];
  for (let e: unknown = err, depth = 0; e != null && depth < 4; depth++) {
    parts.push(describeThrown(e));
    try {
      e = (e as { cause?: unknown }).cause;
    } catch {
      break; // a throwing `cause` getter or a revoked Proxy ends the chain
    }
  }
  return parts.join(" | ");
}

// A chunk/script load error after the module already loaded in this document. The
// dynamic-import patterns match any URL (or none, for Safari) because the failing chunk is
// the module's own; an unrelated URL is not stale of THIS module, so when a URL is named
// it must sit under /modules/<id>/.
export function isStaleChunkError(err: unknown, id: string): boolean {
  const msg = messageOf(err);
  if (MF_SCRIPT_FAILURE.test(msg)) return true;
  if (!DYNAMIC_IMPORT_FAILURE.test(msg)) return false;
  const url = /https?:\/\/[^\s)'"]+/.exec(msg);
  if (url === null) return true;
  try {
    return new URL(url[0]).pathname.startsWith(`/modules/${id}/`);
  } catch {
    return false;
  }
}

export function classifyLoadError(err: unknown, id: string): ModuleLoadError {
  try {
    if (err instanceof ModuleLoadError) return err; // throws for a revoked Proxy
  } catch {
    // classified below from its safe text
  }
  const msg = messageOf(err);
  if (msg.includes(SHARE_REFUSAL)) {
    return new ModuleLoadError("incompatible", `This module needs a shared library version this uzi does not provide: ${msg}`, { cause: err });
  }
  if (MISSING_EXPOSE.test(msg)) {
    return new ModuleLoadError("incompatible", `The module does not expose the configured page: ${msg}`, { cause: err });
  }
  if (loadedOnce.has(id) && isStaleChunkError(err, id)) {
    return new ModuleLoadError("stale", `A module file changed after it loaded: ${msg}`, { cause: err });
  }
  return new ModuleLoadError("transient", msg, { cause: err });
}

function withTimeout<T>(p: Promise<T>, ms: number, id: string): Promise<T> {
  return new Promise<T>((resolve, reject) => {
    let done = false;
    const timer = setTimeout(() => {
      done = true;
      reject(new ModuleLoadError("transient", `Timed out after ${ms} ms loading module "${id}"`));
    }, ms);
    p.then(
      (v) => {
        if (done) return; // a late arrival is discarded
        done = true;
        clearTimeout(timer);
        resolve(v);
      },
      (e: unknown) => {
        if (done) return;
        done = true;
        clearTimeout(timer);
        reject(e);
      },
    );
  });
}

// budgetMs: the remaining load deadline (a Retry shares one deadline across re-registration
// and load, so the whole Retry stays within the per-load timeout).
async function doLoad(entry: Pick<ModuleEntry, "id" | "page">, budgetMs: number = timeoutMs()): Promise<ModulePageComponent> {
  const { id } = entry;
  const token = (attemptToken.get(id) ?? 0) + 1;
  attemptToken.set(id, token);
  try {
    const page = entry.page.replace(/^\.\//, "");
    const mod = (await withTimeout(loadModuleRemote(`${id}/${page}`), budgetMs, id)) as {
      default?: unknown;
      hostContract?: unknown;
    } | null;
    if (attemptToken.get(id) !== token) {
      throw new ModuleLoadError("transient", `A newer load of module "${id}" superseded this one`);
    }
    if (mod == null || mod.default == null) {
      throw new ModuleLoadError("incompatible", `The module does not expose the configured page "${entry.page}"`);
    }
    if (mod.hostContract !== HOST_CONTRACT) {
      throw new ModuleLoadError(
        "incompatible",
        `The module targets host contract ${describeThrown(mod.hostContract ?? "none")}; this uzi supports contract ${HOST_CONTRACT}`,
      );
    }
    loadedOnce.add(id);
    retryFailed.delete(id);
    return mod.default as ModulePageComponent;
  } catch (e) {
    throw classifyLoadError(e, id);
  }
}

// Loads (or joins the in-flight load of) the module's page component. Rejects with a
// ModuleLoadError.
export function loadModulePage(entry: Pick<ModuleEntry, "id" | "page">): Promise<ModulePageComponent> {
  const running = inflight.get(entry.id);
  if (running) return running;
  const p = doLoad(entry).finally(() => {
    if (inflight.get(entry.id) === p) inflight.delete(entry.id);
  });
  inflight.set(entry.id, p);
  return p;
}

// Retry for a transient first-load failure: re-register the remote with a fresh entry URL,
// then load again. Not an update mechanism. The whole of re-registration plus load is ONE
// shared in-flight promise, registered synchronously, so a remount while a Retry is pending
// joins it instead of starting an independent load. A Retry that fails transient is
// recorded (retryFailed) so the error card offers Reload page next to Retry.
export function retryModuleLoad(entry: Pick<ModuleEntry, "id" | "page">): Promise<ModulePageComponent> {
  const { id } = entry;
  const running = inflight.get(id);
  if (running) return running;
  const n = ++retryCounter;
  const p: Promise<ModulePageComponent> = (async () => {
    try {
      // One deadline for the whole Retry (re-registration plus load), the same budget as a
      // single load: a remount joins this promise, so a hang here must not spin forever.
      // Elapsed time on the monotonic clock: a wall-clock step must not stretch the budget.
      const budget = timeoutMs();
      const start = performance.now();
      await withTimeout(reRegisterModule(id, n), budget, id);
      const remaining = budget - (performance.now() - start);
      if (!(remaining > 0)) {
        throw new ModuleLoadError("transient", `Timed out after ${budget} ms loading module "${id}"`);
      }
      return await doLoad(entry, remaining);
    } catch (e) {
      const err = classifyLoadError(e, id); // never throws: guards a revoked Proxy
      if (err.kind === "transient" && !loadedOnce.has(id)) retryFailed.add(id);
      throw err;
    }
  })().finally(() => {
    if (inflight.get(id) === p) inflight.delete(id);
  });
  inflight.set(id, p);
  return p;
}
