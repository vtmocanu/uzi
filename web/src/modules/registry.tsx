// The module list provider (PRD #4686, "Loader"). /modules.json is fetched ONCE per
// document through a module-level memoized promise, so StrictMode's double effect,
// remounts and any number of consumers share one request. A failed request, a non-2xx
// status or a malformed body all mean "no modules" plus one console error.
//
// When the list is non-empty the federation chunk is imported lazily and every module is
// registered (no remote entry is fetched until a module route renders).

import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { registerModuleRemotes } from "./federationEntry";
import { parseModuleList, type ModuleEntry } from "./manifest";
import { describeThrown } from "./thrown";

export interface ModulesState {
  status: "loading" | "ready";
  modules: ModuleEntry[];
}

// Without a provider (most tests, the route smoke test) the list is empty and ready.
const EMPTY_READY: ModulesState = { status: "ready", modules: [] };
const ModulesContext = createContext<ModulesState>(EMPTY_READY);

let memo: Promise<ModuleEntry[]> | null = null;
// The settled list, so a remounted provider starts "ready" instead of flashing "loading".
let settled: ModuleEntry[] | null = null;

async function fetchModuleList(): Promise<ModuleEntry[]> {
  let body: unknown;
  try {
    const res = await fetch("/modules.json", { cache: "no-cache" });
    if (!res.ok) {
      console.error(`uzi modules: GET /modules.json failed with HTTP ${res.status}; no modules loaded`);
      return [];
    }
    try {
      body = await res.json();
    } catch {
      console.error("uzi modules: /modules.json is not valid JSON; no modules loaded");
      return [];
    }
  } catch (e) {
    console.error(`uzi modules: GET /modules.json failed (${describeThrown(e)}); no modules loaded`);
    return [];
  }
  const list = parseModuleList(body);
  if (list.length > 0) {
    try {
      await registerModuleRemotes(list);
    } catch (e) {
      // The list stays visible; opening a module then shows its load error card.
      console.error(`uzi modules: registering modules failed (${describeThrown(e)})`);
    }
  }
  return list;
}

export function loadModuleList(): Promise<ModuleEntry[]> {
  memo ??= fetchModuleList().then((list) => {
    settled = list;
    return list;
  });
  return memo;
}

// Test helper: forget the memoized list so the next provider fetches again.
export function resetModuleListForTests(): void {
  memo = null;
  settled = null;
}

export function ModulesProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<ModulesState>(() =>
    settled ? { status: "ready", modules: settled } : { status: "loading", modules: [] },
  );
  useEffect(() => {
    let alive = true;
    void loadModuleList().then((modules) => {
      if (alive) setState((s) => (s.status === "ready" && s.modules === modules ? s : { status: "ready", modules }));
    });
    return () => {
      alive = false;
    };
  }, []);
  const value = useMemo(() => state, [state]);
  return <ModulesContext.Provider value={value}>{children}</ModulesContext.Provider>;
}

export function useModules(): ModulesState {
  return useContext(ModulesContext);
}
