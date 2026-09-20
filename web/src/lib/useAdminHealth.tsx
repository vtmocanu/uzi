import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";

import { useAuth } from "../auth/AuthContext";
import { api, type HealthDoc } from "./api";
import { usePollWhileVisible } from "./usePollWhileVisible";

// HealthStatus is what every admin-health surface reads (PRD #1484 M5): the last-good
// document (null for a non-admin, or before the first load) and the derived attention tally
// the pips render.
export interface HealthStatus {
  doc: HealthDoc | null;
  attentionCount: number;
}

// healthAttentionCount is the "needs attention" tally the tab/sidebar/overview pips render:
// every non-ok, non-na check (warn + danger + unknown). na is a check that cannot apply on
// this deployment and ok is passing — neither is attention. Local: exported nowhere because
// the provider is now the one place that derives it (knip flags an unused value export).
function healthAttentionCount(doc: HealthDoc): number {
  return doc.counts.warn + doc.counts.danger + doc.counts.unknown;
}

// useHealthPoll is the single best-effort poll of GET /api/admin/health. It polls every 10 s
// while the tab is visible (usePollWhileVisible) and KEEPS THE LAST-GOOD document on a failed
// fetch, mirroring CustodyBoardAlert — a transient blip must never blank a pip or the Health
// page. It fetches ONLY while `enabled`: the provider passes `isAdmin`, so a non-admin NEVER
// triggers a request to the admin endpoint (exactly UpdateEscalationBanner's `if (!isAdmin)`
// gate, which "makes no fetch for non-admins"). When disabled it holds no document, so a
// consumer of a non-admin session reads `{ doc: null, attentionCount: 0 }`.
function useHealthPoll(enabled: boolean): HealthStatus {
  const [doc, setDoc] = useState<HealthDoc | null>(null);

  const load = useCallback(async () => {
    // The isAdmin gate lives HERE and nowhere else, so there is exactly one place a request
    // can be issued and exactly one condition that gates it.
    if (!enabled) return;
    try {
      setDoc(await api.getAdminHealth());
    } catch {
      // Best-effort: a fetch failure keeps the last-good document (or stays null on the
      // first load). Health surfacing must never break the page it sits on.
    }
  }, [enabled]);

  useEffect(() => {
    void load();
  }, [load]);
  usePollWhileVisible(load, 10000);

  const activeDoc = enabled ? doc : null;
  return { doc: activeDoc, attentionCount: activeDoc ? healthAttentionCount(activeDoc) : 0 };
}

const HealthStatusContext = createContext<HealthStatus | undefined>(undefined);

// HealthStatusProvider is the ONE gated poller (PRD #1484 M5). Mounted once, high in
// AppShell, so it wraps every consumer — the app-wide danger banner, the sidebar Admin pip,
// the Overview admin card, the Admin > Health tab pip and the Health page — and there is
// EXACTLY ONE poll for the whole app. It reads isAdmin from the auth context the same way
// UpdateEscalationBanner does; a non-admin never fetches (useHealthPoll's `enabled` gate).
export function HealthStatusProvider({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  const value = useHealthPoll(user?.is_admin === true);
  return <HealthStatusContext.Provider value={value}>{children}</HealthStatusContext.Provider>;
}

// useHealthStatus is the consumer hook every admin-health surface uses. In the app the
// provider is always mounted in AppShell, so `ctx` is defined and the fallback poll below
// stays OFF — one poll, one isAdmin gate. The fallback exists ONLY for a surface rendered
// standalone (a component test that mounts a page without AppShell): with no provider it
// self-fetches, exactly as the old shared hook did, so those tests keep working. It does not
// read the auth context, so it never depends on an AuthProvider being present in such a test.
export function useHealthStatus(): HealthStatus {
  const ctx = useContext(HealthStatusContext);
  const fallback = useHealthPoll(ctx === undefined);
  return ctx ?? fallback;
}
