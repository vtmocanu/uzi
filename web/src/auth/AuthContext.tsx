import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import {
  api,
  ApiError,
  setUnauthorizedHandler,
  setVaultLockedHandler,
  DEFAULT_PRD_LABEL,
  DEFAULT_AUTOPILOT_LABEL,
  DEFAULT_PRDLESS_LABEL,
  type SessionResponse,
  type User,
} from "../lib/api";
import { applyTheme, resolveTheme, DEFAULT_THEME, type Theme } from "../lib/theme";

interface AuthState {
  user: User | null;
  loading: boolean;
  // Instance forge labels delivered on the session bootstrap (PRD #19 M2). They
  // hold the compiled-in defaults until the first session response resolves, so
  // consumers (Board, issue creation) can read them unconditionally.
  prdLabel: string;
  autopilotLabel: string;
  // Theme state from the session bootstrap (PRD #21). theme is the resolved
  // theme currently applied to <html>; themeOverride is the user's raw pick
  // (null = "use default"); defaultTheme is the instance default the Appearance
  // picker labels its "Use default (<name>)" option with. Applying the attribute
  // itself happens in applySession — no component reads `theme` to branch.
  theme: Theme;
  themeOverride: string | null;
  defaultTheme: Theme;
  // PRDLESS escape-hatch config (PRD #22). prdlessEnabled gates the label toggle's
  // visibility; it defaults to false so a server that predates the bootstrap
  // fields simply hides the toggle rather than showing one the backend 422s.
  prdlessLabel: string;
  prdlessEnabled: boolean;
  // Vault status (PRD #32): true when the user's secret vault is unlocked in the
  // server process. Rides the session payload; drives the header badge, the locked
  // banner, and the "waiting for vault unlock" run state. Defaults to true (a
  // server that predates the field, or a signed-out visitor, shows no banner).
  vaultUnlocked: boolean;
  register: (email: string, password: string, displayName: string) => Promise<void>;
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
}

const AuthContext = createContext<AuthState | undefined>(undefined);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);
  const [prdLabel, setPrdLabel] = useState(DEFAULT_PRD_LABEL);
  const [autopilotLabel, setAutopilotLabel] = useState(DEFAULT_AUTOPILOT_LABEL);
  const [theme, setTheme] = useState<Theme>(DEFAULT_THEME);
  const [themeOverride, setThemeOverride] = useState<string | null>(null);
  const [defaultTheme, setDefaultTheme] = useState<Theme>(DEFAULT_THEME);
  const [prdlessLabel, setPrdlessLabel] = useState(DEFAULT_PRDLESS_LABEL);
  const [prdlessEnabled, setPrdlessEnabled] = useState(false);
  const [vaultUnlocked, setVaultUnlocked] = useState(true);

  // applySession records the user and the instance labels from a session
  // response, falling back to the compiled-in defaults for a server that predates
  // the label fields. It also resolves and applies the theme (PRD #21): the
  // server sends the resolved theme, but we re-resolve from the override +
  // default so a server that predates the theme fields still yields ember, then
  // stamp <html data-theme> so a login/refresh restyles live. prdless_enabled is a
  // bool, so `?? false` (not `||`) keeps an explicit false; an absent field (older
  // server) also reads as off.
  const applySession = useCallback((session: SessionResponse) => {
    setUser(session.user);
    setPrdLabel(session.prd_label || DEFAULT_PRD_LABEL);
    setAutopilotLabel(session.autopilot_label || DEFAULT_AUTOPILOT_LABEL);
    const resolved = resolveTheme(session.theme_override, session.default_theme);
    setThemeOverride(session.theme_override ?? null);
    setDefaultTheme(resolveTheme(session.default_theme, DEFAULT_THEME));
    setTheme(resolved);
    applyTheme(resolved);
    setPrdlessLabel(session.prdless_label || DEFAULT_PRDLESS_LABEL);
    setPrdlessEnabled(session.prdless_enabled ?? false);
    // Absent field (older server) reads as unlocked, so no spurious banner.
    setVaultUnlocked(session.vault?.unlocked ?? true);
  }, []);

  const refresh = useCallback(async () => {
    try {
      applySession(await api.me());
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) {
        setUser(null);
      }
    }
  }, [applySession]);

  // Any authenticated request that comes back 401 (a session expired or deleted
  // mid-session) clears the user here; a rendered ProtectedRoute then redirects
  // to /login (replace). Because we only clear state — never navigate imperatively
  // — the initial me() probe's expected 401 composes without looping: it clears an
  // already-empty session and leaves a signed-out visitor on their public page.
  useEffect(() => {
    setUnauthorizedHandler(() => setUser(null));
    return () => setUnauthorizedHandler(null);
  }, []);

  // Any 409 vault_locked (a save that raced a pod restart) refreshes the session
  // so the SPA learns the vault is locked and shows the unlock banner (PRD #32).
  useEffect(() => {
    setVaultLockedHandler(() => {
      void refresh();
    });
    return () => setVaultLockedHandler(null);
  }, [refresh]);

  // Refresh vault status when the tab regains focus: the DEK cache is per-process,
  // so a restart while the tab was backgrounded flips the vault to locked without
  // any request from this tab. There is no global socket to push it (PRD #32); a
  // focus refresh is the cheap catch-up. Only while signed in.
  useEffect(() => {
    if (!user) return;
    const onFocus = () => {
      void refresh();
    };
    window.addEventListener("focus", onFocus);
    return () => window.removeEventListener("focus", onFocus);
  }, [user, refresh]);

  useEffect(() => {
    (async () => {
      await refresh();
      setLoading(false);
    })();
  }, [refresh]);

  const register = useCallback(
    async (email: string, password: string, displayName: string) => {
      applySession(await api.register(email, password, displayName));
    },
    [applySession],
  );

  const login = useCallback(
    async (email: string, password: string) => {
      applySession(await api.login(email, password));
    },
    [applySession],
  );

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      setUser(null);
    }
  }, []);

  const value = useMemo<AuthState>(
    () => ({
      user,
      loading,
      prdLabel,
      autopilotLabel,
      theme,
      themeOverride,
      defaultTheme,
      prdlessLabel,
      prdlessEnabled,
      vaultUnlocked,
      register,
      login,
      logout,
      refresh,
    }),
    [
      user,
      loading,
      prdLabel,
      autopilotLabel,
      theme,
      themeOverride,
      defaultTheme,
      prdlessLabel,
      prdlessEnabled,
      vaultUnlocked,
      register,
      login,
      logout,
      refresh,
    ],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
