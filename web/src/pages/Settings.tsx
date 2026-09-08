// Settings → Account & tokens: the account card (moved here from the old
// dashboard), the Anthropic token lifecycle, the vault, and appearance. Lives
// inside SettingsShell so tokens/forge/access are one discoverable area. The
// token LIST itself is AnthropicTokens (PRD #104 M6) — this page owns the fetch
// so the list and the rate-limit meters refresh together. Run-behavior defaults
// (autopilot, usage-limit park, judge, CI autofix, MR review rework, worker
// model) moved to the Run defaults tab (RunDefaults.tsx): this tab is who you
// are and what you hold, that one is how your runs behave.

import { useMemo, useState, useSyncExternalStore } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, type UserSettingsPatch } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { Alert, Button, Card, cx, SectionTitle, Toggle } from "../components/ui";
import { AnthropicTokens } from "../components/AnthropicTokens";
import { CodexCredentials } from "../components/CodexCredentials";
import { SettingsShell } from "../components/SettingsShell";
import { RateLimitCard } from "../components/RateLimitMeters";
import { VaultBadge, useVaultLock } from "../components/VaultControls";
import { SlackNotifications } from "../components/SlackNotifications";
import { prefs } from "../lib/prefs";
import { emitSidebarTokensChanged } from "../lib/sidebarTokens";
import {
  applyAppearance,
  isTheme,
  THEME_LABELS,
  LIGHT_THEMES,
  DARK_THEMES,
  type Theme,
} from "../lib/theme";
import { useDemoMode, setDemoMode } from "../lib/demoMode";
import { maskEmail, maskName } from "../lib/demoMask";

// One-time dismissal (per browser) of the rotate-your-legacy-token reminder.
const ROTATE_NOTICE_KEY = "uzi.vault.rotateNoticeDismissed";

// TYPEFACE_ENABLED gates the typeface control. The IBM Plex family is now bundled
// (PRD #1167 m6, @fontsource/ibm-plex-{sans,mono} imported in main.tsx), so the
// picker is live. Kept as the single on/off switch the m6 flip turned on.
const TYPEFACE_ENABLED = true;

// Appearance-mode choices: the polarity switch. "system" follows the OS, the other
// two pin a polarity ("Lights on" = light, "Lights off" = dark).
const MODE_OPTIONS: { value: string; label: string }[] = [
  { value: "system", label: "System" },
  { value: "light", label: "Lights on" },
  { value: "dark", label: "Lights off" },
];

// Typeface choices (PRD #1167 m6): the platform default or the bundled IBM Plex.
const TYPEFACE_OPTIONS: { value: string; label: string }[] = [
  { value: "system", label: "System" },
  { value: "plex", label: "IBM Plex" },
];

// Keyboard-focus ring on the appearance option cards (WCAG 2.4.7). Every card is a
// <label> wrapping an sr-only radio, so the global input:focus-visible ring lands on
// the clipped input and is invisible; project it onto the card with :has(:focus-visible).
// Same utility the AgentPicker cards use, kept identical for one focus language.
const CARD_FOCUS_RING =
  "has-[:focus-visible]:outline has-[:focus-visible]:outline-2 has-[:focus-visible]:outline-brand has-[:focus-visible]:outline-offset-2";

// prefersDarkSnapshot / subscribe back a useSyncExternalStore so the Appearance UI
// tracks the OS colour-scheme LIVE. Under "system" mode the painted theme already
// re-stamps on an OS flip (theme.ts applyAppearance's matchMedia listener); this
// keeps the Settings UI (which polarity paints, the picker dimming, the "Showing…"
// line) in step instead of stale until the next render. No-matchMedia / SSR reads false.
function prefersDarkSnapshot(): boolean {
  return typeof window !== "undefined" && !!window.matchMedia
    ? window.matchMedia("(prefers-color-scheme: dark)").matches
    : false;
}

function subscribePrefersDark(onChange: () => void): () => void {
  if (typeof window === "undefined" || !window.matchMedia) return () => {};
  const mql = window.matchMedia("(prefers-color-scheme: dark)");
  mql.addEventListener("change", onChange);
  return () => mql.removeEventListener("change", onChange);
}

