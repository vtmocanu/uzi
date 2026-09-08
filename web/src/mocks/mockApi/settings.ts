import {
  type AppSettings,
  type BindMode,
  type ReleaseCheckStatus,
  type SettingSource,
  type SettingsResponse,
  type SlackLink,
  type UpdateSettingsPayload,
  type UserSettings,
  type UserSettingsPatch,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import {
  isAppearanceMode,
  isDarkTheme,
  isLightTheme,
  isTheme,
  isTypeface,
  resolveAppearance,
  THEME_POLARITY,
  type Theme,
} from "../../lib/theme";
import { daysAgo, mockBuildInfo } from "../data";
import { state } from "../store";
import { delay, oidcDemo, requireSession, users } from "./shared";
import { agentSource, mockAllowedAgentSourceHosts } from "./agentSource";
import { secrets } from "./secrets";

// ── Settings persistence (demo build) ────────────────────────────────────────
// The mock persists ONLY the settings maps to localStorage so a hard reload of
// the demo keeps the picked theme (and labels / worker model) instead of snapping
// back to seed — making no-flash + persistence witnessable end to end in the
// sanctioned preview vehicle. Runs, issues, workers, secrets etc. are
// deliberately NOT persisted. Versioned + shape-checked: a blob from an older
// seed schema (or a corrupt one) is discarded and re-seeded, never served, so
// stale demo state can't outlive a seed-schema change.
// Bumped to v2 for PRD #47 (the six health_* keys joined AppSettings): a stale v1
// blob lacks them, so discarding it re-seeds a complete shape.
// Bumped to v3 for PRD #69 M4 (judge_enforce_all / judge_cooldown_seconds /
// judge_daily_budget joined AppSettings, judge_model joined UserSettings): a stale v2
// blob lacks them, so discarding it re-seeds a complete shape.
// Bumped to v4 for PRD #1167 "Lights on" m2 (the four appearance override fields joined
// UserSettings, the four default_* appearance fields joined AppSettings): a stale v3
// blob lacks them, so discarding it re-seeds a complete shape.
const MOCK_SETTINGS_KEY = "uzi.mock.v4";
const SEED_USER_SETTINGS: UserSettings = {
  default_model: null,
  default_effort: null,
  judge_model: null,
  summary_model: null,
  theme: null,
  // PRD #1167 "Lights on" m2: the four raw appearance overrides, all null = inherit
  // the instance defaults.
  appearance_mode: null,
  light_theme: null,
  dark_theme: null,
  typeface: null,
  sidebar_token_ids: [],
  // PRD #700 M6: MR review watcher per-user opt-in. null = the default-ON state;
  // an explicit false opts the account out.
  mr_rework_enabled: null,
};
const SEED_APP_SETTINGS: AppSettings = {
  autopilot_label: "autopilot",
  // PRD #764: the single run-eligibility label.
  uzi_label: "uzi",
  default_theme: "ember",
  // PRD #1167 "Lights on" m2: the compiled instance appearance defaults. default_dark_theme
  // mirrors the legacy default_theme ("ember"); the mode defaults to dark, the light-slot
  // default is "hall", and the typeface default is the platform "system" — matching the
  // theme.ts DEFAULT_* fallbacks.
  default_appearance_mode: "dark",
  default_light_theme: "hall",
  default_dark_theme: "ember",
  default_typeface: "system",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  // PRD #69: enforced mode off, spend guards at their server defaults (cooldown 60s,
  // budget 0 = unlimited).
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
  // PRD #914: instance-wide CI-autofix kill-switch, default ON.
  ci_autofix_enabled: "true",
  // PRD #529 / #649 M1: ephemeral worker auto-provisioning instance kill-switch, default OFF.
  ephemeral_workers_enabled: "false",
  // PRD #836: upstream release-check toggles, both default ON (the master air-gap
  // switch and the escalation-banner switch). The Updates card reads the live values
  // off getReleaseCheck and writes them back here through updateSettings.
  release_check_enabled: "true",
  release_check_banner_enabled: "true",
  // PRD #362 Decision 8: the run-summary generator model, haiku by default.
  summary_model: "haiku",
  health_enabled: "true",
  health_stall_seconds: "300",
  // PRD #1170: near-timeout at 85% of the run's wall-clock budget (replaces the old
  // wall-clock "slow after seconds" default of 2700).
  health_near_timeout_pct: "85",
  health_queued_seconds: "600",
  health_approval_seconds: "3600",
  health_nudge_cooldown_seconds: "1800",
  docker_repo_allowlist: "",
  // PRD #84 M2: capability-aware scheduling kill-switch, default ON.
  capability_aware_scheduling: "true",
  // Issue #534 M2: GitHub Projects v2 sync instance kill-switch, default OFF.
  github_project_sync_enabled: "false",
  // PRD #685: instance branding config, all string-space. Fresh installs are
  // unbranded (app_logo_mode "default", brand_mode "none").
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_keep_name: "true",
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: "false",
};

interface PersistedSettings {
  v: 1;
  userSettings: UserSettings;
  appSettings: AppSettings;
}

// isPersistedSettings validates the version AND the shape (key presence + value
// types) so only a blob matching the current schema is trusted; anything else
// falls through to a fresh seed.
function isPersistedSettings(p: unknown): p is PersistedSettings {
  if (typeof p !== "object" || p === null) return false;
  const o = p as Record<string, unknown>;
  if (o.v !== 1) return false;
  const us = o.userSettings;
  const as = o.appSettings;
  if (typeof us !== "object" || us === null || typeof as !== "object" || as === null) return false;
  const u = us as Record<string, unknown>;
  const a = as as Record<string, unknown>;
  const okUser =
    (u.default_model === null || typeof u.default_model === "string") &&
    // Optional so a pre-#617 blob stays valid; absent reads as inherit, and the SEED
    // provides it going forward (like judge_model).
    (u.default_effort === undefined || u.default_effort === null || typeof u.default_effort === "string") &&
    // Optional so a pre-#69 blob stays valid; absent reads as inherit.
    (u.judge_model === undefined || u.judge_model === null || typeof u.judge_model === "string") &&
    // Optional so a pre-#362 blob stays valid; absent reads as inherit.
    (u.summary_model === undefined || u.summary_model === null || typeof u.summary_model === "string") &&
    (u.theme === null || typeof u.theme === "string") &&
    // PRD #1167 "Lights on" m2: the four raw appearance overrides, each null-or-string
    // (the SEED provides them going forward; the v4 key bump discards any older blob).
    (u.appearance_mode === null || typeof u.appearance_mode === "string") &&
    (u.light_theme === null || typeof u.light_theme === "string") &&
    (u.dark_theme === null || typeof u.dark_theme === "string") &&
    (u.typeface === null || typeof u.typeface === "string") &&
    // Optional so a pre-#700 blob stays valid; absent/null reads as the default-ON state.
    (u.mr_rework_enabled === undefined ||
      u.mr_rework_enabled === null ||
      typeof u.mr_rework_enabled === "boolean") &&
    // Optional so a pre-feature blob stays valid; absent reads as default-only.
    (u.sidebar_token_ids === undefined ||
      (Array.isArray(u.sidebar_token_ids) &&
        u.sidebar_token_ids.every((id) => typeof id === "string")));
  const okApp =
    typeof a.autopilot_label === "string" &&
    // PRD #764: accept legacy blobs that predate this field (undefined) — it is filled
    // from the seed default ("uzi") on load — but reject a malformed non-string.
    (a.uzi_label === undefined || typeof a.uzi_label === "string") &&
    typeof a.default_theme === "string" &&
    // PRD #1167 "Lights on" m2: the four instance appearance defaults, each a string
    // (the SEED provides them; the v4 key bump discards any older blob that lacked them).
    typeof a.default_appearance_mode === "string" &&
    typeof a.default_light_theme === "string" &&
    typeof a.default_dark_theme === "string" &&
    typeof a.default_typeface === "string" &&
    // PRD #649: accept legacy blobs that predate this field (undefined), but reject a
    // malformed non-string so a bad localStorage blob can't violate the AppSettings contract.
    (a.ephemeral_workers_enabled === undefined || typeof a.ephemeral_workers_enabled === "string") &&
    typeof a.slack_enabled === "string" &&
    typeof a.public_base_url === "string" &&
    typeof a.judge_enabled === "string" &&
    typeof a.judge_model === "string" &&
    typeof a.judge_enforce_all === "string" &&
    typeof a.judge_cooldown_seconds === "string" &&
    typeof a.judge_daily_budget === "string" &&
    // Optional so a pre-#362 blob stays valid; a missing summary_model is filled
    // from the seed default ("haiku") on load.
    (a.summary_model === undefined || typeof a.summary_model === "string") &&
    typeof a.health_enabled === "string" &&
    typeof a.health_stall_seconds === "string" &&
    // PRD #1170 replaced health_slow_seconds with this key WITHOUT bumping the v3
    // storage key, so accept a legacy blob that predates it (undefined) — a missing
    // value is filled from the seed default ("85") on load — but reject a malformed
    // non-string. This preserves a developer's saved config instead of discarding the
    // whole blob and reseeding.
    (a.health_near_timeout_pct === undefined || typeof a.health_near_timeout_pct === "string") &&
    typeof a.health_queued_seconds === "string" &&
    typeof a.health_approval_seconds === "string" &&
    typeof a.health_nudge_cooldown_seconds === "string" &&
    typeof a.docker_repo_allowlist === "string";
  return okUser && okApp;
}

function loadSettings(): { userSettings: UserSettings; appSettings: AppSettings } {
  try {
    const raw = localStorage.getItem(MOCK_SETTINGS_KEY);
    if (raw) {
      const parsed: unknown = JSON.parse(raw);
      if (isPersistedSettings(parsed)) {
        // Merge over the seed so a pre-#362 blob (no summary_model on either
        // side) still yields a complete shape; the persisted values win where present.
        const appSettings = { ...SEED_APP_SETTINGS, ...parsed.appSettings };
        // PRD #1170: drop the retired health_slow_seconds key a pre-migration v3 blob
        // carries, so it doesn't ride along into the served settings / get re-persisted.
        delete (appSettings as Record<string, unknown>).health_slow_seconds;
        return {
          userSettings: { ...SEED_USER_SETTINGS, ...parsed.userSettings },
          appSettings,
        };
      }
    }
  } catch {
    // Storage unavailable (private mode) or a corrupt/legacy blob: re-seed.
  }
  return { userSettings: { ...SEED_USER_SETTINGS }, appSettings: { ...SEED_APP_SETTINGS } };
}

// persistSettings write-throughs the current settings maps. Called from the
// putMySettings / updateSettings mock handlers after they mutate.
function persistSettings(): void {
  try {
    const blob: PersistedSettings = { v: 1, userSettings, appSettings };
    localStorage.setItem(MOCK_SETTINGS_KEY, JSON.stringify(blob));
  } catch {
    // Storage unavailable: the demo still works in-memory for this session.
  }
}

const loadedSettings = loadSettings();

// ── Upstream release check (PRD #836) ────────────────────────────────────────
// Persisted remote facts from the last release poll, consistent with mockBuildInfo
// (running 0.4.2, latest v0.5.0, update_available). The body carries a couple of
// markdown bullets so the Updates card's plain-text notes excerpt renders, and NO
// `### Security` heading — the `security` derivation is therefore false, matching
// mockBuildInfo.latest.security. checkReleaseNow refreshes checked_at to simulate a
// successful re-check. The two toggles live in appSettings (release_check_*), so the
// derived status/enabled fields are read from there at build time — flipping a toggle
// in the card and re-reading getReleaseCheck stays coherent.
const releaseCheckFacts = {
  latest_tag: "v0.5.0",
  latest_name: "Hosted worker drain controls",
  body:
    "### Added\n" +
    "- Worker drain deadline controls on the fleet page (#812)\n" +
    "- Per-run cost roll-up in the board column footer (#799)\n\n" +
    "### Fixed\n" +
    "- Changelog drawer marker fixed on pre-release builds (#821)\n",
  notes_url: "https://github.com/vtmocanu/uzi/releases/tag/v0.5.0",
  published_at: daysAgo(3),
  checked_at: daysAgo(0),
};

// Mock server-side banner snooze tag (PRD #836 M6): the release tag the escalation
// banner was snoozed for, or null when never snoozed. `banner_snoozed` is DERIVED true
// iff this equals the current latest_tag, so snoozeReleaseBanner setting it to the
// latest_tag makes subsequent getReleaseCheck reads return banner_snoozed:true — and it
// would auto-clear if the fixture's latest_tag advanced, mirroring the server.
let releaseBannerSnoozeTag: string | null = null;

// releaseCheckStatus builds the admin DTO from the persisted facts + the live
// toggles. Master toggle off → status "disabled" and no derivation, mirroring the
// server. With facts present and the toggle on it reports "ok"; the derivations are
// consistent with mockBuildInfo (update available, not far-behind, not security).
function releaseCheckStatus(): ReleaseCheckStatus {
  const enabled = appSettings.release_check_enabled === "true";
  const bannerEnabled = appSettings.release_check_banner_enabled === "true";
  const running = mockBuildInfo.version; // "0.4.2", bare
  if (!enabled) {
    return {
      release_check_enabled: false,
      release_check_banner_enabled: bannerEnabled,
      interval: "6h",
      running_version: running,
      update_available: false,
      far_behind: false,
      security: false,
      banner_snoozed: false,
      status: "disabled",
    };
  }
  const security = /(^|\n)###\s+Security\b/i.test(releaseCheckFacts.body);
  return {
    release_check_enabled: true,
    release_check_banner_enabled: bannerEnabled,
    interval: "6h",
    running_version: running,
    latest_tag: releaseCheckFacts.latest_tag,
    latest_name: releaseCheckFacts.latest_name,
    body: releaseCheckFacts.body,
    notes_url: releaseCheckFacts.notes_url,
    published_at: releaseCheckFacts.published_at,
    checked_at: releaseCheckFacts.checked_at,
    update_available: true,
    // Demo choice (PRD #836 M6): far_behind is forced true so mock mode exercises the
    // escalation banner (surface 4). The real server derives this from the version gap +
    // age heuristic (D4) — a 0.4.2 → v0.5.0 delta would NOT trip it — but the mock has no
    // upstream to poll, so it hard-sets the state the banner needs to be demonstrable.
    far_behind: true,
    security,
    banner_snoozed:
      releaseBannerSnoozeTag !== null && releaseBannerSnoozeTag === releaseCheckFacts.latest_tag,
    status: "ok",
  };
}

let userSettings: UserSettings = loadedSettings.userSettings;

export let appSettings: AppSettings = loadedSettings.appSettings;
// PRD #685: whether a logo asset exists for each slot. The demo tracks only
// presence (a bool), never bytes — mirroring how the public /api/branding read
// exposes app_logo_present/brand_logo_present. Fresh install: neither uploaded.
const brandingAssets: { app: boolean; brand: boolean } = { app: false, brand: false };
// Slack secret tokens (PRD #25) are write-only: the demo tracks only whether one
// is configured, never a value, mirroring the real API's `secrets` map. There is
// no ENV overlay in the demo, so every key's source is db/default.
const slackSecrets: Record<string, boolean> = { slack_bot_token: false, slack_app_token: false };

// The current user's Slack linking state (PRD #25 M3). The demo starts unlinked;
// setting an override moves it to "pending" (a real deployment would then DM the
// target a Confirm card), and there is no inbound socket here to confirm it.
let slackLink: Omit<SlackLink, "state" | "workspace"> = { member_id: null, notify: true, resolved_id: null, confirmed: false };

// slackLinkResponse derives the state field the real API returns, so the mock and
// the server never disagree on how member_id/resolved_id/confirmed map to a state.
// workspace mirrors the admin slack_status: the demo has no real socket, so Slack
// is always "disabled" (see settingsResponse), which maps to "unconfigured" here.
function slackLinkResponse(): { slack: SlackLink } {
  const state: SlackLink["state"] = !slackLink.resolved_id
    ? "unlinked"
    : slackLink.confirmed
      ? "confirmed"
      : "pending";
  return { slack: { ...slackLink, state, workspace: "unconfigured" } };
}

// settingsResponse builds the admin SettingsResponse from the mock's current
// state: readable non-secret values, per-secret configured flags, and per-key
// sources (all db/default — the demo has no ENV overlay).
function settingsResponse(): SettingsResponse {
  const sources: Record<string, SettingSource> = {};
  for (const key of Object.keys(appSettings)) sources[key] = "db";
  for (const key of Object.keys(slackSecrets)) sources[key] = slackSecrets[key] ? "db" : "default";
  // The demo has no real socket, so Slack is always "disabled" here.
  return {
    settings: { ...appSettings },
    secrets: { ...slackSecrets },
    sources,
    slack_status: "disabled",
    oidc_status: oidcDemo().oidcStatus,
    oidc_provider_name: oidcDemo().providerName,
  };
}

// sessionBody is the auth/session bootstrap payload: the signed-in user, the
// current instance labels (PRD #19 M2, PRD #764: the single `uzi` label and the
// autopilot label), and the resolved `appearance` object (PRD #1167 "Lights on" m2),
// mirroring the real API so the mocked SPA resolves it the same way.
export function sessionBody() {
  // PRD #1167 "Lights on" m2: resolve the appearance from the raw user overrides
  // against the instance defaults, using the SAME chain (theme.resolveAppearance) the
  // server and the SPA use — a valid override wins, else the instance default, else the
  // compiled fallback, polarity-aware for the theme slots.
  const overrides = {
    mode: userSettings.appearance_mode,
    light: userSettings.light_theme,
    dark: userSettings.dark_theme,
    typeface: userSettings.typeface,
  };
  const defaults = {
    mode: appSettings.default_appearance_mode,
    light: appSettings.default_light_theme,
    dark: appSettings.default_dark_theme,
    typeface: appSettings.default_typeface,
  };
  const resolved = resolveAppearance(overrides, defaults);
  return {
    user: requireSession(),
    uzi_label: appSettings.uzi_label,
    autopilot_label: appSettings.autopilot_label,
    appearance: {
      mode: resolved.mode,
      light_theme: resolved.light,
      dark_theme: resolved.dark,
      typeface: resolved.typeface,
      overrides: {
        mode: userSettings.appearance_mode,
        light_theme: userSettings.light_theme,
        dark_theme: userSettings.dark_theme,
        typeface: userSettings.typeface,
      },
      defaults: {
        mode: appSettings.default_appearance_mode,
        light_theme: appSettings.default_light_theme,
        dark_theme: appSettings.default_dark_theme,
        typeface: appSettings.default_typeface,
      },
    },
    // Deprecated single-theme trio (PRD #21), derived from the resolved appearance so
    // the pre-m2 picker stays coherent: `theme` is the resolved slot for the resolved
    // mode (system falls to the dark slot), `theme_override` is the raw legacy override,
    // and `default_theme` is the legacy-chained instance dark default.
    theme: resolved.mode === "light" ? resolved.light : resolved.dark,
    theme_override: userSettings.theme,
    default_theme: resolveAppearance(
      {},
      { dark: appSettings.default_dark_theme || appSettings.default_theme },
    ).dark,
    // A passwordless (OIDC) demo user has no vault yet, so the SPA shows the
    // passphrase-create banner; a password demo user keeps the existing behavior.
    vault: oidcDemo().passwordless
      ? { unlocked: false, exists: false }
      : { unlocked: state.vaultUnlocked, exists: true },
    has_password: !oidcDemo().passwordless,
    // Judge consent (PRD #69 M4), resolved exactly like the server: enforced only when
    // BOTH the kill-switch and enforce-all are on (Gate-2-wins), and the effective model
    // is user-value-wins over the instance value, which falls back to opus.
    judge_enforced_by_admin: appSettings.judge_enabled === "true" && appSettings.judge_enforce_all === "true",
    effective_judge_model:
      (userSettings.judge_model ?? "").trim() !== ""
        ? (userSettings.judge_model as string)
        : appSettings.judge_model.trim() !== ""
          ? appSettings.judge_model
          : "opus",
  };
}

export const settingsApi = {
  // PRD #685: public branding read. Coerces the string-space settings to the typed
  // (bool) chrome shape and derives the two *_present flags from the tracked assets.
  branding: async () =>
    delay({
      app_logo_mode: appSettings.app_logo_mode,
      app_logo_preset: appSettings.app_logo_preset,
      app_logo_present: brandingAssets.app,
      app_logo_keep_name: appSettings.app_logo_keep_name === "true",
      brand_mode: appSettings.brand_mode,
      brand_company: appSettings.brand_company,
      brand_placement: appSettings.brand_placement,
      brand_plaque: appSettings.brand_plaque === "true",
      brand_logo_present: brandingAssets.brand,
    }),
  // Admin logo upload/delete. The demo enforces the same type allowlist and 256 KiB
  // cap the server does, then records presence (never bytes).
  uploadBrandingLogo: async (slot: string, file: File) => {
    requireSession();
    if (slot !== "app" && slot !== "brand") {
      throw new ApiError(404, "unknown logo slot");
    }
    const allowed = ["image/png", "image/webp", "image/svg+xml"];
    if (!allowed.includes(file.type)) {
      throw new ApiError(400, "logo must be a PNG, WebP or SVG image");
    }
    if (file.size > 262144) {
      throw new ApiError(400, "logo must be at most 262144 bytes");
    }
    brandingAssets[slot] = true;
    await delay(undefined, 40);
  },
  deleteBrandingLogo: async (slot: string) => {
    requireSession();
    if (slot !== "app" && slot !== "brand") {
      throw new ApiError(404, "unknown logo slot");
    }
    brandingAssets[slot] = false;
    return delay({ status: "ok" });
  },
  // ── Admin: instance settings (PRD #19) ───────────────────────────────────────
  // Mirrors the server's Decision 8 validation so the demo surfaces the same
  // rejection messages the real API would.
  getSettings: async () => delay(settingsResponse()),
  // Demo is fully DEK-sealed (no legacy rows), so the admin migration notice is
  // hidden; the wiring is still exercised by the AdminSettings unit test.
  vaultMigration: async () => delay({ master_sealed: 0 }),
  // ── Upstream release check (PRD #836 M5) ─────────────────────────────────────
  // Admin read (RequireAdminRO): the full release-check status backing the Updates
  // card, derived from the persisted facts + the live toggles.
  getReleaseCheck: async () => {
    requireSession();
    return delay({ release_check: releaseCheckStatus() });
  },
  // "Check now" (RequireAdmin): simulate a successful re-check by refreshing the
  // checked-at stamp, then return the refreshed status.
  checkReleaseNow: async () => {
    requireSession();
    releaseCheckFacts.checked_at = new Date().toISOString();
    return delay({ release_check: releaseCheckStatus() });
  },
  // Snooze the escalation banner (RequireAdmin, PRD #836 M6): set the snooze tag to the
  // current latest_tag so subsequent getReleaseCheck reads report banner_snoozed:true,
  // then return the refreshed status — mirroring the server's tag-keyed upsert.
  snoozeReleaseBanner: async () => {
    requireSession();
    releaseBannerSnoozeTag = releaseCheckFacts.latest_tag;
    return delay({ release_check: releaseCheckStatus() });
  },

  updateSettings: async (updates: UpdateSettingsPayload) => {
    // Secret tokens are write-only: validated + recorded as configured, never
    // merged into the readable settings (mirrors the real structural exclusion).
    const nonSecret: Partial<AppSettings> = {};
    for (const [key, raw] of Object.entries(updates)) {
      const value = raw ?? "";
      if (key === "slack_bot_token" || key === "slack_app_token") {
        const prefix = key === "slack_bot_token" ? "xoxb-" : "xapp-";
        if (!value.startsWith(prefix)) {
          throw new ApiError(400, `${key}: token must start with ${prefix}`);
        }
        slackSecrets[key] = true;
        continue;
      }
      // Agent-source config (PRD #602 M5) routes to the dedicated agent-source state,
      // not the readable settings — the card reads it back through getAgentSource.
      if (key === "agent_source_credential") {
        // Write-only secret: an empty value leaves it unchanged; any value marks it
        // configured. Never merged into the readable settings.
        if (value.trim() !== "") agentSource.config.credential_configured = true;
        continue;
      }
      if (key === "agent_source_repo_url") {
        const url = value.trim();
        if (url !== "") {
          // Mirror the server's write-time SSRF-allowlist gate (and its https-only rule).
          let host = "";
          try {
            const parsed = new URL(url);
            if (parsed.protocol !== "https:") throw new Error("scheme");
            host = parsed.hostname;
          } catch {
            throw new ApiError(400, "agent_source_repo_url: must be an https URL");
          }
          if (!mockAllowedAgentSourceHosts.includes(host)) {
            throw new ApiError(400, "URL is not in the agent-source allowlist");
          }
        }
        agentSource.config.url = url;
        if (url === "") {
          // Deconfiguring the source clears its sync history and any staged snapshot
          // — there is nothing left to sync from or review, so the card returns to
          // its clean "never synced / nothing staged" fresh-install shape.
          agentSource.config.enabled = false;
          agentSource.status = {};
          agentSource.staged = null;
        } else if (agentSource.staged) {
          // Keep the staged snapshot's source_url coherent with the configured URL.
          agentSource.staged.source_url = url;
        }
        continue;
      }
      if (key === "agent_source_ref") {
        agentSource.config.ref = value.trim();
        if (agentSource.staged) agentSource.staged.source_ref = value.trim();
        continue;
      }
      if (key === "agent_source_folder") {
        // PRD #702 M1: repo-relative subfolder. Empty resolves to the default
        // (".claude/agents") server-side; a configured value normalizes away a
        // single trailing slash, mirroring the Cache reader.
        const folder = value.trim();
        agentSource.config.folder = folder === "" ? ".claude/agents" : folder.replace(/\/$/, "");
        continue;
      }
      if (key === "agent_source_enabled") {
        if (value !== "true" && value !== "false") {
          throw new ApiError(400, "agent_source_enabled: must be \"true\" or \"false\"");
        }
        agentSource.config.enabled = value === "true";
        continue;
      }
      if (key === "agent_source_interval") {
        const v = value.trim();
        if (v !== "" && !/^\d+(ns|us|µs|ms|s|m|h)$/.test(v)) {
          throw new ApiError(400, "agent_source_interval: must be a Go duration (e.g. 1h)");
        }
        agentSource.config.interval = v || "1h";
        continue;
      }
      // default_theme routes to the theme registry, not the label rules (PRD #21).
      if (key === "default_theme") {
        if (!isTheme(value)) throw new ApiError(400, `default_theme: unknown theme: "${value}"`);
        nonSecret.default_theme = value;
        continue;
      }
      // PRD #1167 "Lights on" m2: the four instance appearance defaults route to the
      // theme registry like default_theme, polarity-aware for the two theme slots.
      if (key === "default_appearance_mode") {
        if (!isAppearanceMode(value)) {
          throw new ApiError(400, "default_appearance_mode: must be one of system, light, dark");
        }
        nonSecret.default_appearance_mode = value;
        continue;
      }
      if (key === "default_light_theme") {
        if (!isLightTheme(value)) {
          throw new ApiError(400, "default_light_theme: must be a light theme");
        }
        nonSecret.default_light_theme = value;
        continue;
      }
      if (key === "default_dark_theme") {
        if (!isDarkTheme(value)) {
          throw new ApiError(400, "default_dark_theme: must be a dark theme");
        }
        nonSecret.default_dark_theme = value;
        continue;
      }
      if (key === "default_typeface") {
        if (!isTypeface(value)) {
          throw new ApiError(400, "default_typeface: must be one of system, plex");
        }
        nonSecret.default_typeface = value;
        continue;
      }
      // slack_enabled / judge_enabled / … are strict bools, not labels — without this
      // arm they would fall through to the label rules and fail open on "yes"/"maybe".
      if (
        key === "slack_enabled" ||
        key === "judge_enabled" ||
        key === "judge_enforce_all" ||
        key === "ephemeral_workers_enabled" ||
        key === "release_check_enabled" ||
        key === "release_check_banner_enabled" ||
        key === "health_enabled" ||
        key === "capability_aware_scheduling" ||
        key === "github_project_sync_enabled"
      ) {
        if (value !== "true" && value !== "false") {
          throw new ApiError(400, `${key}: must be "true" or "false"`);
        }
        (nonSecret as Record<string, string>)[key] = value;
        continue;
      }
      // judge_cooldown_seconds: 0 (off) or 60..86400 seconds; judge_daily_budget: 0
      // (unlimited) or 1..10000 runs (PRD #69 M5, mirroring the server validators).
      if (key === "judge_cooldown_seconds") {
        if (!/^\d+$/.test(value)) throw new ApiError(400, "judge_cooldown_seconds: must be a whole number");
        const n = Number(value);
        if (n !== 0 && (n < 60 || n > 86400)) {
          throw new ApiError(400, "judge_cooldown_seconds: must be 0 (off) or between 60 and 86400");
        }
        nonSecret.judge_cooldown_seconds = String(n);
        continue;
      }
      if (key === "judge_daily_budget") {
        if (!/^\d+$/.test(value)) throw new ApiError(400, "judge_daily_budget: must be a whole number");
        const n = Number(value);
        if (n < 0 || n > 10000) {
          throw new ApiError(400, "judge_daily_budget: must be 0 (unlimited) or between 1 and 10000 judge runs");
        }
        nonSecret.judge_daily_budget = String(n);
        continue;
      }
      // judge_model (PRD #46) and summary_model (PRD #362) are model aliases: non-empty
      // single token, mirroring the server's PRD #17 ValidateModel rules.
      if (key === "judge_model" || key === "summary_model") {
        if (value.trim() === "") throw new ApiError(400, `${key}: must not be empty`);
        if (/\s/.test(value)) throw new ApiError(400, `${key}: must be a single token with no spaces`);
        (nonSecret as Record<string, string>)[key] = value;
        continue;
      }
      // public_base_url must be http(s) (PRD #25).
      if (key === "public_base_url") {
        if (!/^https?:\/\/.+/.test(value)) {
          throw new ApiError(400, "public_base_url: must use http or https");
        }
        nonSecret.public_base_url = value;
        continue;
      }
      // PRD #685 branding config. Enum keys are checked against their allowed sets;
      // the two flags are strict bools; brand_company is free text (may be "", may
      // contain commas — mirrors the server's dedicated termsafe validator), capped
      // at 64 runes.
      if (key === "app_logo_mode") {
        if (value !== "default" && value !== "custom" && value !== "preset") {
          throw new ApiError(400, 'app_logo_mode: must be "default", "custom" or "preset"');
        }
        nonSecret.app_logo_mode = value;
        continue;
      }
      // app_logo_preset (PRD #780): a web-catalog slug. Mirrors the server's
      // validateBrandingSlug SHAPE gate — empty is allowed ("no preset"), any other
      // value must be a short lowercase slug. Membership against the catalog is NOT
      // checked here (the web catalog is the source of truth; unknown slugs degrade).
      if (key === "app_logo_preset") {
        if (value !== "" && !/^[a-z][a-z0-9-]{0,31}$/.test(value)) {
          throw new ApiError(400, "app_logo_preset: must be a short lowercase slug (a-z, 0-9, hyphen; 32 chars max)");
        }
        nonSecret.app_logo_preset = value;
        continue;
      }
      if (key === "brand_mode") {
        if (value !== "none" && value !== "text" && value !== "logo") {
          throw new ApiError(400, 'brand_mode: must be "none", "text" or "logo"');
        }
        nonSecret.brand_mode = value;
        continue;
      }
      if (key === "brand_placement") {
        if (value !== "below" && value !== "topright") {
          throw new ApiError(400, 'brand_placement: must be "below" or "topright"');
        }
        nonSecret.brand_placement = value;
        continue;
      }
      if (key === "app_logo_keep_name" || key === "brand_plaque") {
        if (value !== "true" && value !== "false") {
          throw new ApiError(400, `${key}: must be "true" or "false"`);
        }
        (nonSecret as Record<string, string>)[key] = value;
        continue;
      }
      if (key === "brand_company") {
        if ([...value].length > 64) {
          throw new ApiError(400, "brand_company: must be at most 64 characters");
        }
        nonSecret.brand_company = value;
        continue;
      }
      // Run-health detector thresholds (PRD #47), mirroring the server's
      // validateHealthSeconds: a whole number of seconds, accept 0 (off), otherwise the
      // inclusive range [60, 86400].
      if (
        key === "health_stall_seconds" ||
        key === "health_queued_seconds" ||
        key === "health_approval_seconds" ||
        key === "health_nudge_cooldown_seconds"
      ) {
        if (!/^\d+$/.test(value)) {
          throw new ApiError(400, `${key}: must be a whole number of seconds`);
        }
        const n = Number(value);
        if (n !== 0 && (n < 60 || n > 86400)) {
          throw new ApiError(400, `${key}: must be 0 (off) or between 60 and 86400`);
        }
        (nonSecret as Record<string, string>)[key] = String(n);
        continue;
      }
      // Near-timeout percent (PRD #1170), mirroring the server's validateHealthPercent:
      // a whole number that is 0 (disable) or in the inclusive range [50, 99]. 100 is
      // rejected because the sweeper fires the timeout at 100%.
      if (key === "health_near_timeout_pct") {
        if (!/^-?\d+$/.test(value)) {
          throw new ApiError(400, `${key}: must be 0 (disabled) or between 50 and 99 percent`);
        }
        const n = Number(value);
        if (n !== 0 && (n < 50 || n > 99)) {
          throw new ApiError(400, `${key}: must be 0 (disabled) or between 50 and 99 percent`);
        }
        (nonSecret as Record<string, string>)[key] = String(n);
        continue;
      }
      // docker_repo_allowlist (PRD #957): a comma-separated list of repo ids, mirroring the
      // server's validateRepoAllowlist. Empty is allowed (fail-closed empty); every
      // non-empty token must be a valid UUID. The raw (untrimmed) value is stored.
      if (key === "docker_repo_allowlist") {
        const ok = value
          .split(",")
          .map((t) => t.trim())
          .filter((t) => t !== "")
          .every((t) =>
            /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(t),
          );
        if (!ok) {
          throw new ApiError(
            400,
            "docker_repo_allowlist: must be a comma-separated list of repo ids (UUIDs)",
          );
        }
        nonSecret.docker_repo_allowlist = value;
        continue;
      }
      if (key !== "autopilot_label" && key !== "uzi_label") {
        throw new ApiError(400, `unknown setting: ${key}`);
      }
      if (!value || value.trim() === "") throw new ApiError(400, `${key}: must not be empty`);
      if (value.length > 64) throw new ApiError(400, `${key}: must be at most 64 characters`);
      if (value.includes(",")) throw new ApiError(400, `${key}: must not contain a comma`);
      (nonSecret as Record<string, string>)[key] = value;
    }
    const merged = { ...appSettings, ...nonSecret };
    // The `uzi` and autopilot labels must differ (PRD #764), mirroring the server's
    // ValidateMerged.
    if (merged.uzi_label === merged.autopilot_label) {
      throw new ApiError(400, "uzi_label and autopilot_label must differ");
    }
    appSettings = merged;
    persistSettings();
    return delay(settingsResponse());
  },
  // ── Autopilot opt-in (PRD #19 M3) ────────────────────────────────────────────
  setAutopilotEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.autopilot_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },

  // ── Usage-limit default (PRD #35 M3) ─────────────────────────────────────────
  // 🔴 TOUCHES THE USER ROW ONLY. It must not walk `state.runs` "helpfully" applying
  // the new default: the flag is copied onto a run at CREATION, so a sweep would
  // silently undo every per-run override the user had made — including on the run
  // they are looking at. The demo has to teach that these are two separate controls,
  // because that is the thing about this feature people get wrong.
  setWaitOnLimit: async (enabled: boolean) => {
    const u = requireSession();
    u.wait_on_limit = enabled;
    return delay({ user: { ...u } }, 200);
  },

  // ── Early-limit-reset Slack alert opt-in (PRD #1020 M4/M5) ────────────────────
  // Own-user (session identity, never a body id, mirroring the server). Default true.
  setNotifyEarlyReset: async (enabled: boolean) => {
    const u = requireSession();
    u.notify_early_limit_reset = enabled;
    return delay({ user: { ...u } }, 200);
  },

  // ── Run-judge opt-in (PRD #46) ───────────────────────────────────────────────
  // Own-user (session identity, never a body id, mirroring the server's audit H3).
  setJudgeEnabled: async (
    enabled: boolean,
    anthropicToken?: string | null,
    judgeBindMode?: BindMode,
  ) => {
    const u = requireSession();
    u.judge_enabled = enabled;
    // Three-way, like the server: undefined leaves the binding, null clears it, a
    // label binds it (400 when it names nothing).
    if (anthropicToken !== undefined) {
      if (anthropicToken === null || anthropicToken.trim() === "") {
        u.judge_anthropic_secret_id = null;
        u.judge_anthropic_secret_label = null;
      } else {
        const secret = secrets.find(
          (x) =>
            x.kind === "anthropic_token" &&
            x.label.toLowerCase() === anthropicToken.trim().toLowerCase(),
        );
        if (!secret) throw new ApiError(400, "no Anthropic token with that label");
        u.judge_anthropic_secret_id = secret.id;
        u.judge_anthropic_secret_label = secret.label;
      }
    }
    // PRD #1140 M3: the three-valued bind mode rides along when provided, so the
    // mock reflects the effective mode the picker drives (auto / default / pinned).
    if (judgeBindMode !== undefined) {
      u.judge_anthropic_bind_mode = judgeBindMode;
    }
    return delay({ user: { ...u } }, 200);
  },
  // Admin per-user toggle: target from the id argument (the path on the server).
  setUserJudgeEnabled: async (id: string, enabled: boolean) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    u.judge_enabled = enabled;
    return delay({ user: { ...u } });
  },

  // ── CI-autofix opt-in (PRD #71) ──────────────────────────────────────────────
  // Own-user (session identity, never a body id, mirroring the server).
  setCIAutofixEnabled: async (enabled: boolean | null) => {
    const u = requireSession();
    u.ci_autofix_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },
  // Admin per-user toggle: target from the id argument (the path on the server).
  setUserCIAutofixEnabled: async (id: string, enabled: boolean | null) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    u.ci_autofix_enabled = enabled;
    return delay({ user: { ...u } });
  },

  // ── AI-attribution opt-out (issue #916) ──────────────────────────────────────
  // Own-user (session identity, never a body id, mirroring the server). Default true.
  setAttributionEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.attribution_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },

  // ── Ephemeral-workers opt-in (PRD #649) ──────────────────────────────────────
  // Own-user (session identity, never a body id, mirroring the server).
  setEphemeralWorkersEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.ephemeral_workers_enabled = enabled;
    // Persist on the backing users[] record too, not just the session copy, so the
    // opt-in survives a mock logout/login (login() re-copies from users[]).
    const stored = users.find((x) => x.id === u.id);
    if (stored) stored.ephemeral_workers_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },
  getMySettings: async () => delay({ settings: { ...userSettings } }),
  putMySettings: async (patch: UserSettingsPatch) => {
    // PATCH-like: apply only the fields present in the body, mirroring the real
    // handler so a theme-only save never clears the model and vice versa. Staged
    // into a LOCAL copy first and committed only after every field validates, so a
    // body whose first field is valid and second is not (e.g. appearance_mode:"light"
    // + light_theme:"ember") returns 400 without half-applying — matching the real
    // handler's validate-all-then-write ordering.
    let next = { ...userSettings };
    if (patch.default_model !== undefined) {
      const trimmed = patch.default_model?.trim() ?? "";
      next = { ...next, default_model: trimmed === "" ? null : trimmed };
    }
    if (patch.default_effort !== undefined) {
      // Closed enum (PRD #617 M5): blank clears to inherit; any other value must be one
      // of the five SDK levels, mirroring the server's ValidateEffort.
      const trimmed = patch.default_effort?.trim() ?? "";
      if (trimmed !== "" && !["low", "medium", "high", "xhigh", "max"].includes(trimmed)) {
        throw new ApiError(400, "effort must be one of low, medium, high, xhigh, max");
      }
      next = { ...next, default_effort: trimmed === "" ? null : trimmed };
    }
    if (patch.judge_model !== undefined) {
      // Same rules as default_model (PRD #69 M2): blank clears to inherit, a value with
      // internal whitespace is rejected, mirroring the server's ValidateModel.
      const trimmed = patch.judge_model?.trim() ?? "";
      if (trimmed !== "" && /\s/.test(trimmed)) {
        throw new ApiError(400, "judge_model must be a single token with no spaces");
      }
      next = { ...next, judge_model: trimmed === "" ? null : trimmed };
    }
    if (patch.summary_model !== undefined) {
      // Same rules as judge_model (PRD #362 M2): blank clears to inherit, a value with
      // internal whitespace is rejected, mirroring the server's ValidateModel.
      const trimmed = patch.summary_model?.trim() ?? "";
      if (trimmed !== "" && /\s/.test(trimmed)) {
        throw new ApiError(400, "summary_model must be a single token with no spaces");
      }
      next = { ...next, summary_model: trimmed === "" ? null : trimmed };
    }
    if (patch.theme !== undefined) {
      const t = patch.theme?.trim() ?? "";
      if (t !== "" && !isTheme(t)) throw new ApiError(400, `unknown theme: "${t}"`);
      // PRD #1167 "Lights on" m2: a valid legacy theme value also FOLDS into the
      // appearance columns — it sets the matching-polarity slot AND pins appearance_mode
      // to that polarity, mirroring the Go legacy mapping. Any explicit appearance_* field
      // in the same PUT wins, because those are applied AFTER this block below. An empty
      // value clears the legacy override AND the migrated appearance override (mode +
      // dark slot), mirroring the server's legacy-reset coherence.
      if (t === "") {
        next = { ...next, theme: null, appearance_mode: null, dark_theme: null };
      } else {
        const polarity = THEME_POLARITY[t as Theme];
        next = {
          ...next,
          theme: t,
          appearance_mode: polarity,
          ...(polarity === "light" ? { light_theme: t } : { dark_theme: t }),
        };
      }
    }
    // PRD #1167 "Lights on" m2: the four raw appearance overrides. Tri-state like the rest
    // of the patch — absent leaves the field, present-null clears it back to inherit, and a
    // value is validated (polarity-aware for the theme slots) then set. Applied AFTER the
    // legacy theme fold above, so an explicit field wins when both are sent in one PUT.
    if (patch.appearance_mode !== undefined) {
      if (patch.appearance_mode !== null && !isAppearanceMode(patch.appearance_mode)) {
        throw new ApiError(400, "appearance_mode: must be one of system, light, dark");
      }
      next = { ...next, appearance_mode: patch.appearance_mode };
    }
    if (patch.light_theme !== undefined) {
      if (patch.light_theme !== null && !isLightTheme(patch.light_theme)) {
        throw new ApiError(400, "light_theme: must be a light theme");
      }
      next = { ...next, light_theme: patch.light_theme };
    }
    if (patch.dark_theme !== undefined) {
      if (patch.dark_theme !== null && !isDarkTheme(patch.dark_theme)) {
        throw new ApiError(400, "dark_theme: must be a dark theme");
      }
      next = { ...next, dark_theme: patch.dark_theme };
    }
    if (patch.typeface !== undefined) {
      if (patch.typeface !== null && !isTypeface(patch.typeface)) {
        throw new ApiError(400, "typeface: must be one of system, plex");
      }
      next = { ...next, typeface: patch.typeface };
    }
    if (patch.sidebar_token_ids !== undefined) {
      // Whole-set replace, mirroring how the real handler would treat a list
      // value; null clears back to default-only. Ids are stored as given — a
      // stale id (deleted token) is harmless, it just matches nothing.
      next = { ...next, sidebar_token_ids: patch.sidebar_token_ids ?? [] };
    }
    if (patch.mr_rework_enabled !== undefined) {
      // Tri-state (PRD #700 M6): present-false opts out, present-true re-enables,
      // present-null clears back to the default-ON state (stored as null). Mirrors
      // the server treating an absent/null value as ON.
      next = { ...next, mr_rework_enabled: patch.mr_rework_enabled ?? null };
    }
    // Every field validated: commit the staged copy in one shot, then persist.
    userSettings = next;
    persistSettings();
    return delay({ settings: { ...userSettings } });
  },
  // ── Slack linking (PRD #25 M3) ───────────────────────────────────────────────
  getMySlack: async () => delay(slackLinkResponse()),
  setMySlackNotify: async (notify: boolean) => {
    slackLink = { ...slackLink, notify };
    return delay(slackLinkResponse());
  },
  setMySlackOverride: async (memberId: string | null) => {
    const member = memberId?.trim() ?? "";
    if (member === "") {
      // Clear the override: fall back to email auto-match (nothing resolved here).
      slackLink = { ...slackLink, member_id: null, resolved_id: null, confirmed: false };
    } else {
      if (!/^[A-Za-z0-9]{1,64}$/.test(member)) throw new ApiError(400, "invalid Slack member ID");
      // A set resets confirmation: the target must Confirm before content flows.
      slackLink = { ...slackLink, member_id: member, resolved_id: member, confirmed: false };
    }
    return delay(slackLinkResponse());
  },
  testMySlackDM: async () => {
    if (!slackLink.resolved_id) throw new ApiError(400, "no linked Slack account to send a test DM to");
    return delay({ status: "sent" });
  },
  getSlackStatus: async () => delay({ slack_status: "disabled" }),
};
