import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { useAuth } from "../auth/AuthContext";
import { api, type MaintenanceState } from "./api";
import { usePollWhileVisible } from "./usePollWhileVisible";

// MaintenanceContextValue is what every maintenance surface reads (PRD #3454): the last-good
// GET /api/maintenance state (null before the first load, for a signed-out session, or when
// every fetch so far failed) and a reload the admin card calls after a toggle, so the banner
// and queue-reason labels follow the change without waiting for the next tick.
interface MaintenanceContextValue {
  status: MaintenanceState | null;
  reload: () => void;
}

const NO_PROVIDER: MaintenanceContextValue = { status: null, reload: () => {} };

const MaintenanceContext = createContext<MaintenanceContextValue>(NO_PROVIDER);

// MaintenanceProvider is the ONE poll of GET /api/maintenance for the whole app, mounted in
// AppShell beside HealthStatusProvider. Every signed-in user polls (the banner is for
// everyone), every 10 s while the tab is visible. Failure is quiet: a failed fetch keeps the
// last-good state (or stays null), so a transient blip never flashes the banner on or off.
export function MaintenanceProvider({ children }: { children: ReactNode }) {
  const { user } = useAuth();
  const enabled = user != null;
  const [status, setStatus] = useState<MaintenanceState | null>(null);

  const load = useCallback(async () => {
    if (!enabled) return;
    try {
      setStatus(await api.getMaintenance());
    } catch {
      // Best-effort: keep the last-good state. Maintenance visibility must never break a page.
    }
  }, [enabled]);

  useEffect(() => {
    void load();
  }, [load]);
  usePollWhileVisible(load, 10000);

  const value = useMemo<MaintenanceContextValue>(
    () => ({ status: enabled ? status : null, reload: () => void load() }),
    [enabled, status, load],
  );
  return <MaintenanceContext.Provider value={value}>{children}</MaintenanceContext.Provider>;
}

// useMaintenance reads the shared state. Outside a provider (a component test mounting one
// surface standalone) it reads `{ status: null }` and makes no request, so every consumer
// renders exactly as it did before maintenance mode existed.
export function useMaintenance(): MaintenanceContextValue {
  return useContext(MaintenanceContext);
}