function usePrefersDark(): boolean {
  return useSyncExternalStore(subscribePrefersDark, prefersDarkSnapshot, () => false);
}

// The three light themes (dawn/hall/shadow) share ONE token set in index.css
// ([data-theme="dawn"],[data-theme="hall"],[data-theme="shadow"]): the cool-steel
// ground #F2F4F7 and the rust brand #B93C0B. They differ only in WHERE the dark
// factory shows through — the console pane (dawn), the sidebar frame (hall), or
// live-run cards (shadow). DARK_FACTORY is the ember dark the factory reveals
// (index.css sets --bg to that dark ground — 8 10 15 = #080a0f, the live ember
// --bg — on .console/.frame/[data-live]), so the swatch chip matches what ships.
const LIGHT_GROUND = "#f2f4f7";
const LIGHT_BRAND = "#b93c0b";
const DARK_FACTORY = "#080a0f";

// Representative swatch colours per theme id: the ground (page background) and the
// brand accent, so each theme card previews at a glance without loading its CSS.
// Hardcoded on purpose (the live tokens live in index.css and are not readable as
// values here); a new theme adds one row alongside its theme.ts entry. `accent`
// (light themes only) names WHERE the shared light palette reveals the dark
// factory, so the three otherwise-identical light swatches read distinctly — the
// ThemePicker markup places a small dark chip accordingly.
const THEME_SWATCH: Record<
  Theme,
  { ground: string; brand: string; accent?: "console" | "frame" | "live" }
> = {
  ember: { ground: "#080a0f", brand: "#fb923c" },
  mission: { ground: "#05080f", brand: "#22d3ee" },
  dawn: { ground: LIGHT_GROUND, brand: LIGHT_BRAND, accent: "console" },
  hall: { ground: LIGHT_GROUND, brand: LIGHT_BRAND, accent: "frame" },
  shadow: { ground: LIGHT_GROUND, brand: LIGHT_BRAND, accent: "live" },
};

// ThemePicker renders a radio group of theme cards (a swatch + label each) for one
// polarity. `dimmed` reduces opacity for the polarity that cannot currently apply
// (the user can still change it — it is neither hidden nor disabled); `disabled`
// (the in-flight busy flag) actually blocks input while a save is pending.
function ThemePicker({
  groupLabel,
  themes,
  selected,
  dimmed,
  disabled,
  onPick,
}: {
  groupLabel: string;
  themes: readonly Theme[];
  selected: string;
  dimmed: boolean;
  disabled: boolean;
  onPick: (t: Theme) => void;
}) {
  return (
    <div
      role="radiogroup"
      aria-label={groupLabel}
      className={cx("grid grid-cols-2 gap-2 sm:grid-cols-3", dimmed && "opacity-60")}
    >
      {themes.map((t) => {
        const sw = THEME_SWATCH[t];
        const active = selected === t;
        return (
          <label
            key={t}
            className={cx(
              "flex items-center gap-2 rounded-lg border px-3 py-2 text-sm transition-colors",
              CARD_FOCUS_RING,
              active ? "border-brand bg-raised" : "border-edge hover:border-edge-strong",
              disabled ? "cursor-not-allowed" : "cursor-pointer",
            )}
          >
            <input
              type="radio"
              name={groupLabel}
              value={t}
              checked={active}
              disabled={disabled}
              onChange={() => onPick(t)}
              className="sr-only"
            />
            <span
              aria-hidden="true"
              className="relative flex h-5 w-8 shrink-0 overflow-hidden rounded border border-edge"
            >
              {/* Ground + brand: accurate for every theme (the three light themes
                  share one palette, so these are identical across dawn/hall/shadow). */}
              <span className="h-full w-1/2" style={{ backgroundColor: sw.ground }} />
              <span className="h-full w-1/2" style={{ backgroundColor: sw.brand }} />
              {/* Light themes only: a small dark chip placed to signal WHERE this
                  theme reveals the dark factory, so the shared palette still reads as
                  three distinct themes. Dawn = a dark console-pane block; Hall = a
                  dark sidebar strip down the left edge; Shadow = a dark live-run pill. */}
              {sw.accent === "console" && (
                <span
                  className="absolute left-1 top-1 h-2.5 w-3 rounded-sm"
                  style={{ backgroundColor: DARK_FACTORY }}
                />
              )}
              {sw.accent === "frame" && (
                <span
                  className="absolute inset-y-0 left-0 w-1.5"
                  style={{ backgroundColor: DARK_FACTORY }}
                />
              )}
              {sw.accent === "live" && (
                <span
                  className="absolute bottom-0.5 right-0.5 h-2 w-2.5 rounded-full"
                  style={{ backgroundColor: DARK_FACTORY }}
                />
              )}
            </span>
            <span className="text-fg">{THEME_LABELS[t]}</span>
          </label>
        );
      })}
    </div>
  );
}

