// Settings → Account & tokens: the account card (moved here from the old
// dashboard), the Anthropic token lifecycle, the vault, and appearance. Lives
// inside SettingsShell so tokens/forge/access are one discoverable area. The
// token LIST itself is AnthropicTokens (PRD #104 M6) — this page owns the fetch
// so the list and the rate-limit meters refresh together. Run-behavior defaults
// (autopilot, usage-limit park, judge, CI autofix, MR review rework, worker
// model) moved to the Run defaults tab (RunDefaults.tsx): this tab is who you
// are and what you hold, that one is how your runs behave.

import { useCallback, useEffect, useState } from "react";
import { useAuth } from "../auth/AuthContext";
import { api, type SecretMeta } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { Alert, Button, Card, Field, SectionTitle, Select, Toggle } from "../components/ui";
import { AnthropicTokens } from "../components/AnthropicTokens";
import { SettingsShell } from "../components/SettingsShell";
import { RateLimitCard } from "../components/RateLimitMeters";
import { VaultBadge, useVaultLock } from "../components/VaultControls";
import { SlackNotifications } from "../components/SlackNotifications";
import { prefs } from "../lib/prefs";
import { emitSidebarTokensChanged } from "../lib/sidebarTokens";
import { applyTheme, resolveTheme, THEMES, THEME_LABELS, isTheme } from "../lib/theme";
import { useDemoMode, setDemoMode } from "../lib/demoMode";
import { maskEmail, maskName } from "../lib/demoMask";

// One-time dismissal (per browser) of the rotate-your-legacy-token reminder.
const ROTATE_NOTICE_KEY = "uzi.vault.rotateNoticeDismissed";

export function Settings() {
  const { user, refresh, themeOverride, defaultTheme, vaultUnlocked } = useAuth();
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
  const [secrets, setSecrets] = useState<SecretMeta[]>([]);
  const [loading, setLoading] = useState(true);
  // busy is the page-level guard the token card also respects, so two token
  // mutations cannot race each other's reloads.
  const [busy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  // Which non-default tokens also show on the sidebar rail (the default always
  // does). Owned here beside `secrets` so the checkbox column and the token list
  // load and reload together.
  const [sidebarTokenIds, setSidebarTokenIds] = useState<string[]>([]);

  const load = useCallback(async () => {
    try {
      const [{ secrets: rows }, { settings }] = await Promise.all([
        api.listSecrets(),
        api.getMySettings(),
      ]);
      setSecrets(rows.filter((s) => s.kind === "anthropic_token"));
      setSidebarTokenIds(settings.sidebar_token_ids ?? []);
    } catch (err) {
      setError(errorMessage(err, "Failed to load settings"));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

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

  // Appearance: the per-user theme override. "" = use the instance default. The
  // change is applied live (optimistic) then persisted; a failed save re-syncs
  // from the server, reverting the optimistic stamp.
  const [themeBusy, setThemeBusy] = useState(false);
  const [themeError, setThemeError] = useState("");

  // Demo mode: per-device localStorage flag (NOT the server-backed theme above),
  // so it deliberately gets its own card below Appearance rather than sitting
  // beside the "follows you across browsers" control. Live via useDemoMode().
  const demoMode = useDemoMode();

  const changeTheme = async (value: string) => {
    setThemeError("");
    const override = value === "" ? null : value;
    // Apply immediately so the switch feels live; the server value reconciles.
    applyTheme(resolveTheme(override, defaultTheme));
    setThemeBusy(true);
    try {
      await api.putMySettings({ theme: override });
      await refresh(); // sync themeOverride + re-apply the authoritative theme
    } catch (err) {
      setThemeError(errorMessage(err, "Failed to save theme"));
      await refresh(); // revert the optimistic stamp to the server's truth
    } finally {
      setThemeBusy(false);
    }
  };

  return (
    <SettingsShell description="Your Anthropic tokens, vault, appearance, and account.">
      {error && <Alert message={error} />}
      {notice && <Alert tone="success" message={notice} />}

      <AnthropicTokens
        secrets={secrets}
        loading={loading}
        busy={busy}
        reload={load}
        onError={setError}
        onNotice={setNotice}
        judgeSecretId={user?.judge_anthropic_secret_id ?? null}
        sidebarTokenIds={sidebarTokenIds}
        onToggleSidebarToken={toggleSidebarToken}
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

      <Card className="space-y-5">
        <div>
          <SectionTitle>Appearance</SectionTitle>
          <p className="mt-2 text-sm text-muted">
            The theme uzi renders for you. It follows you across browsers. Leave it on{" "}
            <em>Use default</em> to track the instance default your admin sets.
          </p>
        </div>

        {themeError && <Alert message={themeError} />}

        <div className="max-w-sm">
          <Field label="Theme" htmlFor="appearance-theme">
            <Select
              id="appearance-theme"
              value={isTheme(themeOverride) ? themeOverride : ""}
              disabled={themeBusy}
              onChange={(e) => changeTheme(e.target.value)}
            >
              <option value="">Use default ({THEME_LABELS[defaultTheme]})</option>
              {THEMES.map((t) => (
                <option key={t} value={t}>
                  {THEME_LABELS[t]}
                </option>
              ))}
            </Select>
          </Field>
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
