// Shared, scoped cache for the run-start preview (issue #3261). A provider holds one
// GET /me/effective-settings and one start-preview POST per distinct choice for each
// SCOPE, a scope being one (user, refresh generation). A surface bumps `generation` when it
// refreshes its start prerequisites; `null` means it has not applied its first prerequisites,
// so nothing is fetched. No timers.
//
// Ordering rule: a response is published only while (a) the effect that requested it is still
// live (its passive cleanup has not run) AND (b) its scope is still the scope the latest commit
// carries (a ref set in a layout effect). (b) is needed because React commits a new scope before
// it runs passive cleanups, and outside act those cleanups can run as a later task, so a
// response settling in between would pass (a) alone. A key change is covered by (a) and the
// visibility key check. Every scope object is created purely (no render-phase mutation), so
// React StrictMode's double render and double effect run are harmless; the per-scope promise
// maps keep that to one request each. After A -> B -> A the first A scope is a different
// object and stays dead. A user change or logout drops every published value.

import { createContext, useContext, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { api } from "./api";
import type { EffectiveSettings, StartPreview, StartPreviewRequest } from "./apiTypes";
import { useOptionalAuth } from "../auth/AuthContext";
import { startPreviewKey, startPreviewView, type StartPreviewViewModel } from "./startPreview";

interface Scope {
  userId: string | null;
  generation: number | null;
  get: Promise<EffectiveSettings> | null;
  posts: Map<string, Promise<StartPreview>>;
}

interface StoreValue {
  scope: Scope;
}

const StoreContext = createContext<StoreValue | null>(null);

export function StartPreviewProvider({ generation, children }: { generation: number | null; children: ReactNode }) {
  const userId = useOptionalAuth()?.user?.id ?? null;
  const value = useMemo<StoreValue>(
    () => ({ scope: { userId, generation, get: null, posts: new Map() } }),
    [userId, generation],
  );
  return <StoreContext.Provider value={value}>{children}</StoreContext.Provider>;
}

function scopeGet(scope: Scope): Promise<EffectiveSettings> {
  // Inside a promise chain so a synchronous throw (a missing method) becomes a rejection.
  scope.get ??= Promise.resolve().then(() => api.getEffectiveSettings());
  return scope.get;
}

function scopePost(scope: Scope, key: string, request: StartPreviewRequest): Promise<StartPreview> {
  let p = scope.posts.get(key);
  if (!p) {
    p = Promise.resolve().then(() => api.previewStartSettings(request));
    scope.posts.set(key, p);
  }
  return p;
}

interface Published {
  userId: string;
  key: string;
  error: boolean;
  preview: StartPreview | null;
  effective: EffectiveSettings | null;
}

export interface StartPreviewResult {
  status: "idle" | "loading" | "ready" | "error";
  view: StartPreviewViewModel | null;
  preview: StartPreview | null;
  effective: EffectiveSettings | null;
}

// useStartPreview returns null outside a provider (the surface renders nothing). A null
// request means "do not fetch" (status idle).
export function useStartPreview(request: StartPreviewRequest | null): StartPreviewResult | null {
  const store = useContext(StoreContext);
  const key = request ? startPreviewKey(request) : null;
  const requestRef = useRef(request);
  const [published, setPublished] = useState<Published | null>(null);
  const scope = store?.scope ?? null;
  const userId = scope?.userId ?? null;

  useEffect(() => {
    requestRef.current = request;
  });

  // A different user (or logout) never sees the previous user's value.
  useEffect(() => {
    setPublished((p) => (p && p.userId !== userId ? null : p));
  }, [userId]);

  // The scope the latest COMMIT carries. Layout effects run synchronously during commit, before
  // any passive cleanup, so this moves the moment a new scope commits; the passive cleanup below
  // can run a task later, and a response settling in that gap must not publish.
  const currentScope = useRef<Scope | null>(scope);
  useLayoutEffect(() => {
    currentScope.current = scope;
  }, [scope]);

  useEffect(() => {
    const req = requestRef.current;
    if (!scope || !req || key === null || scope.userId === null || scope.generation === null) return;
    const uid = scope.userId;
    let live = true;
    void Promise.allSettled([scopeGet(scope), scopePost(scope, key, req)]).then(([g, p]) => {
      if (!live || currentScope.current !== scope) return;
      setPublished({
        userId: uid,
        key,
        error: p.status === "rejected",
        preview: p.status === "fulfilled" ? p.value : null,
        effective: g.status === "fulfilled" ? g.value : null,
      });
    });
    return () => {
      live = false;
    };
  }, [scope, key]);

  const visible = published && published.userId === userId && published.key === key ? published : null;
  const view = useMemo(
    () => (visible?.preview ? startPreviewView(visible.preview, visible.effective) : null),
    [visible],
  );
  if (!store) return null;
  if (key === null) return { status: "idle", view: null, preview: null, effective: null };
  if (!visible) return { status: "loading", view: null, preview: null, effective: null };
  if (visible.error) return { status: "error", view: null, preview: null, effective: visible.effective };
  return { status: "ready", view, preview: visible.preview, effective: visible.effective };
}
