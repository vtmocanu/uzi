// Shared, scoped cache for the run-start preview (issue #3261). A provider holds one
// GET /me/effective-settings and one start-preview POST per distinct choice for each
// SCOPE, a scope being one (user, refresh generation). A surface bumps `generation` when it
// refreshes its start prerequisites; `null` means it has not applied its first prerequisites,
// so nothing is fetched. No timers.
//
// Ordering rule: a response is published only when its scope is still the provider's current
// scope AND the consumer still wants its key. A scope change (generation or user) takes
// effect in the same render, so a response still pending from the old scope can never
// publish, whichever order responses arrive in. Scope epochs only grow, so after A -> B -> A
// the first A scope stays dead. A user change or logout drops every published value.

import { createContext, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { api } from "./api";
import type { EffectiveSettings, StartPreview, StartPreviewRequest } from "./apiTypes";
import { useOptionalAuth } from "../auth/AuthContext";
import { startPreviewKey, startPreviewView, type StartPreviewViewModel } from "./startPreview";

let nextEpoch = 0;

interface Scope {
  epoch: number;
  userId: string | null;
  generation: number | null;
  get: Promise<EffectiveSettings> | null;
  posts: Map<string, Promise<StartPreview>>;
}

interface StoreValue {
  scope: Scope;
  // The epoch of the provider's newest scope, written as the scope is created.
  latest: { epoch: number };
}

const StoreContext = createContext<StoreValue | null>(null);

export function StartPreviewProvider({ generation, children }: { generation: number | null; children: ReactNode }) {
  const userId = useOptionalAuth()?.user?.id ?? null;
  const [latest] = useState(() => ({ epoch: 0 }));
  const value = useMemo<StoreValue>(() => {
    const scope: Scope = { epoch: ++nextEpoch, userId, generation, get: null, posts: new Map() };
    latest.epoch = scope.epoch;
    return { scope, latest };
  }, [userId, generation, latest]);
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

  const latest = store?.latest ?? null;
  useEffect(() => {
    const req = requestRef.current;
    if (!scope || !latest || !req || key === null || scope.userId === null || scope.generation === null) return;
    const uid = scope.userId;
    let live = true;
    void Promise.allSettled([scopeGet(scope), scopePost(scope, key, req)]).then(([g, p]) => {
      if (!live || latest.epoch !== scope.epoch) return;
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
  }, [scope, latest, key]);

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
