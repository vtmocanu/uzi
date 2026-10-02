import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  api,
  ApiError,
  setUnauthorizedHandler,
  setVaultLockedHandler,
  DEFAULT_AUTOPILOT_LABEL,
  type AppearanceState,
  type SessionResponse,
  type User,
} from "../lib/api";
import {
  applyAppearance,
  isDarkTheme,
  DEFAULT_LIGHT_THEME,
  DEFAULT_DARK_THEME,
} from "../lib/theme";

// Compiled-in default for the single run-eligibility label (PRD #764). The SPA
// uses it until the session bootstrap resolves the configured `uzi_label`.
export const DEFAULT_UZI_LABEL = "uzi";

// fallbackAppearance synthesises a minimal AppearanceState so the app still paints
// when a server predates PRD #1167's `appearance` object (it sends only the
// deprecated single-theme trio). Pre-#1167 themes are all dark, so the RESOLVED
// theme paints the dark slot pinned to dark mode. Crucially it keys off
// `resolvedTheme` (session.theme — the user's override already applied by the old
// server), not only the instance default, so a user who saved e.g. mission is not
// repainted as the instance-default ember. The raw override rides overrides.dark
// (for the picker); defaults.dark carries the instance default; everything else
// takes compiled defaults. Also seeds the provider's initial state before the
// first session response resolves (resolvedTheme === defaultTheme, no override).
function fallbackAppearance(
  resolvedTheme: string,
  themeOverride: string | null,
  defaultTheme: string,
): AppearanceState {
  const dark = isDarkTheme(resolvedTheme) ? resolvedTheme : DEFAULT_DARK_THEME;
  const defaultDark = isDarkTheme(defaultTheme) ? defaultTheme : DEFAULT_DARK_THEME;
  const overrideDark = isDarkTheme(themeOverride) ? themeOverride : null;
  return {
    mode: "dark",
    light_theme: DEFAULT_LIGHT_THEME,
    dark_theme: dark,
    typeface: "system",
    overrides: { mode: null, light_theme: null, dark_theme: overrideDark, typeface: null },
    defaults: {
      mode: "dark",
      light_theme: DEFAULT_LIGHT_THEME,
      dark_theme: defaultDark,
      typeface: "system",
    },
  };
}