export function Settings() {
  const { user, refresh, appearance, vaultUnlocked } = useAuth();
  const [vaultNotice, setVaultNotice] = useState("");
  const { lock, locking } = useVaultLock(() =>
    setVaultNotice(
      "Vault locked. Runs already in flight finish; your new runs wait as “waiting for vault unlock” until you unlock again.",
    ),
  );
  const [rotateDismissed, setRotateDismissed] = useState(() => prefs.get(ROTATE_NOTICE_KEY, false));
  const dismissRotate = () => {
    prefs.set(ROTATE_NOTICE_KEY, true);
    setRotateDismissed(true);
  };
  // busy is the page-level guard the token card also respects, so two token
  // mutations cannot race each other's reloads.
  const [busy] = useState(false);
  // `error` is the load error (from the hook, as `loadError`) OR an error set by a
  // mutation handler / the token card here — the two share the one Alert slot below.
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  // Which non-default tokens also show on the sidebar rail (the default always
  // does). Owned here beside `secrets` so the checkbox column and the token list
  // load and reload together.
  const [sidebarTokenIds, setSidebarTokenIds] = useState<string[]>([]);

  const { data, loading, error: loadError, reload } = useAsyncData(
    async () => {
      const [{ secrets: rows }, { settings }] = await Promise.all([
        api.listSecrets(),
        api.getMySettings(),
      ]);
      setSidebarTokenIds(settings.sidebar_token_ids ?? []);
      // ONE listSecrets() call feeds both cards: the Anthropic slice is unchanged,
      // and the Codex slice is the union of the two Codex kinds (they share one
      // card and one default across both).
      return {
        secrets: rows.filter((s) => s.kind === "anthropic_token"),
        codexSecrets: rows.filter(
          (s) => s.kind === "codex_auth" || s.kind === "openai_api_key",
        ),
      };
    },
    [],
    { fallback: "Failed to load settings" },
  );
  const secrets = useMemo(() => data?.secrets ?? [], [data]);
  const codexSecrets = useMemo(() => data?.codexSecrets ?? [], [data]);

  // Whole-set replace over PUT /me/settings, then tell the sidebar rail (a
  // separate mount) to refetch now rather than on its next poll.
  const toggleSidebarToken = async (id: string, shown: boolean) => {
    setError("");
    const next = shown
      ? [...new Set([...sidebarTokenIds, id])]
      : sidebarTokenIds.filter((x) => x !== id);
    try {
      const { settings } = await api.putMySettings({ sidebar_token_ids: next });
      setSidebarTokenIds(settings.sidebar_token_ids ?? next);
      emitSidebarTokensChanged();
    } catch (err) {
      setError(errorMessage(err, "Failed to update the sidebar meters"));
    }
  };

  // Appearance (PRD #1167 "Lights on"): the per-user mode + light/dark theme pair
  // + typeface, each field a tri-state override (null = inherit the instance
  // default). Every control follows one OPTIMISTIC pattern: compute the new
  // resolved appearance locally, applyAppearance immediately so the change feels
  // live, then persist and refresh; a failed save re-syncs from the server,
  // reverting the optimistic stamp. `appearanceBusy` disables the controls while a
  // request is in flight.
  const [appearanceBusy, setAppearanceBusy] = useState(false);
  const [appearanceError, setAppearanceError] = useState("");

  // Demo mode: per-device localStorage flag (NOT the server-backed appearance
  // above), so it deliberately gets its own card below Appearance rather than
  // sitting beside the "follows you across browsers" control. Live via useDemoMode().
  const demoMode = useDemoMode();

  // The current resolved appearance as applyAppearance's argument shape; each
  // handler replaces exactly one field before stamping.
  const current = {
    mode: appearance.mode,
    light: appearance.light_theme,
    dark: appearance.dark_theme,
    typeface: appearance.typeface,
  };

  const saveAppearance = async (
    optimistic: { mode: string; light: string; dark: string; typeface: string },
    patch: UserSettingsPatch,
  ) => {
    setAppearanceError("");
    // Apply immediately so the switch feels live; the server value reconciles.
    applyAppearance(optimistic);
    setAppearanceBusy(true);
    try {
      await api.putMySettings(patch);
      await refresh(); // sync overrides + re-apply the authoritative appearance
    } catch (err) {
      setAppearanceError(errorMessage(err, "Failed to save appearance"));
      await refresh(); // revert the optimistic stamp to the server's truth
    } finally {
      setAppearanceBusy(false);
    }
  };

  const pickMode = (mode: string) =>
    saveAppearance({ ...current, mode }, { appearance_mode: mode });
  const pickLight = (light: string) =>
    saveAppearance({ ...current, light }, { light_theme: light });
  const pickDark = (dark: string) =>
    saveAppearance({ ...current, dark }, { dark_theme: dark });
  const pickTypeface = (typeface: string) =>
    saveAppearance({ ...current, typeface }, { typeface });
  const useInstanceDefaults = () =>
    saveAppearance(
      {
        mode: appearance.defaults.mode,
        light: appearance.defaults.light_theme,
        dark: appearance.defaults.dark_theme,
        typeface: appearance.defaults.typeface,
      },
      { appearance_mode: null, light_theme: null, dark_theme: null, typeface: null },
    );

  // Which polarity currently paints (so the other picker is dimmed). An explicit
  // mode names it; under "system" it is the OS preference, tracked reactively so an
  // OS light/dark flip re-renders this UI (not just the painted theme).
  const osDark = usePrefersDark();
  const activePolarity: "light" | "dark" =
    appearance.mode === "light"
      ? "light"
      : appearance.mode === "dark"
        ? "dark"
        : osDark
          ? "dark"
          : "light";
  const themeLabel = (id: string) => (isTheme(id) ? THEME_LABELS[id] : id);
  const activeTheme =
    activePolarity === "light" ? appearance.light_theme : appearance.dark_theme;
  const otherPolarity = activePolarity === "light" ? "dark" : "light";
  const otherTheme =
    otherPolarity === "light" ? appearance.light_theme : appearance.dark_theme;
  const otherModeLabel = otherPolarity === "light" ? "Lights on" : "Lights off";

  return (
    <SettingsShell description="Your Anthropic tokens, OpenAI / Codex credentials, vault, appearance, and account.">
      {(loadError || error) && <Alert message={loadError || error} />}
      {notice && <Alert tone="success" message={notice} />}

      <AnthropicTokens
        secrets={secrets}
        loading={loading}
        busy={busy}
        reload={reload}
        onError={setError}
        onNotice={setNotice}
        judgeSecretId={user?.judge_anthropic_secret_id ?? null}
        sidebarTokenIds={sidebarTokenIds}
        onToggleSidebarToken={toggleSidebarToken}
      />

      {/* OpenAI / Codex credentials (PRD #1147 M3). Fed by the SAME listSecrets()
          call above; shares this page's error/notice slots and reload. */}
      <CodexCredentials
        secrets={codexSecrets}
        loading={loading}
        busy={busy}
        reload={reload}
        onError={setError}
        onNotice={setNotice}
      />

      {/* The rotate-your-legacy-token reminder (PRD #32): password protection
          applies from the save forward, never retroactively. */}
      {secrets.length > 0 && !rotateDismissed && (
        <div className="rounded-lg border border-info/40 bg-info/10 px-4 py-3 text-sm text-info">
          <p className="text-fg">
            <strong className="font-semibold">Protecting an older token?</strong> If you first saved
            a token before password-protection was enabled, an operator could have read it. The
            protection applies from the moment you save, not retroactively — for full protection,
            rotate the token in the Anthropic console and replace its value above.
          </p>
          <button
            type="button"
            onClick={dismissRotate}
            className="mt-2 text-xs font-medium text-brand hover:text-brand-hover"
          >
            Got it, dismiss
          </button>
        </div>
      )}

      {/* Claude rate-limit meters (PRD #53). Self-gates: hidden when no token is
          set, greyed on "unavailable", live meters once a reading lands. */}
      <RateLimitCard />

      <Card className="space-y-4">
        <div>
          <SectionTitle>Vault</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            Your Anthropic token is sealed with a key derived from your login password, so it is
            readable only while your vault is unlocked in this session. It unlocks
            automatically when you log in and stays unlocked — including overnight — until you lock it
            or the server restarts. While locked, your agents pause: new runs wait as{" "}
            <em>waiting for vault unlock</em> rather than failing.
          </p>
        </div>

        {/* Info (not success/green): locking flips the badge amber, so a green
            "success" toast would clash with the color language. */}
        {vaultNotice && <Alert tone="info" message={vaultNotice} />}

        <div className="flex items-center justify-between rounded-lg border border-edge bg-raised/60 px-4 py-3">
          <VaultBadge />
          {vaultUnlocked ? (
            <Button variant="secondary" size="sm" disabled={locking} onClick={lock}>
              {locking ? "Locking…" : "Lock vault"}
            </Button>
          ) : (
            <span className="text-sm text-muted">Locked — unlock from the banner above.</span>
          )}
        </div>
      </Card>

      <Card className="space-y-6">
        <div>
          <SectionTitle>Appearance</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            How uzi looks for you. Your choices follow you across browsers. Leave
            everything untouched to track the instance defaults your admin sets, or
            use <em>Use instance defaults</em> below to clear your choices.
          </p>
        </div>

        {appearanceError && <Alert message={appearanceError} />}

        {/* MODE: the light/dark polarity switch. */}
        <div className="space-y-2">
          <p className="text-sm font-medium text-muted">Mode</p>
          <div
            role="radiogroup"
            aria-label="Appearance mode"
            className="flex flex-wrap gap-2"
          >
            {MODE_OPTIONS.map(({ value, label }) => {
              const active = appearance.mode === value;
              return (
                <label
                  key={value}
                  className={cx(
                    "rounded-lg border px-3 py-1.5 text-sm transition-colors",
                    CARD_FOCUS_RING,
                    active
                      ? "border-brand bg-raised text-fg"
                      : "border-edge text-muted hover:border-edge-strong",
                    appearanceBusy ? "cursor-not-allowed" : "cursor-pointer",
                  )}
                >
                  <input
                    type="radio"
                    name="appearance-mode"
                    value={value}
                    checked={active}
                    disabled={appearanceBusy}
                    onChange={() => pickMode(value)}
                    className="sr-only"
                  />
                  {label}
                </label>
              );
            })}
          </div>
          <p className="text-xs text-faint">
            Showing {themeLabel(activeTheme)} ({activePolarity}). {otherModeLabel}{" "}
            shows {themeLabel(otherTheme)}.
          </p>
        </div>

        {/* LIGHTS ON (light-polarity) theme picker. Dimmed when dark is painting. */}
        <div className="space-y-2">
          <p className="text-sm font-medium text-muted">Lights on theme</p>
          <ThemePicker
            groupLabel="Lights on theme"
            themes={LIGHT_THEMES}
            selected={appearance.light_theme}
            dimmed={activePolarity !== "light"}
            disabled={appearanceBusy}
            onPick={pickLight}
          />
        </div>

        {/* LIGHTS OFF (dark-polarity) theme picker. Dimmed when light is painting. */}
        <div className="space-y-2">
          <p className="text-sm font-medium text-muted">Lights off theme</p>
          <ThemePicker
            groupLabel="Lights off theme"
            themes={DARK_THEMES}
            selected={appearance.dark_theme}
            dimmed={activePolarity !== "dark"}
            disabled={appearanceBusy}
            onPick={pickDark}
          />
        </div>

        {/* TYPEFACE: live since m6 — IBM Plex is bundled (@fontsource). */}
        <div className="space-y-2">
          <p className="text-sm font-medium text-muted">Typeface</p>
          <div
            role="radiogroup"
            aria-label="Typeface"
            className={cx("flex flex-wrap gap-2", !TYPEFACE_ENABLED && "opacity-60")}
          >
            {TYPEFACE_OPTIONS.map(({ value, label }) => {
              const active = appearance.typeface === value;
              const disabled = !TYPEFACE_ENABLED || appearanceBusy;
              return (
                <label
                  key={value}
                  className={cx(
                    "rounded-lg border px-3 py-1.5 text-sm transition-colors",
                    CARD_FOCUS_RING,
                    active
                      ? "border-brand bg-raised text-fg"
                      : "border-edge text-muted",
                    disabled ? "cursor-not-allowed" : "cursor-pointer hover:border-edge-strong",
                  )}
                >
                  <input
                    type="radio"
                    name="appearance-typeface"
                    value={value}
                    checked={active}
                    disabled={disabled}
                    onChange={() => pickTypeface(value)}
                    className="sr-only"
                  />
                  {label}
                </label>
              );
            })}
          </div>
        </div>

        <div className="border-t border-edge pt-4">
          <Button
            variant="secondary"
            size="sm"
            disabled={appearanceBusy}
            onClick={useInstanceDefaults}
          >
            Use instance defaults
          </Button>
        </div>
      </Card>

      {/* Demo mode (PRD #886). Deliberately its OWN card below Appearance so it is
          not confused with the server-backed theme control that "follows you across
          browsers" — this one is per-device (localStorage) and screenshot-only. */}
      <Card className="space-y-5">
        <div>
          <SectionTitle>Demo mode</SectionTitle>
          <p id="demo-mode-desc" className="mt-2 text-sm text-muted">
            This device only. Masks emails, repo names, forge host, and other
            identifying info in what you see — for screenshots. Doesn&apos;t change
            your data or affect anyone else.
          </p>
        </div>

        <div className="flex items-center justify-between rounded-lg border border-edge bg-raised/60 px-4 py-3">
          <span className="text-sm font-medium text-fg">
            Demo mode: {demoMode ? "On" : "Off"}
          </span>
          <Toggle
            checked={demoMode}
            onChange={setDemoMode}
            label="Demo mode"
            aria-describedby="demo-mode-desc"
          />
        </div>
      </Card>

      <SlackNotifications />

      {user && (
        <Card>
          <SectionTitle>Your account</SectionTitle>
          <dl className="mt-3 divide-y divide-edge">
            {(
              [
                ["Email", maskEmail(user.email, demoMode)],
                ["Display name", user.display_name !== null ? maskName(user.display_name, demoMode) : "—"],
                ["Role", user.is_admin ? "Administrator" : "User"],
                ["Account status", user.is_active ? "Active" : "Deactivated"],
                ["Joined", new Date(user.created_at).toLocaleString()],
                ["Last login", user.last_login ? new Date(user.last_login).toLocaleString() : "—"],
              ] as [string, string][]
            ).map(([k, v]) => (
              <div key={k} className="flex justify-between py-2 text-sm">
                <dt className="text-muted">{k}</dt>
                <dd className="text-fg">{v}</dd>
              </div>
            ))}
          </dl>
        </Card>
      )}
    </SettingsShell>
  );
}
