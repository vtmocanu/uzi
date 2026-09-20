import { useCallback, useEffect, useState } from "react";

import { api, type HealthDoc } from "./api";
import { usePollWhileVisible } from "./usePollWhileVisible";

// useAdminHealth is the one shared reader of GET /api/admin/health (PRD #1484 M4). It
// polls best-effort every 10 s while the tab is visible (usePollWhileVisible) and KEEPS
// THE LAST-GOOD document on a failed fetch, mirroring CustodyBoardAlert's try/catch — a
// transient blip must never blank the tab pip or the Health page.
//
// It is deliberately the single source for BOTH consumers today: AdminShell (the Health
// tab's severity pip, which must render on every admin tab, not only while viewing Health)
// and the Health page itself (its content). The endpoint caches one evaluation for 5 s, so
// two pollers hitting it is cheap; a shared hook keeps the polling/last-good logic in one
// place rather than duplicated. M5 reuses it for the sidebar Admin pip.
//
// The endpoint is admin-only (RequireAdminRO), and every current caller is an admin-guarded
// surface, so there is no non-admin fetch to gate here. M5's sidebar pip renders app-wide
// and WILL need to gate the fetch on admin; that is M5's concern, not this hook's.
export function useAdminHealth(): HealthDoc | null {
  const [doc, setDoc] = useState<HealthDoc | null>(null);

  const load = useCallback(async () => {
    try {
      setDoc(await api.getAdminHealth());
    } catch {
      // Best-effort: a fetch failure keeps the last-good document (or stays null on the
      // first load). Health surfacing must never break the page it sits on.
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);
  usePollWhileVisible(load, 10000);

  return doc;
}

// healthAttentionCount is the "needs attention" tally the tab/sidebar pip renders: every
// non-ok, non-na check (so warn + danger + unknown). na is a check that cannot apply on
// this deployment and ok is passing — neither is attention. Exported so AdminShell and the
// Health page derive the same number.
export function healthAttentionCount(doc: HealthDoc): number {
  return doc.counts.warn + doc.counts.danger + doc.counts.unknown;
}