interface AuthState {
  user: User | null;
  loading: boolean;
  // Instance forge labels delivered on the session bootstrap (PRD #19 M2, PRD #764).
  // They hold the compiled-in defaults until the first session response resolves, so
  // consumers (Board, issue creation) can read them unconditionally. uziLabel is the
  // single run-eligibility label: an issue is runnable iff it carries it.
  uziLabel: string;
  autopilotLabel: string;
  // Resolved appearance from the session bootstrap (PRD #1167 "Lights on"). It
  // carries the resolved mode/light_theme/dark_theme/typeface the SPA stamps, plus
  // the raw per-field `overrides` (each null = inherit) and the instance `defaults`
  // the Appearance picker needs to render its controls and the "Use instance
  // defaults" action. Stamping <html> happens in applySession via applyAppearance;
  // no component reads this to branch — only the Appearance card renders from it.
  appearance: AppearanceState;
  // Vault status (PRD #32): true when the user's secret vault is unlocked in the
  // server process. Rides the session payload; drives the header badge, the locked
  // banner, and the "waiting for vault unlock" run state. Defaults to true (a
  // server that predates the field, or a signed-out visitor, shows no banner).
  vaultUnlocked: boolean;
  // vaultExists is whether the user has a vault row at all; hasPassword is false for
  // OIDC-only users (PRD #45). Together they let the locked banner choose between the
  // passphrase-create dialog (passwordless + no vault yet) and the unlock banner.
  // Both default true so password users and older servers keep the existing flow.
  vaultExists: boolean;
  hasPassword: boolean;
  // Judge consent (PRD #69 M4). judgeEnforcedByAdmin is true when the admin has put
  // the judge in ENFORCED mode (kill-switch on AND enforce_all on): the RunDefaults
  // card then shows the enforced banner. effectiveJudgeModel is the model this user's
  // judge actually runs on after per-user→instance→default resolution. Both default to
  // the safe reading (not enforced, "") so an older server never fabricates enforcement.
  judgeEnforcedByAdmin: boolean;
  effectiveJudgeModel: string;
  register: (email: string, password: string, displayName: string) => Promise<void>;
  login: (email: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
  // serverUnreachable is true when the session probe failed for a reason other than
  // 401 (a 503 from a transient DB outage, or a network error), so the SPA does not
  // know whether the visitor is signed in. Route guards show a retry panel instead
  // of bouncing to /login while it is set and there is no user. Cleared by any
  // successful session response, a 401, a login/register, or a logout.
  serverUnreachable: boolean;
  // retry re-runs the session probe now; the provider also re-runs it every 5s
  // while serverUnreachable and signed out. Same function as refresh.
  retry: () => Promise<void>;
}

// Re-probe cadence while the server is unreachable and nobody is signed in.
const RETRY_INTERVAL_MS = 5000;

const AuthContext = createContext<AuthState | undefined>(undefined);

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);
  const [uziLabel, setUziLabel] = useState(DEFAULT_UZI_LABEL);
  const [autopilotLabel, setAutopilotLabel] = useState(DEFAULT_AUTOPILOT_LABEL);
  const [appearance, setAppearance] = useState<AppearanceState>(() =>
    fallbackAppearance("ember", null, "ember"),
  );
  const [vaultUnlocked, setVaultUnlocked] = useState(true);
  const [vaultExists, setVaultExists] = useState(true);
  const [hasPassword, setHasPassword] = useState(true);
  const [judgeEnforcedByAdmin, setJudgeEnforcedByAdmin] = useState(false);
  const [effectiveJudgeModel, setEffectiveJudgeModel] = useState("");
  const [serverUnreachable, setServerUnreachable] = useState(false);
  // At most one session probe is in flight: a refresh while one is pending joins it,
  // so probe results can never settle out of order. sessionGen is bumped only by an
  // explicit login, register or logout, or by another request's 401; a probe that
  // started before one of those drops its result, so it cannot undo the newer state.
  const probeInFlight = useRef<Promise<void> | null>(null);
  const sessionGen = useRef(0);

  // applySession records the user and the instance labels from a session
  // response, falling back to the compiled-in defaults for a server that predates
  // the label fields. It also stores the resolved appearance and stamps <html>
  // (PRD #1167): the server sends the resolved appearance, but a server that
  // predates the field is handled defensively by synthesising a minimal one from
  // the deprecated single-theme trio so the app still paints. applyAppearance
  // stamps data-theme/data-font and arms the live system-mode listener.
  const applySession = useCallback((session: SessionResponse) => {
    setUser(session.user);
    setServerUnreachable(false);
    setLoading(false);
    setUziLabel(session.uzi_label || DEFAULT_UZI_LABEL);
    setAutopilotLabel(session.autopilot_label || DEFAULT_AUTOPILOT_LABEL);
    // Read as possibly-absent: an older server omits `appearance` entirely, so we
    // synthesise one from its resolved theme trio (which already reflects the user's
    // override), not just the instance default_theme.
    const nextAppearance =
      (session.appearance as AppearanceState | undefined) ??
      fallbackAppearance(session.theme, session.theme_override, session.default_theme);
    setAppearance(nextAppearance);
    applyAppearance({
      mode: nextAppearance.mode,
      light: nextAppearance.light_theme,
      dark: nextAppearance.dark_theme,
      typeface: nextAppearance.typeface,
    });
    // Absent field (older server) reads as unlocked, so no spurious banner.
    setVaultUnlocked(session.vault?.unlocked ?? true);
    // Absent → true so a password user / older server never sees the create dialog.
    setVaultExists(session.vault?.exists ?? true);
    setHasPassword(session.has_password ?? true);
    // Both are bools/strings, so `?? false` / `?? ""` (not `||`) preserve an explicit
    // value while an absent field (older server) reads as the safe not-enforced state.
    setJudgeEnforcedByAdmin(session.judge_enforced_by_admin ?? false);
    setEffectiveJudgeModel(session.effective_judge_model ?? "");
  }, []);

  const refresh = useCallback(() => {
    if (probeInFlight.current) return probeInFlight.current;
    const gen = sessionGen.current;
    const probe = (async () => {
      try {
        const session = await api.me();
        if (gen !== sessionGen.current) return;
        applySession(session);
      } catch (err) {
        if (gen !== sessionGen.current) return;
        if (err instanceof ApiError && err.status === 401) {
          setUser(null);
          setServerUnreachable(false);
        } else {
          // A 503 (transient DB outage), another non-401 status, or a network
          // failure: the session state is unknown, so leave user untouched.
          setServerUnreachable(true);
        }
        setLoading(false);
      } finally {
        probeInFlight.current = null;
      }
    })();
    probeInFlight.current = probe;
    return probe;
  }, [applySession]);

  // Any authenticated request that comes back 401 (a session expired or deleted
  // mid-session) clears the user here; a rendered ProtectedRoute then redirects
  // to /login (replace). We only clear state, never navigate imperatively. The
  // session probe's own 401 does not come through here (api.me opts out); refresh
  // handles it behind the generation check.
  useEffect(() => {
    setUnauthorizedHandler(() => {
      // Another request's 401: the session is gone. Invalidate any pending probe
      // so its later result cannot restore the user.
      sessionGen.current += 1;
      setUser(null);
      setServerUnreachable(false);
      setLoading(false);
    });
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

  // The initial probe. refresh() clears loading when its result is applied; a
  // StrictMode re-run joins the same in-flight probe instead of starting another.
  useEffect(() => {
    void refresh();
  }, [refresh]);

  // While the server is unreachable and nobody is signed in, re-probe on a timer.
  // Each failed probe leaves serverUnreachable true, so the effect does not re-run;
  // chain the next attempt from the probe's completion instead of a state change.
  useEffect(() => {
    if (!serverUnreachable || user) return;
    let timer: ReturnType<typeof setTimeout> | undefined;
    let cancelled = false;
    const schedule = () => {
      timer = setTimeout(async () => {
        await refresh();
        if (!cancelled) schedule();
      }, RETRY_INTERVAL_MS);
    };
    schedule();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [serverUnreachable, user, refresh]);

  const register = useCallback(
    async (email: string, password: string, displayName: string) => {
      const session = await api.register(email, password, displayName);
      sessionGen.current += 1;
      applySession(session);
    },
    [applySession],
  );

  const login = useCallback(
    async (email: string, password: string) => {
      const session = await api.login(email, password);
      sessionGen.current += 1;
      applySession(session);
    },
    [applySession],
  );

  const logout = useCallback(async () => {
    try {
      await api.logout();
    } finally {
      sessionGen.current += 1;
      setUser(null);
      setServerUnreachable(false);
      setLoading(false);
    }
  }, []);

  const value = useMemo<AuthState>(
    () => ({
      user,
      loading,
      uziLabel,
      autopilotLabel,
      appearance,
      vaultUnlocked,
      vaultExists,
      hasPassword,
      judgeEnforcedByAdmin,
      effectiveJudgeModel,
      register,
      login,
      logout,
      refresh,
      serverUnreachable,
      retry: refresh,
    }),
    [
      user,
      loading,
      uziLabel,
      autopilotLabel,
      appearance,
      vaultUnlocked,
      vaultExists,
      hasPassword,
      judgeEnforcedByAdmin,
      effectiveJudgeModel,
      register,
      login,
      logout,
      refresh,
      serverUnreachable,
    ],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
