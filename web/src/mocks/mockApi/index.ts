// The in-browser mock implementation of the API client. Same surface, same
// response shapes, zero network: every method resolves from the in-memory store
// after a small jittered delay (so loading states render believably). Board
// moves, template CRUD, worker tokens, run inputs — all work locally.

import {
  type AutoStatus,
  type BindMode,
  type AgentSelectionInput,
  type AgentSourceApplyResult,
  type AgentSourceView,
  type AppSettings,
  type Board,
  type BoardPrefs,
  type Chat,
  type Card,
  type CreatedIssue,
  type IssueDraft,
  type IncidentalFinding,
  type IncidentalFindingBacklog,
  type IncidentalFindingBucket,
  type IncidentalFindingFileResult,
  type IncidentalFindingIssueDraft,
  type ReleaseCheckStatus,
  type Run,
  type RunPriority,
  type CatalogEntry,
  type Schedule,
  type ScheduleInput,
  type SchedulePreviewInput,
  type RunMessage,
  type SettingSource,
  type SettingsResponse,
  type SlackLink,
  type UpdateSettingsPayload,
  type RunInputKind,
  type SecretMeta,
  type UserSettings,
  type UserSettingsPatch,
} from "../../lib/api";
// ApiError / isTerminalRun are imported from their own leaf modules (not the
// `../lib/api` barrel) so this mock-mode client introduces no runtime import
// edge back to lib/api.ts — the api → mockApi → api cycle behind issue #165.
import { ApiError } from "../../lib/apiError";
import { isTerminalRun } from "../../lib/runStatus";
import { isTheme, resolveTheme } from "../../lib/theme";
import { recommendationLabel, verdictLabel } from "../../lib/judge";
import {
  LIVE_RUN_ID,
  mockAdmin,
  mockAdminRateLimits,
  mockAdminWorkers,
  mockBuildInfo,
  daysAgo,
  mockMyRateLimitsByUser,
  mockMyTokenRateLimits,
  mockAgentSource,
  mockFindings,
  type MockFinding,
  mockOtherRunOwners,
  mockRepos,
  type MockReview,
  mockRunInputs,
  mockSecrets,
  mockWorkers,
  runListItem,
} from "../data";
import { ensureLive, handleInput, scheduleChatReply, startNewRun } from "../engine";
import { appendMessage, getProposal, getRun, nextRunId, patchRun, putProposal, state } from "../store";
import { delay, oidcDemo, requireSession, users } from "./shared";
import { agentsApi, LEAD_NAME_RE, templates } from "./agents";
import { cliTokensApi } from "./cliTokens";
import { forgeApi, repos } from "./forge";
import { judgeApi, reviews } from "./judge";
import { memoryApi } from "./memory";
import { notificationsApi } from "./notifications";

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
const MOCK_SETTINGS_KEY = "uzi.mock.v3";
const SEED_USER_SETTINGS: UserSettings = {
  default_model: null,
  default_effort: null,
  judge_model: null,
  summary_model: null,
  theme: null,
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
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  // PRD #69: enforced mode off, spend guards at their server defaults (cooldown 60s,
  // budget 0 = unlimited).
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
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
  health_slow_seconds: "2700",
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
    typeof a.health_slow_seconds === "string" &&
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
        return {
          // Merge over the seed so a pre-#362 blob (no summary_model on either
          // side) still yields a complete shape; the persisted values win where present.
          userSettings: { ...SEED_USER_SETTINGS, ...parsed.userSettings },
          appSettings: { ...SEED_APP_SETTINGS, ...parsed.appSettings },
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

// Incidental-findings coordinates (PRD #333 M7). Mutable copy so file/dismiss persist in a
// demo session; the seed stays pristine so a module reload re-seeds a clean backlog.
const findings: MockFinding[] = mockFindings.map((f) => ({ ...f, labels: [...f.labels], run_ids: [...f.run_ids] }));

// findingDTO projects a mock coordinate to the wire DTO, omitting the optional keys exactly as
// the server's `omitempty` tags do (a null finding_id / iid / resolved_at is simply absent).
function findingDTO(f: MockFinding): IncidentalFinding {
  return {
    ...(f.finding_id ? { finding_id: f.finding_id } : {}),
    location: f.location,
    repo_id: f.repo_id,
    repo_path: f.repo_path,
    status: f.status,
    last_title: f.last_title,
    seen_in_runs: f.seen_in_runs,
    ...(f.filed_issue_iid != null ? { filed_issue_iid: f.filed_issue_iid } : {}),
    ...(f.filed_issue_url ? { filed_issue_url: f.filed_issue_url } : {}),
    ...(f.resolved_at ? { resolved_at: f.resolved_at } : {}),
  };
}

// matchFindingBucket maps a disposition status to the ?bucket= filter (D7): to_file shows only
// open, filed/dismissed show their own status, all shows everything (the transient `filing` is
// invisible to to_file, exactly like the server).
function matchFindingBucket(status: MockFinding["status"], bucket: IncidentalFindingBucket): boolean {
  switch (bucket) {
    case "to_file":
      return status === "open";
    case "filed":
      return status === "filed";
    case "dismissed":
      return status === "dismissed";
    case "all":
      return true;
    default:
      return false;
  }
}

// Monotonic iid for issues the preview files (PRD #68), above the seeded #71.
let nextFiledIssueIid = 90;

// mockIssueDraft mirrors the server's deterministic templating (PRD #68 M2): the
// category→repo default resolved against the connected repos (an empty default → mock
// state D), the fenced body, the server-side `uzi` label (PRD #764), and a provenance
// line. Faithful enough for the preview to render every state, not a byte-for-byte copy
// of the Go renderer (its fence/strip/scan is unit-tested there).
function mockIssueDraft(
  runId: string,
  rec: MockReview["recommendations"][number],
  review: MockReview,
): IssueDraft {
  const label = recommendationLabel(rec.category);
  const enabledRepoIds = new Set(repos.filter((r) => r.enabled).map((r) => r.id));
  let default_repo_id = "";
  let default_note = "";
  if (rec.category === "improve_agent" || rec.category === "add_agent") {
    const rid = getRun(runId)?.repo_id ?? "";
    if (enabledRepoIds.has(rid)) {
      default_repo_id = rid;
      default_note =
        "Defaulted to the judged run's repo — repo agents live in its .claude/agents/. Pick any repo you have connected.";
    } else {
      default_note = "The judged run's repo isn't one you've connected. Pick the repo to file this against.";
    }
  } else {
    // PRD #590 M2: the uzi-own-repo default now comes from the caller's enabled
    // self_improve default schedule (server-side); the preview does not model that
    // schedule, so it renders mock state D (no default, pick a repo).
    default_note =
      "No uzi repo is configured on this instance (or it isn't one you've connected), so there's no default. Pick the repo to file this against.";
  }
  const description = [
    "## What the judge found",
    "",
    "````",
    rec.rationale_md,
    "````",
    "",
    "## Context",
    "",
    `- Recommendation: **${label}**${rec.target ? " — `" + rec.target + "`" : ""}${
      rec.confidence ? ` (${rec.confidence} confidence)` : ""
    }`,
    `- Verdict on the judged run: **${verdictLabel(review.verdict)}**`,
    "",
    "## Judge's summary of the run",
    "",
    "````",
    review.summary_md,
    "````",
    "",
    "---",
    "Opened by uzi on behalf of @vlad, from a run retrospective. The quoted text above is LLM-authored and unverified.",
  ].join("\n");
  return {
    default_repo_id,
    title: rec.target ? `${label}: ${rec.target}` : label,
    description,
    labels: ["uzi"],
    provenance: `from vlad's worker, run ${runId.slice(0, 8)}`,
    default_note,
  };
}
// Agent-source demo state (PRD #602 M5): a deep clone so mutations (config save,
// sync, apply) do not leak back into the shared fixture. The config half is edited
// through updateSettings' agent_source_* arm; the status/staged halves by sync/apply.
let agentSource: AgentSourceView = structuredClone(mockAgentSource);

// Persisted REMOTE facts from the last update check (PRD #702 M4), mirroring the
// server's persist-facts/derive-at-read split: the update-check writes these, and
// getAgentSource DERIVES update_available/latest_ref from them + the LIVE config at
// read time — so a pin bump or apply self-clears the badge with no new egress. Null
// until a check has run, so a fresh install shows no badge.
let agentSourceRemote: { latestRef: string; tipSha: string; checkedAt: string } | null = null;

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

// parseAgentSourceSemver mirrors the server's re-prefix + IsValid guard (Decision 4):
// a non-`v`-prefixed or malformed ref is not a comparable semver (null), never treated
// as equal. Returns the [major, minor, patch] triple for a valid vX.Y.Z, else null.
function parseAgentSourceSemver(ref: string): [number, number, number] | null {
  const m = /^v?(\d+)\.(\d+)\.(\d+)(?:[-+].*)?$/.exec(ref.trim());
  if (!m) return null;
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}

// compareAgentSourceSemver returns >0 when a is newer than b (numeric per-component,
// so v1.10.0 sorts ABOVE v1.2.0 — a lexical compare gets this wrong, the discriminating
// case Decision 4 calls out).
function compareAgentSourceSemver(a: [number, number, number], b: [number, number, number]): number {
  for (let i = 0; i < 3; i++) {
    if (a[i] !== b[i]) return a[i] - b[i];
  }
  return 0;
}

// deriveAgentSourceStatus computes the update-available fields at read time from the
// stored remote facts + the live config (Decision 6), mirroring the server:
//   - no check has run (remote null) → no update fields (no badge);
//   - 40-hex SHA pin → never an update;
//   - tag-pinned (valid semver ref) → update iff the newest remote semver tag is strictly
//     greater, naming it in latest_ref;
//   - branch-pinned/empty ref → update ("moved", latest_ref empty) iff the remote tip
//     differs from last_applied_sha.
function deriveAgentSourceStatus(): AgentSourceView["status"] {
  const base = agentSource.status;
  const remote = agentSourceRemote;
  if (!remote) return base;
  const ref = agentSource.config.ref.trim();
  let updateAvailable = false;
  let latestRef = "";
  if (/^[0-9a-f]{40}$/i.test(ref)) {
    // SHA-pinned: an immutable pin is intentionally frozen — no signal.
    updateAvailable = false;
  } else {
    const pinned = parseAgentSourceSemver(ref);
    if (pinned) {
      const cand = parseAgentSourceSemver(remote.latestRef);
      if (cand && compareAgentSourceSemver(cand, pinned) > 0) {
        updateAvailable = true;
        latestRef = remote.latestRef;
      }
    } else if (
      remote.tipSha &&
      remote.tipSha.toLowerCase() !== (base.last_applied_sha ?? "").toLowerCase()
    ) {
      // branch-pinned/empty: the advertised tip moved past what is applied. Compared
      // case-INSENSITIVELY to mirror the server's DeriveUpdate (strings.EqualFold), so a
      // mixed-case fixture can't drift the mock from the real derive.
      updateAvailable = true;
    }
  }
  return {
    ...base,
    update_available: updateAvailable,
    latest_ref: latestRef || undefined,
    update_checked_at: remote.checkedAt,
  };
}

// agentSourceView returns the current view with the update-available fields DERIVED at
// read time (Decision 6) — every read path returns this, so the badge is consistent.
function agentSourceView(): AgentSourceView {
  const clone = structuredClone(agentSource);
  clone.status = deriveAgentSourceStatus();
  return clone;
}

// mockAllowedAgentSourceHosts mirrors the server's SSRF allowlist: a save of a URL
// whose host is not on it is rejected with the same shaped 400 the real
// UpdateSettings surfaces. Empty URL (disable) is always allowed.
const mockAllowedAgentSourceHosts = ["github.com", "gitlab.com"];
let secrets: SecretMeta[] = mockSecrets.map((s) => ({ ...s }));

// requireUnlockedVault mirrors the real API: sealing a token needs the vault
// unlocked (PRD #32), so every create/rotate path throws the same 409 the SPA
// turns into an unlock prompt.
function requireUnlockedVault(): void {
  if (!state.vaultUnlocked) {
    throw new ApiError(409, "vault is locked; unlock it with your password, then save again", {
      code: "vault_locked",
    });
  }
}
let userSettings: UserSettings = loadedSettings.userSettings;
let workers = mockWorkers.map((w) => ({ ...w }));

// ── Scheduled runs (PRD #241) demo fixtures + helpers ──────────────────────
// schedulePreviewCap mirrors the server's clamp on the preview N (PRD #241 M4).
const schedulePreviewCap = 10;
let scheduleSeq = 700;
const nextScheduleId = () => `sch-${(scheduleSeq++).toString(36)}`;

// The server caps owner guidance at 8 KiB (MaxGuidanceBytes) and 422s an oversize value on
// every write path (validateScheduleConfig, shared by create and update). The Textarea's
// maxLength caps CHARACTERS, not UTF-8 BYTES, so a multibyte value can pass the input yet
// exceed the byte cap — validate bytes on both mock write paths so mock mode reproduces the
// production 422 rather than silently accepting oversized guidance.
const MAX_GUIDANCE_BYTES = 8 * 1024;
function assertGuidanceWithinCap(guidance: string | null | undefined): void {
  if (guidance != null && new TextEncoder().encode(guidance).length > MAX_GUIDANCE_BYTES) {
    throw new ApiError(422, "guidance is too large");
  }
}

// mockScheduleFires computes the next N fire instants (UTC ISO) for a 5-field
// cron string. It handles the canonical preset shapes (specific min/hour, `1-5`,
// single dow, `*/N` steps) — enough for the demo + tests — and returns [] for
// anything it does not understand (a day-of-month/month restriction), which the
// UI renders as an empty preview exactly as a real invalid cron would.
function mockScheduleFires(cron: string, n: number, from = new Date()): string[] {
  const fields = cron.trim().split(/\s+/);
  if (fields.length !== 5) return [];
  const [minF, hrF, domF, monF, dowF] = fields;
  if (domF !== "*" || monF !== "*") return [];
  const expand = (f: string, max: number): number[] => {
    if (f === "*") return Array.from({ length: max + 1 }, (_, i) => i);
    const step = /^\*\/(\d{1,2})$/.exec(f);
    if (step) {
      const s = Number(step[1]);
      const out: number[] = [];
      for (let i = 0; i <= max; i += s) out.push(i);
      return out;
    }
    const range = /^(\d{1,2})-(\d{1,2})$/.exec(f);
    if (range) {
      const out: number[] = [];
      for (let i = Number(range[1]); i <= Number(range[2]); i++) out.push(i);
      return out;
    }
    if (/^\d{1,2}$/.test(f)) return [Number(f)];
    return [];
  };
  const minutes = expand(minF, 59);
  const hours = expand(hrF, 23);
  const dows = dowF === "*" ? null : expand(dowF, 7).map((d) => d % 7);
  if (minutes.length === 0 || hours.length === 0) return [];
  const out: string[] = [];
  const start = new Date(Date.UTC(from.getUTCFullYear(), from.getUTCMonth(), from.getUTCDate()));
  for (let day = 0; day < 400 && out.length < n; day++) {
    const base = new Date(start.getTime() + day * 86_400_000);
    if (dows && !dows.includes(base.getUTCDay())) continue;
    for (const h of hours) {
      for (const mi of minutes) {
        const t = Date.UTC(base.getUTCFullYear(), base.getUTCMonth(), base.getUTCDate(), h, mi);
        if (t > from.getTime() && out.length < n) out.push(new Date(t).toISOString());
      }
    }
  }
  return out.slice(0, n);
}

// scheduleDTO recomputes the live next-fire preview at read time, exactly as the
// server does — the list and the modal preview then agree by construction.
function scheduleDTO(s: Schedule): Schedule {
  let nextFires: string[] = [];
  let nextFireAt: string | null = null;
  if (s.status === "active" && s.enabled) {
    if (s.timing === "recurring") {
      nextFires = mockScheduleFires(s.cron_expr, 3);
      nextFireAt = nextFires[0] ?? null;
    } else if (s.run_at && new Date(s.run_at).getTime() > Date.now()) {
      nextFireAt = s.run_at;
    }
  }
  return { ...s, next_fire_at: nextFireAt, next_fires: nextFires };
}

const daysFromNow = (d: number, h: number, m = 0): string => {
  const t = new Date();
  t.setUTCHours(h, m, 0, 0);
  t.setUTCDate(t.getUTCDate() + d);
  return t.toISOString();
};

// The owner's user-authored schedules (origin='user'). Materialized default rows
// (origin='default') are appended below from the catalog fixture; keeping the two
// lists separate lets the user rows stay free of the three origin fields, which the
// map injects uniformly.
const userSchedules: Omit<
  Schedule,
  "origin" | "catalog_slug" | "customized" | "sibling_group_id" | "baked_guidance"
>[] = [
  {
    id: "sch-7kd2", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "sweep", issue_iid: null, labels: null, prompt: "",
    timing: "recurring", cron_expr: "0 2 * * 1-5", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(-1, 2), auto_approve: true, wait_on_limit: true,
    max_issues: 1,
    guidance: "Keep the diff small and add a failing test first.",
    model: "fable",
    override_subagent_model: true,
    enabled: true, status: "active", created_at: daysFromNow(-14, 9),
    updated_at: daysFromNow(-1, 2), next_fires: [],
    // Fired on time, started nothing: the one candidate within the cap was skipped
    // for a benign reason, and `capped` says there were older candidates behind it —
    // the amber cell + the cap hint (Goal 2). The whole point of PRD #308.
    last_fire: {
      fired_at: daysFromNow(-1, 2), matched: 1, capped: true,
      started: [],
      skips: [
        {
          issue_iid: 96,
          title: "Mid-run worker restart discards all un-pushed commits on resume",
          reason: "not_eligible",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/96",
        },
      ],
    },
  },
  {
    id: "sch-3bf1", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "issue", issue_iid: 142, labels: null, prompt: "",
    timing: "recurring", cron_expr: "0 3 * * *", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(0, 3), auto_approve: false, wait_on_limit: true,
    max_issues: null,
    guidance: "Prefer the smallest change that closes the issue; no new deps.",
    model: null,
    override_subagent_model: false,
    enabled: true, status: "active", created_at: daysFromNow(-9, 10),
    updated_at: daysFromNow(0, 3), next_fires: [],
    // A healthy fire: it started the run it matched (green "1 started").
    last_fire: {
      fired_at: daysFromNow(0, 3), matched: 1, capped: false,
      started: [
        {
          issue_iid: 142,
          run_id: "3f1a2b7c-9d4e-4a1b-8c6d-1e2f3a4b5c6d",
          title: "RunKind (TypeScript) omits 'chat', which the DB CHECK allows",
          web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/142",
        },
      ],
      skips: [],
    },
  },
  {
    id: "sch-9qm4", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "issue", issue_iid: 158, labels: null, prompt: "",
    timing: "once", cron_expr: "", run_at: daysFromNow(1, 9),
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: null, auto_approve: true, wait_on_limit: false,
    max_issues: null,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true, status: "active", created_at: daysFromNow(-1, 20),
    updated_at: daysFromNow(-1, 20), next_fires: [], last_fire: null,
  },
  {
    id: "sch-pr0m", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "prompt", issue_iid: null, labels: null,
    prompt: "hunt for flaky tests and open an MR",
    timing: "recurring", cron_expr: "0 9 * * 1", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(-7, 9), auto_approve: true, wait_on_limit: false,
    max_issues: null,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true, status: "active", created_at: daysFromNow(-21, 11),
    updated_at: daysFromNow(-7, 9), next_fires: [], last_fire: null,
  },
  {
    id: "sch-zt88", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas-api",
    target: "sweep", issue_iid: null, labels: ["bug"], prompt: "",
    timing: "recurring", cron_expr: "0 */6 * * *", run_at: null,
    timezone: "UTC", next_fire_at: null,
    last_fired_at: daysFromNow(-3, 18), auto_approve: true, wait_on_limit: false,
    max_issues: 3,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: false, status: "active", created_at: daysFromNow(-30, 8),
    updated_at: daysFromNow(-3, 18), next_fires: [],
    // A healthy sweep: every matched candidate started a run (green "3 started",
    // each pairing issue ↔ run in the expanded panel).
    last_fire: {
      fired_at: daysFromNow(-3, 18), matched: 3, capped: false,
      started: [
        { issue_iid: 124, run_id: "a20b4e51-77c8-4d2a-9f10-2b3c4d5e6f70", title: "web: judge free text renders without Unicode Cf stripping", web_url: "https://gitlab.example.com/vtmocanu/atlas-api/-/issues/124" },
        { issue_iid: 139, run_id: "c7d5f0a2-1e34-4b56-88a9-0c1d2e3f4a5b", title: "Poller sync timeouts against forge-fake in the e2e stack" },
        { issue_iid: 151, run_id: "e91f6b03-42d7-4c88-b1a2-3c4d5e6f7a80", title: "Board card CI badge flickers on refetch" },
      ],
      skips: [],
    },
  },
  {
    // A parked schedule (status='error'): the last fire failed and the scheduler
    // stopped advancing it, so the list shows the red "parked" badge and an "error"
    // Next-run pill. Demoing this state is the whole reason it's a seed row.
    id: "sch-er0r", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi",
    target: "issue", issue_iid: 173, labels: null, prompt: "",
    timing: "recurring", cron_expr: "30 1 * * *", run_at: null,
    timezone: "Europe/Bucharest", next_fire_at: null,
    last_fired_at: daysFromNow(-1, 1, 30), auto_approve: true, wait_on_limit: false,
    max_issues: null,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true, status: "error", created_at: daysFromNow(-12, 15),
    updated_at: daysFromNow(-1, 1, 30), next_fires: [], last_fire: null,
  },
];

// The builtin default-jobs catalog (PRD #589), mirroring
// api/internal/schedtmpl/catalog/*.md. A prompt entry carries the file body as
// `prompt` (labels/guidance empty, max_issues 0); a sweep entry carries its selector
// `labels` plus the body as `guidance` (prompt empty). auto_approve/wait_on_limit are
// the fixed run flags every default is seeded with (schedtmpl.AutoApprove/WaitOnLimit).
const scheduleCatalog: CatalogEntry[] = [
  {
    slug: "test-improvement",
    name: "Weekly test improvement",
    description: "Weekly pass that finds one under-tested area and strengthens its tests.",
    target: "prompt", cron: "0 8 * * 1", timezone: "UTC", model: "",
    prompt:
      "Spend this run improving the project's automated tests. Pick ONE area that is meaningfully under-tested — a module with thin coverage, an important branch with no assertion, or a bug-prone path — and add focused, genuinely useful tests for it. Prefer a small number of high-value tests over many shallow ones, and run the project's test suite to confirm your additions pass.\n\nMake every test earn its place: prove each assertion is non-vacuous by identifying a plausible defect that would make it fail (if you sanity-check by mutating the code under test, do it in a throwaway copy and never a production file, and change it to a value another case already produces, not a fresh sentinel); assert the observable end-state, not an intermediate call; prefer positive assertions over negative ones; never weaken or delete an existing assertion to make a suite pass; and do not re-touch a test another recent run just changed — pick a different area so parallel runs do not collide.\n\nGuardrail: change TEST files only — no production (non-test) file; a behavior needing a production change to be testable is out of scope (pick another area), and a real production bug found while testing is reported, not fixed. Commit your new tests and open a merge request; if nothing worthwhile this week an empty week is acceptable — do not invent low-value tests to hit a number — open no MR and leave a note on what you looked at.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "docs-hygiene",
    name: "Docs hygiene",
    description: "Weekly sweep for mechanical documentation defects — dead links, stale references, drift.",
    target: "prompt", cron: "0 3 * * 1", timezone: "UTC", model: "",
    prompt:
      "Audit the project's documentation for mechanical defects: broken or moved links, references to files or commands that no longer exist, stale frontmatter, and obvious typos. Focus on correctness, not rewriting for style. Verify each problem against the actual repository before fixing it; when a broken link has more than one plausible target, describe it rather than guessing the repoint.\n\nApply the mechanical corrections and open a merge request. Guardrail: mechanical fixes only (links, stale refs, frontmatter, typos), documentation files only — no prose rewrites and no source/build/CI/agent-config edits, keep the diff to mechanical corrections. If there is nothing to fix, open no MR and leave a note.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "bug-hunt",
    name: "Bug hunt — deep audit",
    description: "Deep audit of one subsystem for correctness bugs, confirmed by a reviewer and an auditor.",
    target: "prompt", cron: "0 4 * * 3", timezone: "UTC", model: "",
    prompt:
      "Pick ONE subsystem and audit it deeply for real correctness bugs: unhandled errors, race conditions, off-by-one and boundary mistakes, incorrect edge-case handling, and broken invariants. For every candidate bug, construct the concrete input or state that triggers it and confirm the wrong behavior by reading the code carefully; have a reviewer and an auditor confirm each finding before you rely on it, and discard anything you cannot substantiate.\n\nFor the single highest-confidence bug, apply the smallest correct fix backed by a deterministic test that would have caught it (fails reliably before, passes after; skip the test only for a non-code fix or a genuinely contrived reproduction, and say why), commit it, and open one merge request. If you find no clearly-real bug, open no MR and leave your audit notes as a report.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "feature-bingo",
    name: "Feature bingo",
    description: "Weekly brainstorm that proposes one concrete new feature and opens an MR adding it as an idea file.",
    target: "prompt", cron: "0 3 * * 2", timezone: "UTC", model: "fable",
    prompt:
      "Brainstorm ONE concrete, genuinely useful new feature or improvement for this project. Ground it in what the codebase actually does: name the problem it solves, sketch how it would work, and note roughly where it would live and what it would touch.\n\nFirst read the existing files under the `ideas/` folder to avoid duplicates, and check the codebase so you do not propose something that already exists. Write your proposal to a single new idea file under the `ideas/` folder at the repository root, commit it, and open a merge request titled `bingo: <feature>`. If nothing worthwhile comes to mind, open no MR and leave a note.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "refactor-scout",
    name: "Refactor scout",
    description: "Biweekly propose-only scout that surveys the repo for one high-value structural refactor and opens an MR adding it as a proposal file.",
    target: "prompt", cron: "0 5 1,15 * *", timezone: "UTC", model: "fable",
    prompt:
      "Survey this repository for ONE high-value structural refactor worth proposing — and PROPOSE it, never implement it. This job never changes the code under refactor; its only output is a single proposal file. Pick a candidate from these shapes: duplication with 3+ occurrences of the same responsibility, an oversized file with a natural cohesion seam, dead branches or config the gates cannot see, or a costly consistency defect — the one with the best impact-to-effort ratio.\n\nDedup before you propose: read the existing `ideas/refactors/` folder INCLUDING already-declined proposals and their decline reasons, and do not re-propose a recorded idea unless the evidence has materially changed (say exactly what changed). Carry your own rigour — derive every count from a named command you ran, cite each claim as `file:line @ <sha>`, and mark it verified or plausible. File it ONLY if it passes its own rubric (impact >= effort, behavior-preserving or flagged, a dedup needs 3+ same-responsibility occurrences, a split needs a real seam).\n\nStay on the propose-only, structural side of the self-improvement job, which IMPLEMENTS small fixes — this one proposes refactors too big or risky for one unattended MR. Write the proposal to a single new file `ideas/refactors/YYYY-MM-DD-<slug>.md` (create the folder if needed), commit it, and open a merge request titled `refactor-scout: <slug>`. If nothing clears the bar this cycle, make no change and open no MR: leave a short note on what you surveyed and why nothing qualified.",
    labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
  {
    slug: "bug-triage",
    name: "Bug triage sweep",
    description: "Daily sweep over open issues labelled \"bug\", starting a run for the oldest few.",
    target: "sweep", cron: "0 2 * * *", timezone: "UTC", model: "",
    prompt: "", labels: ["bug"], max_issues: 3, auto_approve: true, wait_on_limit: true,
    guidance:
      "Triage the sweep's bug issue. Reproduce or confirm the reported problem, find its root cause, and fix it if the fix is small and well-contained; otherwise document the diagnosis and the minimal reproduction so a maintainer can act. Keep changes scoped to the bug at hand and back any fix with a test that would have caught it.",
  },
  {
    slug: "planned-sweep",
    name: "Planned-work sweep",
    description: "Daily sweep over open issues labelled \"Planned\", starting a run for the oldest few.",
    target: "sweep", cron: "0 2 * * *", timezone: "UTC", model: "",
    prompt: "", labels: ["Planned"], max_issues: 3, auto_approve: true, wait_on_limit: true,
    guidance:
      "Implement the sweep's planned-work issue. Treat the issue description (and any linked spec) as the specification, deliver the change end to end with tests, and run the project's gate before finishing. Keep the work scoped to what the issue asks for and stop to report if it turns out to depend on something not yet in place.",
  },
  {
    slug: "assigned-sweep",
    name: "Assigned-work sweep",
    description: "Daily sweep over open issues assigned to the uzi bot account, starting a run for the oldest few.",
    target: "sweep", cron: "0 2 * * *", timezone: "UTC", model: "",
    prompt: "", labels: null, max_issues: 3, auto_approve: true, wait_on_limit: true,
    guidance:
      "Implement the sweep's assigned issue. This sweep selects by assignee rather than a label, so there is no selector label to match. Treat the issue description (and any linked spec) as the specification, deliver the change end to end with tests, and run the project's gate before finishing. Keep the work scoped to what the issue asks for and stop to report if it turns out to depend on something not yet in place.",
  },
  {
    // A self_improve entry (PRD #590) carries neither a prompt nor labels/guidance: the
    // orchestration lead resolves its own tracking issue at fire time. So it edits like a
    // prompt default (cadence/model only) but has no baked text to show.
    slug: "self-improve",
    name: "Self-improvement",
    description: "Autonomous self-improvement — audit uzi's own codebase and open one improvement MR per cycle.",
    target: "self_improve", cron: "0 4 */2 * *", timezone: "UTC", model: "",
    prompt: "", labels: [], guidance: "", max_issues: 0, auto_approve: true, wait_on_limit: true,
  },
];

// materializeDefault builds an origin='default' schedule row from a catalog entry, as
// the server does when the owner enables a default on a repo (PRD #589 M2): the
// resolved catalog values are carried on the row so the modal can show the baked
// prompt read-only, even though the real DB stores NULL for those columns.
function materializeDefault(
  entry: CatalogEntry,
  repoId: string,
  id: string,
  over: Partial<Schedule> = {},
): Schedule {
  const repo = mockRepos.find((r) => r.id === repoId);
  const now = new Date().toISOString();
  return {
    id,
    repo_id: repoId,
    repo_path: repo?.path_with_namespace ?? "",
    target: entry.target,
    issue_iid: null,
    labels: entry.target === "sweep" && entry.labels ? [...entry.labels] : null,
    prompt: entry.target === "prompt" ? entry.prompt : "",
    timing: "recurring",
    cron_expr: entry.cron,
    run_at: null,
    timezone: entry.timezone,
    next_fire_at: null,
    last_fired_at: null,
    last_fire: null,
    auto_approve: entry.auto_approve,
    wait_on_limit: entry.wait_on_limit,
    // PRD #841: match the real ScheduleDTO shape — mr_rework_enabled is a non-omitempty
    // *bool, so the API always emits it (null = inherit), never omits it. A catalog default
    // is inherit until an override sets it, so seed the explicit null sentinel here rather
    // than leaving the field undefined (which would diverge from the server response shape).
    mr_rework_enabled: null,
    max_issues: entry.target === "sweep" ? entry.max_issues : null,
    // Owner OVERLAY (issue #675): null by default; a seed sets it via `...over`.
    guidance: null,
    // Resolved catalog guidance for a sweep default, shown read-only (issue #675).
    baked_guidance: entry.target === "sweep" ? entry.guidance || null : null,
    model: entry.model || null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "default",
    catalog_slug: entry.slug,
    customized: false,
    // Defaults never group (grouping is a custom-row view concept, PRD #636 Decision 2).
    sibling_group_id: null,
    created_at: now,
    updated_at: now,
    next_fires: [],
    ...over,
  };
}

const catalogBySlug = (slug: string): CatalogEntry | undefined =>
  scheduleCatalog.find((e) => e.slug === slug);

// Seed a few materialized defaults so the Default-jobs UX is visible under
// VITE_UZI_MOCK=1: bug-triage enabled on TWO repos (Layout A — one summary row
// expanding to two per-repo sub-rows, one active + one paused so the resume
// affordance shows), and docs-hygiene enabled + customized on one repo (the
// "customized" indicator + a prominent Reset).
const seededDefaults: Schedule[] = [
  materializeDefault(catalogBySlug("bug-triage")!, "repo-uzi", "sch-def-bt-uzi", {
    last_fired_at: daysFromNow(-1, 2),
    last_fire: {
      fired_at: daysFromNow(-1, 2), matched: 2, capped: false,
      started: [
        { issue_iid: 96, run_id: "b1c0ffee-0000-0000-0000-000000000096", title: "Worker restart drops commits", web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/96" },
        { issue_iid: 88, run_id: "b1c0ffee-0000-0000-0000-000000000088", title: "Race in the run poller", web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/88" },
      ],
      skips: [],
    },
  }),
  // Already materialized on repo-atlas but PAUSED: re-enabling is a server no-op that
  // returns this paused row, so the UI must offer resume here, never a fresh "enable".
  materializeDefault(catalogBySlug("bug-triage")!, "repo-atlas", "sch-def-bt-atlas", {
    enabled: false,
  }),
  // A customized default (owner shifted the cadence): the "customized" indicator + a
  // prominent Reset that restores the catalog cron.
  materializeDefault(catalogBySlug("docs-hygiene")!, "repo-uzi", "sch-def-dh-uzi", {
    cron_expr: "0 6 * * 1",
    customized: true,
  }),
  // A sweep default carrying an owner GUIDANCE OVERLAY (issue #675): its baked_guidance
  // is the catalog planned-sweep text while `guidance` is a distinct, owner-set overlay,
  // so the modal must show the two independently. The two values MUST differ so a test
  // cannot pass vacuously against the old single-field shape. planned-sweep on repo-ledger
  // is used by no other seed/test, so this adds no enablement collision.
  materializeDefault(catalogBySlug("planned-sweep")!, "repo-ledger", "sch-def-ps-overlay", {
    guidance: "prefer a failing test first, then the smallest fix",
    customized: true,
  }),
];

let schedules: Schedule[] = [
  ...userSchedules.map(
    (s): Schedule => ({
      ...s,
      origin: "user",
      catalog_slug: null,
      customized: false,
      sibling_group_id: null,
      // Baked catalog guidance is default-sweep-only (issue #675); a user row never has it.
      baked_guidance: null,
    }),
  ),
  ...seededDefaults,
];

// The forge labels that already exist on each repo (PRD #589 M4, sweep-label WARN).
// repo-atlas is deliberately missing "bug" and "Planned" so enabling bug-triage /
// planned-sweep there surfaces the missing-label warning + the "Create label" confirm.
const repoLabels: Record<string, string[]> = {
  "repo-uzi": ["bug", "Planned", "PRD", "enhancement"],
  "repo-atlas": ["PRD", "enhancement"],
  "repo-payments": ["PRD"],
};

let appSettings: AppSettings = loadedSettings.appSettings;
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

// boardResponse clones a board fixture for return. Cards are shallow-cloned so a caller
// mutating the response never touches the stored fixture.
function boardResponse(b: Board): { board: Board } {
  return {
    board: {
      ...b,
      cards: b.cards.map((c) => ({ ...c })),
    },
  };
}
let workerCounter = 0;


function listRunsFor(): Run[] {
  return [...state.runs.values()].sort((a, b) => b.created_at.localeCompare(a.created_at));
}

// sessionBody is the auth/session bootstrap payload: the signed-in user, the
// current instance labels (PRD #19 M2, PRD #764: the single `uzi` label and the
// autopilot label), and the three resolved theme fields (PRD #21), mirroring the real
// API so the mocked SPA resolves them the same way.
function sessionBody() {
  return {
    user: requireSession(),
    uzi_label: appSettings.uzi_label,
    autopilot_label: appSettings.autopilot_label,
    theme: resolveTheme(userSettings.theme, appSettings.default_theme),
    theme_override: userSettings.theme,
    default_theme: appSettings.default_theme,
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

// rejectInvisibleLabel mirrors the server's validateSecretLabel Cf rule (PRD #111).
//
// The mock is this repo's BROWSABLE SPEC, and until this existed it accepted labels
// production rejects — which is how a browser pass managed to store a bidi-override
// label and demonstrate F12 against a build that was supposed to make it impossible.
// A mock that disagrees with the API about what is valid teaches the wrong lesson and
// leaves the new error copy with nowhere to be seen.
//
// Control characters are not re-checked here: the real validator rejects them too,
// but they cannot be typed into the field, so the Cf half is the one a demo exercises.
function rejectInvisibleLabel(label: string): void {
  if (/\p{Cf}/u.test(label)) {
    throw new ApiError(
      400,
      "Label must not contain invisible formatting characters (zero-width spaces and joiners, bidirectional overrides, the byte-order mark): they let two different tokens look identical, or make a label read as a different account. This also rules out multi-part emoji such as 👨‍👩‍👧, which are joined by one of these characters, so use a plain name instead",
    );
  }
}

// pooledFixtureStatus is each demo token's eligibility WHEN POOLED, so toggling one
// off and on again returns it to the state its fixture describes instead of
// flattening every token to `eligible`.
const pooledFixtureStatus: Record<string, AutoStatus> = {
  "sec-never-polled": "no_reading",
  "sec-low": "below_threshold",
};

export const mockApi = {
  // ── Auth: instant and fake. Any credentials sign in as the admin. ──────────
  // The session bootstrap carries the instance labels alongside the user, mirroring
  // the real API (PRD #19 M2), so the mocked SPA resolves them the same way.
  register: async (email: string, _password: string, displayName: string) => {
    state.session = { ...mockAdmin, email, display_name: displayName || mockAdmin.display_name };
    return delay(sessionBody());
  },
  login: async (email: string, _password: string) => {
    // Persona switch for the demo: logging in as a seeded non-admin (e.g.
    // mira@uzi.local) signs in AS that user, so the non-admin rendering paths
    // (no Global create, view-only builtin/global, own-skills-only) are
    // browser-checkable. Any other email is the admin, as before.
    const persona = users.find((u) => u.email === email.trim().toLowerCase());
    state.session = persona ? { ...persona } : { ...mockAdmin, email: email || mockAdmin.email };
    return delay(sessionBody());
  },
  // Demo mode has registration open and unrestricted. The OIDC fields follow the
  // scenario toggle (default off; ?mock=oidc / sso-only enable SSO) — PRD #45 N6.
  authConfig: async () => {
    const d = oidcDemo();
    return delay({
      registration_enabled: true,
      allowed_email_domains: [],
      oidc_enabled: d.oidcEnabled,
      oidc_provider_name: d.providerName,
      password_login_enabled: d.passwordLoginEnabled,
    });
  },
  // The in-browser demo build has no server; report "demo" to match the header pill.
  // A real SemVer, not "demo" (PRD #113 M5). Upgrade classification compares against
  // this, and a non-SemVer control-plane version turns classification OFF entirely — so
  // the literal "demo" made every badge and the whole Fleet panel unreachable in demo
  // mode. The demo-mode signal does not live here: AppShell renders a separate "demo"
  // pill, so nothing is lost by making this comparable.
  // PRD #113 M6. Computed LIVE from the demo worker list rather than hardcoded, so the
  // badge actually clears when a worker is deleted — web-ux needs to see it appear AND
  // clear, and a constant would only ever show the first half.
  workerUpgradeSummary: async () => {
    const attention = workers.filter(
      (w) => w.upgrade_status === "upgrade_failed" || w.upgrade_status === "outdated",
    ).length;
    return delay({ attention, target_release: "0.4.2" });
  },

  // The FULLY-STAMPED fixture (PRD #175), so the demo shows the popover with every
  // row present — a `dev` build here would hide the three fields this PRD exists to
  // add, in the build whose whole job is to show them off.
  //
  // KNOWN CONSEQUENCE, worth stating rather than leaving to be rediscovered: this
  // is the default for every VITE_UZI_MOCK=1 run, so a browser pass sees the
  // STAMPED shape unless someone swaps this line. The degraded shapes are covered
  // in BuildInfoPopover.test.tsx (mockBuildInfoUnstamped = the laptop's three-key
  // body, mockBuildInfoNoUptime = the struct-literal Handler's two-key one); to see
  // either in a browser, point this line at it. `typeof realApi` cannot enforce any
  // of it, since every field but version and founded is optional.
  version: async () => delay(mockBuildInfo),
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
  logout: async () => {
    state.session = null;
    return delay({ status: "ok" });
  },
  me: async () => {
    if (!state.session) throw new ApiError(401, "authentication required");
    return delay(sessionBody(), 40);
  },

  // ── Admin: users ────────────────────────────────────────────────────────────
  listUsers: async () => delay({ users: users.map((u) => ({ ...u })) }),
  setUserActive: async (id: string, isActive: boolean) => {
    const u = users.find((x) => x.id === id);
    if (!u) throw new ApiError(404, "user not found");
    u.is_active = isActive;
    return delay({ user: { ...u } });
  },

  // ── Admin: instance settings (PRD #19) ───────────────────────────────────────
  // Mirrors the server's Decision 8 validation so the demo surfaces the same
  // rejection messages the real API would.
  getSettings: async () => delay(settingsResponse()),
  // Demo is fully DEK-sealed (no legacy rows), so the admin migration notice is
  // hidden; the wiring is still exercised by the AdminSettings unit test.
  vaultMigration: async () => delay({ master_sealed: 0 }),

  // ── Agent source (PRD #602 M5) ───────────────────────────────────────────────
  getAgentSource: async () => {
    requireSession();
    // Derive update_available/latest_ref at read time from the stored remote facts +
    // the live config (PRD #702 M4), so a bump/apply self-clears the badge with no egress.
    return delay({ agent_source: agentSourceView() });
  },
  // "Sync now": re-runs the reconcile. In the mock it refreshes the sync timestamp
  // and marks the fetch healthy, leaving the staged snapshot pending for review. A
  // sync against no configured URL records an error rather than staging (mirrors the
  // server, which never fetches an empty URL).
  syncAgentSource: async () => {
    requireSession();
    const now = new Date().toISOString();
    if (agentSource.config.url.trim() === "") {
      agentSource.status = {
        ...agentSource.status,
        last_sync_at: now,
        last_sync_status: "error",
        last_sync_error: "No repository URL is configured.",
      };
      agentSource.staged = null;
    } else {
      agentSource.status = {
        ...agentSource.status,
        last_sync_at: now,
        last_sync_status: "ok",
        last_sync_error: undefined,
      };
      if (agentSource.staged) agentSource.staged.fetched_at = now;
    }
    return delay({ agent_source: agentSourceView() });
  },
  // Approve-and-apply. expected_sha binds the reviewed snapshot: a mismatch (or
  // nothing pending) is the 409 the real handler returns for ErrStaleApproval, so the
  // admin re-reviews rather than applying blind.
  applyAgentSource: async (expectedSha: string) => {
    requireSession();
    const staged = agentSource.staged;
    if (!staged || !staged.pending || staged.fetched_sha !== expectedSha) {
      throw new ApiError(409, "the staged snapshot changed since you reviewed it; re-review before applying");
    }
    // Tally the outcome from the diff, mirroring agentsource.ApplyResult.
    let applied = 0;
    let unchanged = 0;
    let conflicts = 0;
    let deprovisioned = 0;
    for (const d of staged.diff) {
      if (d.action === "add" || d.action === "override") applied++;
      else if (d.action === "unchanged") unchanged++;
      else if (d.action === "conflict") conflicts++;
      else if (d.action === "remove") deprovisioned++;
    }
    const skippedParse = staged.roles.filter((r) => !r.ok).length;
    const now = new Date().toISOString();
    agentSource.status = {
      ...agentSource.status,
      last_applied_at: now,
      last_applied_sha: staged.fetched_sha,
    };
    staged.pending = false;
    const result: AgentSourceApplyResult = {
      sha: staged.fetched_sha,
      applied,
      unchanged,
      conflicts,
      deprovisioned,
      skipped_parse: skippedParse,
      already_applied: false,
      message: "applied",
    };
    return delay({ result });
  },

  // Resolve the latest semver tag for a supplied (possibly unsaved) URL (PRD #702 M3).
  // Mirrors the real endpoint: an empty URL is a 400, a host off the SSRF allowlist is
  // the same 400 the ls-remote path refuses with (the resolve is anonymous, so it only
  // works against a public, allowlisted source). On success it returns a plausible
  // semver tag — v1.10.0 for the canonical skills repo, which sorts ABOVE a lexical
  // v1.2.0 (the discriminating fixture). Any other allowlisted URL returns the same.
  resolveAgentSourceLatest: async (url: string) => {
    requireSession();
    const trimmed = url.trim();
    if (trimmed === "") throw new ApiError(400, "url is required");
    let host = "";
    try {
      const parsed = new URL(trimmed);
      if (parsed.protocol !== "https:") throw new Error("scheme");
      host = parsed.hostname;
    } catch {
      throw new ApiError(400, "URL is not in the agent-source allowlist");
    }
    if (!mockAllowedAgentSourceHosts.includes(host)) {
      throw new ApiError(400, "URL is not in the agent-source allowlist");
    }
    return delay({ latest_ref: "v1.10.0" });
  },

  // Update check (PRD #702 M4): ls-remotes the CONFIGURED source with its sealed
  // credential and PERSISTS the remote facts (latest semver tag + tip sha + checked-at),
  // mirroring the server's persist-facts split. getAgentSource then DERIVES
  // update_available/latest_ref from these + the live config, so the badge self-clears
  // after a bump. The mock simulates a source ahead of the default v1.2.0 pin: a latest
  // tag of v1.10.0 (which sorts above v1.2.0 — the discriminating fixture) and a moved tip.
  updateCheckAgentSource: async () => {
    requireSession();
    const now = new Date().toISOString();
    if (agentSource.config.url.trim() === "") {
      // No configured source to check — record the failure in the last_sync_error slot
      // the update check reuses (per the server), so the card surfaces it.
      agentSource.status = {
        ...agentSource.status,
        last_sync_status: "error",
        last_sync_error: "No repository URL is configured.",
      };
      return delay({ agent_source: agentSourceView() });
    }
    // Persist the remote facts; the derived fields are computed at read time.
    agentSourceRemote = {
      latestRef: "v1.10.0",
      tipSha: "a1b2c3d4e5f60718293a",
      checkedAt: now,
    };
    agentSource.status = {
      ...agentSource.status,
      last_sync_status: agentSource.status.last_sync_status ?? "ok",
      last_sync_error: undefined,
    };
    return delay({ agent_source: agentSourceView() });
  },

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
      // slack_enabled / judge_enabled / … are strict bools, not labels — without this
      // arm they would fall through to the label rules and fail open on "yes"/"maybe".
      if (
        key === "slack_enabled" ||
        key === "judge_enabled" ||
        key === "judge_enforce_all" ||
        key === "ephemeral_workers_enabled" ||
        key === "release_check_enabled" ||
        key === "release_check_banner_enabled"
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

  // ── Run-judge opt-in (PRD #46) ───────────────────────────────────────────────
  // Own-user (session identity, never a body id, mirroring the server's audit H3).
  setJudgeEnabled: async (enabled: boolean, anthropicToken?: string | null) => {
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
  setCIAutofixEnabled: async (enabled: boolean) => {
    const u = requireSession();
    u.ci_autofix_enabled = enabled;
    return delay({ user: { ...u } }, 200);
  },
  // Admin per-user toggle: target from the id argument (the path on the server).
  setUserCIAutofixEnabled: async (id: string, enabled: boolean) => {
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

  ...notificationsApi,

  // ── Secrets ─────────────────────────────────────────────────────────────────
  listSecrets: async () =>
    delay({
      // Default first, then by label — the order the server's query returns.
      secrets: [...secrets]
        .sort((a, b) =>
          a.is_default === b.is_default ? a.label.localeCompare(b.label) : a.is_default ? -1 : 1,
        )
        .map((s) => ({ ...s })),
    }),
  putAnthropicToken: async (_token: string) => {
    // Mirror the real API: a locked vault cannot seal a new token (PRD #32).
    requireUnlockedVault();
    const now = new Date().toISOString();
    // The D14 alias rotates the DEFAULT, or creates the first one labelled
    // "default" — exactly what UpsertDefaultUserSecret does server-side.
    const existing = secrets.find((s) => s.kind === "anthropic_token" && s.is_default);
    if (existing) {
      existing.updated_at = now;
      return delay({ secret: { ...existing } });
    }
    const created: SecretMeta = {
      id: `sec-${Math.random().toString(36).slice(2, 8)}`,
      kind: "anthropic_token",
      label: "default",
      is_default: true,
      // The user's FIRST/SOLE anthropic_token is born pooled (issue #804) so a
      // single-token user has a non-empty auto-select pool; token #2+ stays
      // opt-in. Compute it faithfully off the "no existing anthropic_token" rule,
      // evaluated BEFORE pushing this row, or the mock teaches the wrong lesson.
      auto_eligible: secrets.filter((s) => s.kind === "anthropic_token").length === 0,
      created_at: now,
      updated_at: now,
    };
    secrets.push(created);
    return delay({ secret: { ...created } });
  },

  // ── Token CRUD (PRD #104 M2) ───────────────────────────────────────────────
  createAnthropicToken: async (_token: string, label: string, isDefault: boolean) => {
    requireUnlockedVault();
    const trimmed = label.trim();
    if (trimmed === "") throw new ApiError(400, "label must not be empty");
    rejectInvisibleLabel(trimmed);
    const anthropic = () => secrets.filter((s) => s.kind === "anthropic_token");
    if (anthropic().some((s) => s.label.toLowerCase() === trimmed.toLowerCase())) {
      throw new ApiError(409, "a token with that label already exists");
    }
    // The server FORCES a user's first token to be the default whatever the body
    // asks (the invisible-token hazard); mirror that here or the mock teaches the
    // wrong lesson.
    const first = anthropic().length === 0;
    const wantDefault = isDefault || first;
    if (wantDefault) anthropic().forEach((s) => (s.is_default = false));
    const now = new Date().toISOString();
    const created: SecretMeta = {
      id: `sec-${Math.random().toString(36).slice(2, 8)}`,
      kind: "anthropic_token",
      label: trimmed,
      is_default: wantDefault,
      // The user's FIRST/SOLE anthropic_token is born pooled (issue #804); token
      // #2+ stays opt-in (auto_eligible false). Mirror the server or the mock
      // teaches the wrong lesson.
      auto_eligible: first,
      created_at: now,
      updated_at: now,
    };
    secrets.push(created);
    return delay({ secret: { ...created } });
  },
  patchAnthropicToken: async (
    id: string,
    body: { label?: string; default?: boolean; token?: string },
  ) => {
    const row = secrets.find((s) => s.id === id);
    if (!row) throw new ApiError(404, "token not found");
    if (body.token !== undefined) requireUnlockedVault();
    if (body.default === false) {
      throw new ApiError(400, "cannot clear the default; set another token as default instead");
    }
    if (body.label !== undefined) {
      const trimmed = body.label.trim();
      if (trimmed === "") throw new ApiError(400, "label must not be empty");
      rejectInvisibleLabel(trimmed);
      if (
        secrets.some(
          (s) => s.id !== id && s.kind === row.kind && s.label.toLowerCase() === trimmed.toLowerCase(),
        )
      ) {
        throw new ApiError(409, "a token with that label already exists");
      }
      row.label = trimmed;
    }
    if (body.default === true) {
      secrets.filter((s) => s.kind === row.kind).forEach((s) => (s.is_default = false));
      row.is_default = true;
    }
    row.updated_at = new Date().toISOString();
    return delay({ secret: { ...row } });
  },
  // The auto-selection pool toggle (PRD #111 M2). It also re-derives the token's
  // live eligibility, because in the mock that is the only way the chip beside the
  // toggle can move — and a toggle whose visible consequence never changes is the
  // silent no-op the real feature exists to make visible.
  setTokenAutoEligible: async (id: string, autoEligible: boolean) => {
    const row = secrets.find((s) => s.id === id);
    if (!row) throw new ApiError(404, "token not found");
    row.auto_eligible = autoEligible;
    row.updated_at = new Date().toISOString();
    const meter = mockMyTokenRateLimits.find((t) => t.secret_id === id);
    if (meter) {
      meter.auto_eligible = autoEligible;
      // Opting OUT is always `not_pooled` — that gate comes first server-side too.
      // Opting IN restores the token's OWN fixture state rather than hard-coding
      // `eligible` (web-ux F2): the four states the feature exists for — never
      // polled, stale, no usage data, low headroom — were unreachable in the demo
      // because this line asserted every pooled token is pickable, which is the very
      // thing the chip exists to disprove.
      //
      // This does NOT re-implement the gate. The real status is autoselect.Classify's
      // answer, computed server-side; this restores a fixture value, which is why it
      // lives here and not in lib/rateLimits.ts.
      meter.auto_status = autoEligible ? (pooledFixtureStatus[id] ?? "eligible") : "not_pooled";
    }
    return delay({ secret: { ...row } });
  },
  deleteAnthropicTokenById: async (id: string) => {
    const row = secrets.find((s) => s.id === id);
    if (!row) throw new ApiError(404, "token not found");
    const siblings = secrets.filter((s) => s.kind === row.kind);
    // D6: the default may not be deleted while others exist — promote first.
    if (row.is_default && siblings.length > 1) {
      throw new ApiError(
        409,
        "cannot delete the default token while other tokens exist; set another token as default first",
      );
    }
    secrets = secrets.filter((s) => s.id !== id);
    // The real schema CASCADES: migrations 00078/00079 hang composite FKs off
    // user_secrets (user_id, id) with ON DELETE SET NULL, so deleting a bound token
    // unbinds its workers and the judge rather than orphaning them. Without this the
    // mock left workers reading "spends console-key" forever — and with one token
    // left the picker is hidden, so there was no way to correct it. Two reasons that
    // matters beyond tidiness: the shipped Dockerfile.mock demo was showing D5's own
    // promise being broken, and D5's cascade otherwise has schema-level evidence
    // only. Mirrored here so a browser can prove the behaviour end to end.
    workers.forEach((w) => {
      if (w.anthropic_secret_id === id) {
        w.anthropic_secret_id = null;
        w.anthropic_secret_label = null;
      }
    });
    // `state.session` is a COPY, not a reference into `users`, so both have to be
    // swept or the cascade would be invisible to /me — which is the read every
    // judge surface actually uses.
    [...users, state.session].forEach((u) => {
      if (u && u.judge_anthropic_secret_id === id) {
        u.judge_anthropic_secret_id = null;
        u.judge_anthropic_secret_label = null;
      }
    });
    return delay(null);
  },

  // ── Vault (PRD #32) ───────────────────────────────────────────────────────────
  // Any non-empty password unlocks in the demo (there is no real crypto); an empty
  // password is treated as the "wrong password" 403 so the banner's error path is
  // browsable.
  vaultUnlock: async (password: string) => {
    if (password.trim() === "") throw new ApiError(403, "incorrect password");
    state.vaultUnlocked = true;
    return delay(null, 150);
  },
  // Passphrase-create (PRD #45): min length 12, then the demo vault is unlocked.
  vaultCreatePassphrase: async (passphrase: string) => {
    if (passphrase.length < 12) throw new ApiError(400, "passphrase must be at least 12 characters");
    state.vaultUnlocked = true;
    return delay(null, 150);
  },
  vaultLock: async () => {
    state.vaultUnlocked = false;
    return delay(null, 100);
  },
  vaultStatus: async () => delay({ unlocked: state.vaultUnlocked }, 40),
  deleteAnthropicToken: async () => {
    // D14: the kind-path alias 409s for a multi-token user — they delete by id.
    const anthropic = secrets.filter((s) => s.kind === "anthropic_token");
    if (anthropic.length > 1) {
      throw new ApiError(
        409,
        "you have multiple tokens; delete a specific one by id (DELETE /api/me/secrets/anthropic_token/{id})",
      );
    }
    secrets = secrets.filter((s) => s.kind !== "anthropic_token");
    return delay(null);
  },
  getMySettings: async () => delay({ settings: { ...userSettings } }),
  putMySettings: async (patch: UserSettingsPatch) => {
    // PATCH-like: apply only the fields present in the body, mirroring the real
    // handler so a theme-only save never clears the model and vice versa.
    if (patch.default_model !== undefined) {
      const trimmed = patch.default_model?.trim() ?? "";
      userSettings = { ...userSettings, default_model: trimmed === "" ? null : trimmed };
    }
    if (patch.default_effort !== undefined) {
      // Closed enum (PRD #617 M5): blank clears to inherit; any other value must be one
      // of the five SDK levels, mirroring the server's ValidateEffort.
      const trimmed = patch.default_effort?.trim() ?? "";
      if (trimmed !== "" && !["low", "medium", "high", "xhigh", "max"].includes(trimmed)) {
        throw new ApiError(400, "effort must be one of low, medium, high, xhigh, max");
      }
      userSettings = { ...userSettings, default_effort: trimmed === "" ? null : trimmed };
    }
    if (patch.judge_model !== undefined) {
      // Same rules as default_model (PRD #69 M2): blank clears to inherit, a value with
      // internal whitespace is rejected, mirroring the server's ValidateModel.
      const trimmed = patch.judge_model?.trim() ?? "";
      if (trimmed !== "" && /\s/.test(trimmed)) {
        throw new ApiError(400, "judge_model must be a single token with no spaces");
      }
      userSettings = { ...userSettings, judge_model: trimmed === "" ? null : trimmed };
    }
    if (patch.summary_model !== undefined) {
      // Same rules as judge_model (PRD #362 M2): blank clears to inherit, a value with
      // internal whitespace is rejected, mirroring the server's ValidateModel.
      const trimmed = patch.summary_model?.trim() ?? "";
      if (trimmed !== "" && /\s/.test(trimmed)) {
        throw new ApiError(400, "summary_model must be a single token with no spaces");
      }
      userSettings = { ...userSettings, summary_model: trimmed === "" ? null : trimmed };
    }
    if (patch.theme !== undefined) {
      const t = patch.theme?.trim() ?? "";
      if (t !== "" && !isTheme(t)) throw new ApiError(400, `unknown theme: "${t}"`);
      userSettings = { ...userSettings, theme: t === "" ? null : t };
    }
    if (patch.sidebar_token_ids !== undefined) {
      // Whole-set replace, mirroring how the real handler would treat a list
      // value; null clears back to default-only. Ids are stored as given — a
      // stale id (deleted token) is harmless, it just matches nothing.
      userSettings = { ...userSettings, sidebar_token_ids: patch.sidebar_token_ids ?? [] };
    }
    if (patch.mr_rework_enabled !== undefined) {
      // Tri-state (PRD #700 M6): present-false opts out, present-true re-enables,
      // present-null clears back to the default-ON state (stored as null). Mirrors
      // the server treating an absent/null value as ON.
      userSettings = { ...userSettings, mr_rework_enabled: patch.mr_rework_enabled ?? null };
    }
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

  ...agentsApi,

  ...forgeApi,

  // ── Board ───────────────────────────────────────────────────────────────────
  getBoard: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    return delay(boardResponse(b));
  },
  configureColumns: async (repoId: string, columns: { label_name: string }[]) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    b.columns = columns.map((c, i) => ({ label_name: c.label_name, position: i }));
    const names = new Set(b.columns.map((c) => c.label_name));
    for (const card of b.cards) if (card.column && !names.has(card.column)) card.column = "";
    return delay(boardResponse(b));
  },
  // Manual board order (PRD #102 M5). This is a SECOND IMPLEMENTATION of the server's
  // freeze, so it is a contract, not a convenience: mockApi.reorder.test.ts pins the
  // four behaviours below one case each, because a fixture that only walks the happy
  // path agrees with a broken mock on everything it covers.
  //
  // The demo board has no evicted iid and no unlisted open card, so a snapshot-style
  // fixture would pass against a mock missing (2) and (3) entirely.
  //
  //   1. cards are reordered to the submitted iid order;
  //   2. an iid not on the board is SKIPPED, not thrown on (the server no-ops per iid,
  //      because an eviction can land between a client's render and its submit);
  //   3. open cards absent from the list fall to the end in iid order (the mirror of
  //      the server's ClearBoardOrderExcept nulling them, plus its NULLS-LAST read);
  //   4. closed cards are untouched and keep their place.
  reorderBoard: async (repoId: string, iids: number[]) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    if (iids.length > 0) {
      const byIID = new Map(b.cards.map((c) => [c.iid, c]));
      const seen = new Set<number>();
      const ordered: Card[] = [];
      for (const iid of iids) {
        const card = byIID.get(iid);
        // (2) unknown iid: skip. (Also skips a duplicate, matching the server's dedupe.)
        //
        // KNOWN DIVERGENCE, recorded rather than left to be discovered (review M5-7):
        // this also skips a CLOSED card, and SetBoardOrderPositions does not — the
        // server would happily rank one it was handed. Unreachable from the product,
        // because dropIntent filters closed cards out before the request is built, so
        // neither side ever sees one. Kept on the mock side because it is the safer
        // half of the divergence and because the demo board contains closed cards: a
        // hand-built mock-mode request that ranked one would render it in the Closed
        // lane at a rank, which is exactly the state Decision 7b forbids. If the server
        // ever gains its own filter, delete this clause rather than adding a second.
        if (!card || card.closed || seen.has(iid)) continue;
        seen.add(iid);
        ordered.push(card);
      }
      // (3) + (4): everything not named keeps a NULL position server-side, which reads
      // back after the positioned rows, in iid order. Closed cards live here too and so
      // are never given a rank.
      const rest = b.cards.filter((c) => !seen.has(c.iid)).sort((x, y) => x.iid - y.iid);
      b.cards = [...ordered, ...rest];
    }
    return delay(boardResponse(b));
  },
  moveIssue: async (repoId: string, iid: number, toColumn: string) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    const to = toColumn === "open" ? "" : toColumn;
    const columnNames = b.columns.map((c) => c.label_name);
    // Preserve every non-column label (incl. `uzi`) and set the new column label.
    card.labels = [...card.labels.filter((l) => !columnNames.includes(l)), ...(to ? [to] : [])];
    card.column = to;
    card.conflict = false;
    return delay({ card: { ...card } }, 320);
  },
  // Promote (PRD #102 M6, Decision 15; PRD #764): add the configured `uzi` label,
  // apply-only and idempotent. Refuses uzi's own self-improvement tracker the way the
  // server does (Decision 13a), so the demo build cannot show a promote the real API
  // would 422.
  promoteIssue: async (repoId: string, iid: number) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    if (card.labels.includes("uzi-self-improve")) {
      throw new ApiError(422, "this issue is uzi's own self-improvement tracker and cannot be promoted");
    }
    const label = appSettings.uzi_label;
    if (!card.labels.includes(label)) card.labels = [label, ...card.labels];
    return delay({ card: { ...card } }, 320);
  },
  getIssue: async (repoId: string, iid: number) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    // IssueDetail is the card fields (minus latest_run) plus a live description.
    // Synthesize one consistent with has_prd_link so the PRD badge lines up with what
    // the description shows (a linked PRD is optional — PRD #764).
    const { latest_run: _latestRun, ...rest } = card;
    const description = card.has_prd_link
      ? `## Summary\n\nImplement the change described in the linked PRD.\n\nSee \`prds/${iid}-feature.md\` for the full specification.`
      : "This issue has no linked `prds/*.md` file. A PRD is optional — label the issue `uzi` and a run can still be started from it.";
    // bot_forge_user_id rides the issue detail (PRD #767 M5), from the board's single
    // connection, so the issue view evaluates the same "uzi OR assigned-to-bot" predicate
    // as the board card — assignee_ids comes through on ...rest.
    return delay({ issue: { ...rest, assignee_ids: rest.assignee_ids ?? [], description, bot_forge_user_id: b.bot_forge_user_id ?? 0 } });
  },
  syncRepo: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    return delay(boardResponse(b), 650);
  },
  createIssue: async (repoId: string, title: string, description: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    const iid = Math.max(0, ...b.cards.map((c) => c.iid)) + 1;
    const card = {
      iid,
      title,
      state: "opened",
      labels: [appSettings.uzi_label],
      assignee_ids: [] as number[],
      web_url: `${b.web_url}/-/issues/${iid}`,
      forge_type: "gitlab",
      author: requireSession().display_name?.toLowerCase() ?? "you",
      has_prd_link: /prds\/[\w.-]+\.md/.test(description),
      column: "",
      closed: false,
      conflict: false,
      // A just-created issue is the most recently updated thing on the board, so it
      // must lead in "Last updated" mode rather than sinking on a zero value.
      forge_updated_at: new Date().toISOString(),
      latest_run: null,
      pipeline: null,
    };
    b.cards.unshift(card);
    return delay({ card: { ...card } }, 450);
  },

  // Per-user, per-repo board preferences (PRD #196 M3). A SECOND IMPLEMENTATION of the
  // server contract, so it persists across calls within the session and matches the
  // wire shape exactly: null extra_labels = "not customised" (fall back to the admin
  // default), an array (incl. []) = the user's absolute set (Decision 9).
  getBoardPrefs: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    // No row yet reads as the pristine default rather than being seeded, so a later
    // reset back to null and "never touched" stay indistinguishable to the client.
    const prefs = state.boardPrefs.get(repoId) ?? { extra_labels: null, show_all: false };
    return delay<BoardPrefs>({ extra_labels: prefs.extra_labels, show_all: prefs.show_all });
  },
  setBoardPrefs: async (repoId: string, prefs: BoardPrefs) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    // Loose validation for mock-mode parity with the server: each extra label must be
    // a non-empty, comma-free, ≤64-char string; an over-cap list is clamped rather
    // than rejected. null (not customised) is preserved as the sentinel.
    let extraLabels: string[] | null = null;
    if (Array.isArray(prefs.extra_labels)) {
      const cleaned: string[] = [];
      for (const raw of prefs.extra_labels) {
        const l = String(raw).trim();
        if (l === "" || l.includes(",") || l.length > 64) continue;
        if (!cleaned.includes(l)) cleaned.push(l);
      }
      extraLabels = cleaned.slice(0, 64);
    }
    const stored: BoardPrefs = { extra_labels: extraLabels, show_all: Boolean(prefs.show_all) };
    state.boardPrefs.set(repoId, stored);
    return delay<BoardPrefs>({ extra_labels: stored.extra_labels, show_all: stored.show_all }, 320);
  },

  // ── Workers ─────────────────────────────────────────────────────────────────
  listWorkers: async () => delay({ workers: workers.map((w) => ({ ...w })) }),
  createWorker: async (name: string, template?: string) => {
    const w = {
      id: `w-new-${++workerCounter}`,
      name,
      status: "offline",
      busy: false,
      // Hand-run: the user starts this container themselves, so it has no size
      // (PRD #58). provisionHostedWorker below is the other kind.
      kind: "external" as const,
      hosted_size: null,
      // No runs and no advertised cap until the worker registers (PRD #42).
      active_runs: 0,
      max_concurrent_runs: null,
      // Declared at issuance; reported stays null until the worker registers.
      template_declared: template ?? null,
      template_reported: null,
      version: null,
      // No version reported until the worker registers, so nothing to compare
      // against the control-plane release (PRD #113).
      upgrade_status: "unknown" as const,
      upgrade_detail: null,
      upgrade_target: "" as const,
      upgrade_blocking_container: null,
      upgrade_blocking_reason: null,
      upgrade_last_exit_code: null,
      last_heartbeat_at: null,
      created_at: new Date().toISOString(),
      // No resource sample until the worker heartbeats (PRD #49) → no gauges yet.
      stats_cpu_pct: null,
      stats_mem_bytes: null,
      stats_mem_limit_bytes: null,
      stats_source: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_bind_mode: "default" as const,
      draining_since: null,
    };
    workers.push(w);
    const token = `uzi_wk_${Array.from(crypto.getRandomValues(new Uint8Array(18)), (b) => b.toString(16).padStart(2, "0")).join("")}`;
    return delay({ worker: { ...w }, token });
  },
  deleteWorker: async (id: string) => {
    workers = workers.filter((w) => w.id !== id);
    return delay(null);
  },
  // PRD #104 M3: rebind a worker to a named token, or clear it with null. Mirrors
  // the real route's label→id resolution and its 400 for an unknown label, so the
  // picker's error path is browsable.
  setWorkerBindMode: async (id: string, mode: BindMode, label: string | null) => {
    const w = workers.find((x) => x.id === id);
    if (!w) throw new ApiError(404, "worker not found");
    // Mirrors the server's refusal of a contradictory pair, so the picker's error
    // path is browsable in the mock rather than only in production.
    if (mode !== "pinned" && label !== null && label.trim() !== "") {
      throw new ApiError(400, "anthropic_token must be null when anthropic_bind_mode is default or auto");
    }
    if (mode !== "pinned") {
      w.anthropic_bind_mode = mode;
      w.anthropic_secret_id = null;
      w.anthropic_secret_label = null;
      return delay({ worker: { ...w } });
    }
    if (label === null || label.trim() === "") {
      throw new ApiError(400, "anthropic_bind_mode=pinned requires a token label in anthropic_token");
    }
    const secret = secrets.find(
      (x) => x.kind === "anthropic_token" && x.label.toLowerCase() === label.trim().toLowerCase(),
    );
    if (!secret) throw new ApiError(400, "no Anthropic token with that label");
    w.anthropic_bind_mode = "pinned";
    w.anthropic_secret_id = secret.id;
    w.anthropic_secret_label = secret.label;
    return delay({ worker: { ...w } });
  },

  // Hosted workers (PRD #58). The demo is the only place M5 can be seen working: on a
  // real stack WORKER_HOSTING_ENABLED is off by default, and turning it on gets you a
  // worker that sits offline forever, because the controller that would run its pod is
  // M3's. So hosting is hardcoded ON here — a demo of a feature that renders nothing
  // is not a demo — and quota 2 against one seeded hosted worker puts the whole
  // journey three clicks away: provision → 2 of 2 → the button disables → delete →
  // it enables again.
  // Quota 5 against FOUR seeded hosted workers (PRD #496 added two cordoned demo
  // workers, w-cordon-eu and w-cordon-idle, on top of the two PRD #113 M5 seeded). The
  // load-bearing property is unchanged and is why the numbers moved together: there is
  // exactly ONE slot of headroom, so web-ux can still drive provision -> at quota ->
  // button disables -> delete -> it enables again, which is the only way to prove the
  // client-side gate RELEASES rather than merely starting disabled.
  //
  // The second seeded worker is the failed roller, which the demo previously could not
  // show at all — so a browser pass could only ever validate the healthy path.
  hostedConfig: async () => delay({ enabled: true, quota: 5, ephemeral_enabled: true }),
  provisionHostedWorker: async (template: string, size: string, docker = false, name?: string) => {
    const w = {
      id: `w-hosted-${++workerCounter}`,
      // Empty name → the server derives one from template + size (handler's
      // derivedHostedWorkerName), now AWS-style `base.l-<4-hex>`: dot notation,
      // lowercase t-shirt letter, random hex suffix. The M5 form sends none, so this
      // is the live path. The real suffix is crypto/rand; the mock stands in a
      // counter-derived 4-hex from workerCounter (already ++'d for the id) to stay
      // deterministic-enough for tests — a plausible suffix, not byte-for-byte parity.
      name:
        name?.trim() ||
        `${template}.${size.toLowerCase()}-${(workerCounter & 0xffff).toString(16).padStart(4, "0")}`,
      // Offline until the controller starts the pod and it registers — the same
      // lifecycle a hand-run worker has, just with the controller doing the running.
      status: "offline",
      busy: false,
      active_runs: 0,
      max_concurrent_runs: null,
      kind: "hosted" as const,
      hosted_size: size,
      docker,
      template_declared: template,
      template_reported: null,
      version: null,
      // No version reported until the worker registers, so nothing to compare
      // against the control-plane release (PRD #113).
      upgrade_status: "unknown" as const,
      upgrade_detail: null,
      upgrade_target: "" as const,
      upgrade_blocking_container: null,
      upgrade_blocking_reason: null,
      upgrade_last_exit_code: null,
      last_heartbeat_at: null,
      created_at: new Date().toISOString(),
      stats_cpu_pct: null,
      stats_mem_bytes: null,
      stats_mem_limit_bytes: null,
      stats_source: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_bind_mode: "default" as const,
      draining_since: null,
    };
    workers.push(w);
    // { worker } and NOTHING ELSE. Do not mint a token here the way createWorker does
    // above: the real endpoint's transaction returns none, its response cannot carry
    // one, and a mock that invents one documents a contract the server structurally
    // cannot honor. TypeScript will not catch it (delay() infers its own T, so an
    // extra field type-checks clean) — mockApi.hosted.test.ts is what does.
    return delay({ worker: { ...w } });
  },

  // ── Runs ────────────────────────────────────────────────────────────────────
  createRun: async (repoId: string, issueIid: number, force?: boolean) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === issueIid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    const active = [...state.runs.values()].some(
      (r) => r.repo_id === repoId && r.issue_iid === issueIid && !["completed", "failed", "cancelled"].includes(r.status),
    );
    if (active) throw new ApiError(409, "a run is already in progress for this issue");
    // issue #856: a completed prior run that still owns an open MR refuses a fresh
    // run (coded issue_has_open_mr), unless the caller passes force to override.
    if (!force) {
      const openMR = [...state.runs.values()].find(
        (r) =>
          r.repo_id === repoId &&
          r.issue_iid === issueIid &&
          r.kind === "issue" &&
          r.status === "completed" &&
          r.mr_iid != null &&
          r.mr_state === "opened",
      );
      if (openMR) {
        throw new ApiError(
          409,
          `issue #${issueIid} already has open MR !${openMR.mr_iid} — merge or close it, or leave review comments on the MR to iterate, before starting a new run`,
          { code: "issue_has_open_mr", mr_iid: openMR.mr_iid },
        );
      }
    }
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: repoId,
      forge_type: "gitlab",
      mr_web_url: null,
      issue_web_url: null,
      kind: "issue",
      issue_iid: issueIid,
      issue_title: card.title,
      issue_description: "See the linked PRD.",
      title: null,
      resume_of_run_id: null,
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
      branch: null,
      model: null,
      override_subagent_model: false,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      stop_reason: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      pipeline_ref: null,
      pipeline_web_url: null,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      // PRD #122: a freshly synthesised run has no milestones yet — all null, so it
      // renders on the null-fallback path (the iteration badge, no checklist).
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      budget_max_iterations: null,
      budget_wall_seconds: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_select_reason: null,
      anthropic_headroom_pct: null,
      wait_on_limit: false,
      limit_resets_at: null,
      retry_not_before: null,
      limit_wait_count: 0,
      rate_limit_type: null,
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    startNewRun(run.id);
    return delay({ run: { ...run } }, 350);
  },
  createCIFixRun: async (repoId: string, ref: string) => {
    if (!state.boards.get(repoId)) throw new ApiError(404, "repo not found");
    const active = [...state.runs.values()].some(
      (r) => r.repo_id === repoId && r.kind === "ci_fix" && r.pipeline_ref === ref && !["completed", "failed", "cancelled"].includes(r.status),
    );
    if (active) throw new ApiError(409, "an active CI-fix run already exists for this ref");
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: repoId,
      forge_type: "gitlab",
      mr_web_url: null,
      issue_web_url: null,
      kind: "ci_fix",
      issue_iid: null,
      issue_title: `Fix CI: ${ref} pipeline`,
      issue_description: `Diagnose and fix the failed pipeline for \`${ref}\`.`,
      title: null,
      resume_of_run_id: null,
      status: "queued",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: null,
      branch: null,
      model: null,
      override_subagent_model: false,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      stop_reason: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      pipeline_ref: ref,
      pipeline_web_url: `https://gitlab.example.com/myorg/uzi/-/pipelines/4242`,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      // PRD #122: a freshly synthesised run has no milestones yet — all null, so it
      // renders on the null-fallback path (the iteration badge, no checklist).
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      budget_max_iterations: null,
      budget_wall_seconds: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_select_reason: null,
      anthropic_headroom_pct: null,
      wait_on_limit: false,
      limit_resets_at: null,
      retry_not_before: null,
      limit_wait_count: 0,
      rate_limit_type: null,
      claimed_at: null,
      started_at: null,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    startNewRun(run.id);
    return delay({ run: { ...run } }, 350);
  },
  listRuns: async (params?: {
    repoId?: string;
    issueIid?: number;
    // Mirrors the real client's passive-poll flag (#331); the mock does no real
    // fetch, so the marker has no effect here beyond keeping the types compatible.
    passive?: boolean;
  }) =>
    delay({
      runs: listRunsFor()
        // Chat conversations ride runs but have their own page (PRD #39), and judge
        // is a repo-less meta-run — both are excluded here exactly as the real
        // ListRunsForUser excludes them (`kind NOT IN ('chat','judge')`, PRD #239 D4).
        .filter((r) => r.kind !== "chat" && r.kind !== "judge")
        // Caller-scoped, like the real ListRunsForUser: other demo users' runs
        // (mockOtherRunOwners) belong to the admin all-users list only.
        .filter((r) => !(r.id in mockOtherRunOwners))
        .filter((r) => (params?.repoId ? r.repo_id === params.repoId : true))
        .filter((r) => (params?.issueIid != null ? r.issue_iid === params.issueIid : true))
        .map((r) => runListItem(r)),
    }),
  // PRD #40: token/cost usage. Static demo figures — enough to populate the
  // dashboard's "Your usage" and (admin) factory cards + per-user table.
  getUsage: async () =>
    delay({
      lifetime: { input_tokens: 1_610_000, cache_read_tokens: 16_100_000, cache_creation_tokens: 240_000, output_tokens: 710_000, cost_usd: 26.4 },
      last_7_days: { input_tokens: 280_000, cache_read_tokens: 2_800_000, cache_creation_tokens: 40_000, output_tokens: 120_000, cost_usd: 4.55 },
      run_count: 23,
    }),
  getAdminUsage: async () =>
    delay({
      factory: {
        lifetime: { input_tokens: 5_400_000, cache_read_tokens: 53_900_000, cache_creation_tokens: 900_000, output_tokens: 2_400_000, cost_usd: 88.15 },
        last_7_days: { input_tokens: 900_000, cache_read_tokens: 9_100_000, cache_creation_tokens: 120_000, output_tokens: 410_000, cost_usd: 14.9 },
        run_count: 79,
      },
      users: [
        { user_id: "u-maria", email: "maria@example.com", usage: { input_tokens: 2_490_000, cache_read_tokens: 22_400_000, cache_creation_tokens: 400_000, output_tokens: 1_020_000, cost_usd: 37.83 }, run_count: 31 },
        { user_id: "u-vlad", email: "vlad@example.com", usage: { input_tokens: 1_610_000, cache_read_tokens: 16_100_000, cache_creation_tokens: 240_000, output_tokens: 710_000, cost_usd: 26.4 }, run_count: 23 },
        { user_id: "u-andrei", email: "andrei@example.com", usage: { input_tokens: 1_010_000, cache_read_tokens: 13_600_000, cache_creation_tokens: 210_000, output_tokens: 550_000, cost_usd: 19.71 }, run_count: 19 },
        { user_id: "u-dana", email: "dana@example.com", usage: { input_tokens: 290_000, cache_read_tokens: 3_500_000, cache_creation_tokens: 50_000, output_tokens: 120_000, cost_usd: 4.21 }, run_count: 6 },
      ],
      earliest_run: "2026-05-12T09:00:00Z",
    }),
  // ── Claude rate limits (PRD #53) ───────────────────────────────────────────
  // The caller's own reading follows the persona (a demo login as a seeded
  // non-admin shows danger / unavailable / no-token); the admin table covers every
  // row state. Percentages only — no token material ever appears here.
  getMyRateLimits: async () => {
    const me = requireSession();
    return delay({ tokens: mockMyRateLimitsByUser[me.id] ?? mockMyTokenRateLimits }, 60);
  },
  getAdminRateLimits: async () => delay({ users: mockAdminRateLimits.map((u) => ({ ...u })) }, 60),
  getRun: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (id === LIVE_RUN_ID) ensureLive(id);
    // Mirror the server's run-detail read (PRD #37 M4-fix): own_agents is resolved
    // here from the owner's templates (lead stripped), so the plan gate's "My agent
    // templates" card has chips in mock mode without a separate fetch.
    const own_agents = templates
      .filter((t) => !LEAD_NAME_RE.test(t.name))
      .map((t) => ({ name: t.name, description: t.description }));
    return delay({ run: { ...run, own_agents } }, 60);
  },
  // PRD #35: flip this run's usage-limit opt-in. Mirrors the server's guard — the
  // same NEGATIVE predicate the cancel path uses — so a terminal run is refused and
  // `limit_wait` is admitted for free.
  //
  // 🔴 IT MUST NOT TOUCH `status`. A parked run stays parked with its clock intact;
  // this changes what happens at the NEXT limit. A mock that helpfully un-parked the
  // run would teach the demo (and anyone testing against it) the one wrong thing
  // about this control.
  setRunWaitOnLimit: async (id: string, enabled: boolean) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (isTerminalRun(run.status)) throw new ApiError(409, "this run has already finished");
    patchRun(id, { wait_on_limit: enabled });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // PRD #841: set (or clear) a run's per-run MR-review-rework override. Mirrors the
  // server: owner-scoped (the demo caller owns every non-other-user run) and — unlike
  // setRunWaitOnLimit — NO terminal-status guard (D2), because the watcher acts after
  // the run completes, so the toggle stays live on a completed run whose MR is still
  // open. `null` clears back to inherit.
  setRunMrRework: async (id: string, enabled: boolean | null) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    patchRun(id, { mr_rework_enabled: enabled });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // Issue #754: resume an auto-lane run parked at `pool_wait` right now. Mirrors the
  // server: owner-scoped (the demo caller owns every non-other-user run) and
  // pool_wait-ONLY — a 409 ("run is not waiting for a pooled token") on any other
  // status, including a run already resumed to `queued` (so a second click 409s, the
  // idempotent-ish contract the panel's inline "no longer waiting" note relies on).
  // On success the run moves to `queued`, which is what un-parks it.
  resumeRunNow: async (id: string) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.status !== "pool_wait")
      throw new ApiError(409, "run is not waiting for a pooled token");
    patchRun(id, { status: "queued", updated_at: new Date().toISOString() });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  // PRD #320 M6: bump this run to the front of the queue, or clear that override.
  // Mirrors the server: owner-scoped (the demo caller owns every non-other-user run)
  // and QUEUED-ONLY (409 on a non-queued run, exactly like the real endpoint). Clearing
  // the override returns the run to its NATURAL class — "background" for the kinds that
  // demote (judge/self_improve), "normal" otherwise — since the mock has no live rank
  // machinery; the "restored" grace state is a seed, not something undo produces here.
  expediteRun: async (id: string, expedite: boolean) => {
    const run = getRun(id);
    if (!run) throw new ApiError(404, "run not found");
    if (run.status !== "queued") throw new ApiError(409, "run is not queued");
    const natural: RunPriority =
      run.kind === "self_improve" || run.kind === "judge" ? "background" : "normal";
    patchRun(id, { priority: expedite ? "expedited" : natural });
    return delay({ run: { ...getRun(id)! } }, 80);
  },

  ...judgeApi,

  // ── Incidental Findings backlog (PRD #333 M7) ───────────────────────────────
  listFindings: async (bucket: IncidentalFindingBucket = "to_file", repo?: string, run?: string) => {
    const me = requireSession();
    const mine = findings.filter((f) => f.user_id === me.id);
    const byRepo = repo ? mine.filter((f) => f.repo_id === repo) : mine;
    // open_count is the D8 nav-badge count: open coordinates scoped by the ?repo= filter but NOT
    // the bucket or run — so it is stable across a tab switch, exactly like the server meta.
    const openCount = byRepo.filter((f) => f.status === "open").length;
    const byRun = run ? byRepo.filter((f) => f.run_ids.includes(run)) : byRepo;
    const rows = byRun.filter((f) => matchFindingBucket(f.status, bucket)).map(findingDTO);
    const backlog: IncidentalFindingBacklog = {
      bucket,
      repo: repo ?? "",
      run: run ?? "",
      open_count: openCount,
      findings: rows,
    };
    return delay(backlog, 80);
  },
  findingIssueDraft: async (id: string) => {
    const me = requireSession();
    const f = findings.find((x) => x.finding_id === id && x.user_id === me.id);
    if (!f) throw new ApiError(404, "finding not found");
    const draft: IncidentalFindingIssueDraft = {
      title: f.last_title,
      description: f.description_md,
      location: f.location,
      labels: [...f.labels],
      provenance: `Found by a run while working on ${f.repo_path}.`,
    };
    return delay(draft, 80);
  },
  fileFinding: async (id: string, body?: { title?: string; description?: string; labels?: string[] }) => {
    const me = requireSession();
    const f = findings.find((x) => x.finding_id === id && x.user_id === me.id);
    if (!f) throw new ApiError(404, "finding not found");
    // Claim-first: only an `open` coordinate can be filed — a second file is the 409 the stale
    // card / stale backlog row handles gracefully (the guarded claim, D4).
    if (f.status !== "open") throw new ApiError(409, "this finding is already filed or being filed");
    const iid = 900 + (parseInt(id.replace(/\D/g, ""), 10) || 0);
    f.status = "filed";
    f.filed_issue_iid = iid;
    f.resolved_at = new Date().toISOString();
    const res: IncidentalFindingFileResult = {
      issue: {
        iid,
        web_url: `https://gitlab.example.com/${f.repo_path}/-/issues/${iid}`,
        title: body?.title ?? f.last_title,
      },
    };
    return delay(res, 120);
  },
  dismissFinding: async (id: string, reason: "wont_do" | "not_an_issue") => {
    const me = requireSession();
    if (reason !== "wont_do" && reason !== "not_an_issue") {
      throw new ApiError(400, "reason must be wont_do or not_an_issue");
    }
    const f = findings.find((x) => x.finding_id === id && x.user_id === me.id);
    if (!f) throw new ApiError(404, "finding not found");
    if (f.status !== "open") {
      throw new ApiError(409, "cannot dismiss (already filed, being filed, or already dismissed)");
    }
    f.status = "dismissed";
    f.resolved_at = new Date().toISOString();
    return delay({ status: "dismissed", reason }, 80);
  },

  // ── File a forge issue from a recommendation (PRD #68 M4 preview) ────────────
  getIssueDraft: async (runId: string, recId: string) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === runId);
    const rec = review?.recommendations.find((x) => x.id === recId);
    if (!review || !rec) throw new ApiError(404, "recommendation not found");
    return delay({ draft: mockIssueDraft(runId, rec, review) }, 80);
  },
  fileIssue: async (
    runId: string,
    recId: string,
    body: { repo_id: string; title: string; description: string },
  ) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    const review = reviews.find((r) => r.target_run_id === runId);
    const rec = review?.recommendations.find((x) => x.id === recId);
    if (!review || !rec) throw new ApiError(404, "recommendation not found");
    if (review.filed_issues.some((f) => f.category === rec.category && f.target === rec.target)) {
      throw new ApiError(409, "this recommendation already has an issue, or one is being filed");
    }
    const repo = repos.find((r) => r.id === body.repo_id);
    if (!repo) throw new ApiError(404, "repo not found");
    // Demo hook for mock state E (forge rejected): filing against the atlas repo, which
    // the demo treats as write-protected, surfaces the draft-stays-open error path.
    if (repo.path_with_namespace.includes("atlas")) {
      throw new ApiError(502, "could not create the issue on the forge: the forge rejected the request (403)");
    }
    const iid = nextFiledIssueIid++;
    const web_url = `${repo.web_url}/-/issues/${iid}`;
    // Persist the link so a reload shows the filed row (mock C), just like the real API.
    review.filed_issues.push({
      category: rec.category,
      target: rec.target,
      issue_iid: iid,
      issue_url: web_url,
      filed_at: new Date().toISOString(),
    });
    const issue: CreatedIssue = { iid, web_url, title: body.title };
    return delay({ issue }, 200);
  },
  getRunMessages: async (id: string, afterSeq = 0) => {
    const log = state.messages.get(id);
    if (!log) throw new ApiError(404, "run not found");
    return delay({ messages: log.filter((m) => m.seq > afterSeq).map((m) => ({ ...m })) }, 60);
  },
  // PRD #95 steer queue (M2 seeds demo data across delivery states so M3's
  // SteerQueueCard renders every chip). A run with no sample inputs returns an empty
  // queue; a missing run 404s (which the card treats as "no queue", never an error).
  getRunInputs: async (id: string) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    const inputs = (mockRunInputs[id] ?? []).map((i) => ({ ...i }));
    return delay({ inputs }, 60);
  },
  submitRunInput: async (
    id: string,
    kind: RunInputKind,
    body = "",
    selection?: AgentSelectionInput,
    overrideCapabilities?: boolean,
  ) => {
    if (!getRun(id)) throw new ApiError(404, "run not found");
    // PRD #88: the engine returns the refusals the real api answers with (a 409 for an
    // answer to a question that has moved on, a 400 for a malformed body) rather than
    // resolving 200 over a no-op. A mock that swallows a refusal is how a surface ends up
    // built against a laxer contract than the one that ships.
    const rejection = handleInput(id, kind, body);
    if (rejection) throw new ApiError(rejection.status, rejection.message);
    // PRD #37: mirror the selection onto the run row so the mock's read-only
    // post-approval view has something to show.
    if (kind === "approve_plan" && selection) {
      patchRun(id, { agent_source: selection.source, agent_exclusions: selection.exclusions });
    }
    // PRD #84 M4 4c/4d: the "run without the capability" override clears the run's inferred
    // required_capabilities before approving, mirroring the server (the false-positive
    // correction). required_tools/size_class are display-only and untouched.
    if (kind === "approve_plan" && overrideCapabilities) {
      patchRun(id, { required_capabilities: [] });
    }
    return delay({ server_side: false }, 150);
  },

  adminListWorkers: async () => delay({ workers: mockAdminWorkers.map((w) => ({ ...w })) }),
  adminListRuns: async () =>
    delay({
      runs: listRunsFor()
        .filter((r) => r.kind !== "chat")
        .filter((r) => !["completed", "failed", "cancelled"].includes(r.status))
        // Owner attribution: the mock's owner column is mockOtherRunOwners; every
        // other run belongs to the session admin. Before this map existed, EVERY
        // row here was stamped with the session email — the demo factory list was
        // 100% "mine", the exact duplication amendment 2 removes.
        .map((r) => runListItem(r, mockOtherRunOwners[r.id] ?? requireSession().email)),
    }),

  // ── Chat (PRD #39) — real M1 wire ─────────────────────────────────────────
  listChats: async () =>
    delay({
      chats: [...state.runs.values()].filter((r) => r.kind === "chat").map((r) => chatDTO(r)),
      max_turns: CHAT_MAX_TURNS, // the envelope constant, not per-chat
    }),
  createChat: async (message: string) => {
    const now = new Date().toISOString();
    const run: Run = {
      id: nextRunId(),
      repo_id: null,
      forge_type: "",
      mr_web_url: null,
      issue_web_url: null,
      kind: "chat",
      issue_iid: null,
      issue_title: truncateChatTitle(message),
      issue_description: "",
      title: truncateChatTitle(message),
      resume_of_run_id: null,
      status: "running",
      requeue_count: 0,
      iteration_count: 0,
      auto_approve: false,
      worker_id: "w-laptop",
      branch: null,
      model: null,
      override_subagent_model: false,
      mr_iid: null,
      mr_state: null,
      failure_reason: null,
      stop_kind: null,
      stop_reason: null,
      health: "ok",
      health_reason: null,
      health_since: null,
      pipeline_ref: null,
      pipeline_web_url: null,
      fix_verdict: null,
      plan_md: null,
      repo_agents: null,
      agent_source: null,
      agent_exclusions: null,
      own_agents: null,
      // PRD #122: a freshly synthesised run has no milestones yet — all null, so it
      // renders on the null-fallback path (the iteration badge, no checklist).
      milestones: null,
      milestones_completed: null,
      milestones_in_progress: null,
      milestones_candidate: null,
      budget_max_iterations: null,
      budget_wall_seconds: null,
      anthropic_secret_id: null,
      anthropic_secret_label: null,
      anthropic_select_reason: null,
      anthropic_headroom_pct: null,
      wait_on_limit: false,
      limit_resets_at: null,
      retry_not_before: null,
      limit_wait_count: 0,
      rate_limit_type: null,
      claimed_at: now,
      started_at: now,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    state.messages.set(run.id, []);
    appendMessage(run.id, "user_message", null, { text: message });
    scheduleChatReply(run.id, message);
    return delay({ run: { ...run } }, 300);
  },
  sendChatMessage: async (id: string, message: string) => {
    const run = getRun(id);
    if (!run || run.kind !== "chat") throw new ApiError(404, "chat not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "this conversation has ended");
    }
    appendMessage(id, "user_message", null, { text: message });
    scheduleChatReply(id, message);
    return delay({ server_side: false }, 150);
  },
  endChat: async (id: string) => {
    const run = getRun(id);
    if (!run || run.kind !== "chat") throw new ApiError(404, "chat not found");
    patchRun(id, { status: "completed", finished_at: new Date().toISOString() });
    return delay({ server_side: false }, 200);
  },
  continueChat: async (id: string) => {
    const src = getRun(id);
    if (!src || src.kind !== "chat") throw new ApiError(404, "chat not found");
    const now = new Date().toISOString();
    const run: Run = {
      ...src,
      id: nextRunId(),
      status: "running",
      resume_of_run_id: id,
      finished_at: null,
      created_at: now,
      updated_at: now,
    };
    state.runs.set(run.id, run);
    state.messages.set(run.id, []);
    appendMessage(run.id, "status", null, { text: "continuing the conversation on your worker" });
    return delay({ run: { ...run } }, 250);
  },
  confirmProposal: async (chatId: string, proposalId: string) => {
    const p = getProposal(proposalId);
    if (!p || p.run_id !== chatId) throw new ApiError(404, "proposal not found");
    if (p.status !== "pending") throw new ApiError(409, "proposal already resolved");
    // Mark resolved (a re-confirm 409s); the confirm response is the created issue.
    putProposal({ ...p, status: "confirmed" });
    const iid = 200 + Math.floor(Math.random() * 800);
    const issue: CreatedIssue = {
      iid,
      web_url: `https://gitlab.example.com/${p.repo_path ?? "grp/proj"}/-/issues/${iid}`,
      title: p.title,
    };
    // The created-issue link is appended to the conversation (Decision 8).
    appendMessage(chatId, "text", "chat", { text: `Created issue #${iid}: ${issue.web_url}` });
    return delay({ issue }, 350);
  },
  dismissProposal: async (chatId: string, proposalId: string) => {
    const p = getProposal(proposalId);
    if (!p || p.run_id !== chatId) throw new ApiError(404, "proposal not found");
    putProposal({ ...p, status: "dismissed" });
    appendMessage(chatId, "status", null, { text: "proposal dismissed — nothing written to the forge" });
    return delay(null, 200); // 204 No Content
  },
  startRunFromChat: async (repoPath: string, _issueIid: number) => {
    // PRD #191 M5: start a run from a chat's start-run card. Repo paths aren't modelled
    // in the mock state, so it resolves the first seeded board+card and mints a queued
    // issue run via the same path as createRun; the real endpoint applies the
    // PRD/ownership gate keyed by repo_path.
    const repoId = [...state.boards.keys()][0];
    const card = repoId ? state.boards.get(repoId)?.cards[0] : undefined;
    if (!repoId || !card) throw new ApiError(404, `repo ${repoPath} not found`);
    return mockApi.createRun(repoId, card.iid);
  },

  // PRD #322 M1: cancel a run from a chat's cancel card. run_id is untrusted; the real
  // endpoint re-resolves ownership/terminality server-side via SubmitInput(cancel), so
  // the mock reproduces its refusals — a missing run is 404, an already-terminal one 409
  // — rather than resolving 202 over a no-op.
  cancelRunFromChat: async (runId: string) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "run has already finished");
    }
    handleInput(runId, "cancel", "");
    return delay({ server_side: true }, 150);
  },

  // PRD #322 M3: steer a run from a chat's steer card with a human-edited follow-up.
  // run_id + message are untrusted; the real endpoint re-resolves ownership/terminality
  // via SubmitInput(follow_up), which additionally refuses a CHAT run (issue-runs-only),
  // so the mock reproduces its refusals — a missing run is 404, a terminal one 409, and a
  // chat-run target 409 — a follow_up on an issue run succeeds.
  steerRunFromChat: async (runId: string, message: string) => {
    const run = getRun(runId);
    if (!run) throw new ApiError(404, "run not found");
    if (["completed", "failed", "cancelled"].includes(run.status)) {
      throw new ApiError(409, "run has already finished");
    }
    if (run.kind === "chat") {
      throw new ApiError(409, "steering applies to issue runs, not chats");
    }
    handleInput(runId, "follow_up", message);
    return delay({ server_side: true }, 150);
  },

  ...cliTokensApi,

  ...memoryApi,

  // ── Scheduled runs (PRD #241) ──────────────────────────────────────────────
  listSchedules: async () => {
    requireSession();
    return delay(schedules.map(scheduleDTO));
  },
  createSchedule: async (repoId: string, input: ScheduleInput) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    assertGuidanceWithinCap(input.guidance);
    const target = input.target ?? "issue";
    const timing = input.timing ?? "recurring";
    const now = new Date().toISOString();
    const s: Schedule = {
      id: nextScheduleId(),
      repo_id: repoId,
      repo_path: repo.path_with_namespace,
      target,
      issue_iid: target === "issue" ? (input.issue_iid ?? null) : null,
      labels: target === "sweep" && input.labels && input.labels.length ? input.labels : null,
      prompt: target === "prompt" ? (input.prompt ?? "") : "",
      timing,
      cron_expr: timing === "recurring" ? (input.cron_expr ?? "") : "",
      run_at: timing === "once" ? (input.run_at ?? null) : null,
      timezone: input.timezone || "UTC",
      next_fire_at: null,
      last_fired_at: null,
      last_fire: null,
      auto_approve: input.auto_approve ?? true,
      wait_on_limit: input.wait_on_limit ?? true,
      // PRD #841: a create stamps the explicit tri-state override, or leaves it null = inherit.
      mr_rework_enabled: input.mr_rework_enabled ?? null,
      // Sweep-only; new sweeps default to 10 (mirrors the server), unlimited otherwise.
      max_issues: target === "sweep" ? (input.max_issues ?? 10) : null,
      // Guidance on issue/sweep only; null (none) for prompt (re-nulled per target).
      guidance: target === "issue" || target === "sweep" ? (input.guidance ?? null) : null,
      // Baked catalog guidance is a default-sweep-only field (issue #675); null for a user row.
      baked_guidance: null,
      // Model applies to ALL targets (unlike guidance); null = inherit.
      model: input.model ?? null,
      // PRD #305: omitted ≡ false (replace-semantics), default off.
      override_subagent_model: input.override_subagent_model ?? false,
      enabled: input.enabled ?? true,
      status: "active",
      // A create always makes a user-authored row (PRD #589): defaults are born only
      // via enableCatalogSchedule, and a clone via cloneSchedule.
      origin: "user",
      catalog_slug: null,
      customized: false,
      // A bare create sends no group id (standalone). Multi-repo fan-out (PRD #636 M2)
      // stamps a shared id via the create input; a single-repo create stays NULL here.
      sibling_group_id: null,
      created_at: now,
      updated_at: now,
      next_fires: [],
    };
    schedules = [s, ...schedules];
    return delay(scheduleDTO(s), 250);
  },
  getSchedule: async (id: string) => {
    requireSession();
    const s = schedules.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "schedule not found");
    return delay(scheduleDTO(s));
  },
  updateSchedule: async (id: string, input: ScheduleInput) => {
    requireSession();
    const cur = schedules.find((x) => x.id === id);
    if (!cur) throw new ApiError(404, "schedule not found");
    // A catalog default is catalog-owned (PRD #589): the server's patchDefaultScheduleConfig
    // 400s ANY default patch whose body carries a catalog-owned field. Mirror that here so
    // the mock and the server agree — the drift that hid the buildDefaultInput `timing` bug.
    // The rejected set is target/prompt/labels/timing/repo_id/issue_iid (run_at too, but the
    // modal never sends it on a default). Guidance is the exception for a PROMPT default
    // (issue #662) and a SWEEP default (issue #675): it is owner-editable there (overlaid on
    // the catalog prompt/guidance at fire time), so allow it for prompt+sweep defaults and
    // keep rejecting it for issue/self_improve defaults. The 400 message mirrors the server's
    // per-target locked-set string. Only cron/tz/model/flags/max_issues (+prompt/sweep-default
    // guidance) edit.
    if (cur.origin === "default") {
      const guidanceEditable = cur.target === "prompt" || cur.target === "sweep";
      if (
        input.target !== undefined ||
        input.prompt !== undefined ||
        input.labels !== undefined ||
        (!guidanceEditable && input.guidance !== undefined) ||
        input.timing !== undefined ||
        (input.repo_id !== undefined && input.repo_id !== "") ||
        input.issue_iid !== undefined ||
        input.run_at !== undefined
      ) {
        throw new ApiError(
          400,
          guidanceEditable
            ? "a default schedule's prompt, labels, target, timing and repo are catalog-owned and cannot be edited"
            : "a default schedule's prompt, labels, guidance, target, timing and repo are catalog-owned and cannot be edited",
        );
      }
    }
    assertGuidanceWithinCap(input.guidance);
    const m: Schedule = { ...cur };
    if (input.target !== undefined) m.target = input.target;
    if (input.timing !== undefined) m.timing = input.timing;
    if (input.issue_iid !== undefined) m.issue_iid = input.issue_iid;
    if (input.labels !== undefined) m.labels = input.labels.length ? input.labels : null;
    if (input.prompt !== undefined) m.prompt = input.prompt;
    if (input.cron_expr !== undefined) m.cron_expr = input.cron_expr;
    if (input.run_at !== undefined) m.run_at = input.run_at;
    if (input.timezone !== undefined) m.timezone = input.timezone;
    if (input.auto_approve !== undefined) m.auto_approve = input.auto_approve;
    if (input.wait_on_limit !== undefined) m.wait_on_limit = input.wait_on_limit;
    // PRD #841: replace-semantics — apply when present (explicit null clears to inherit).
    if (input.mr_rework_enabled !== undefined) m.mr_rework_enabled = input.mr_rework_enabled;
    // Replace-semantics: apply when the key is present (explicit null = unlimited).
    if (input.max_issues !== undefined) m.max_issues = input.max_issues;
    // Same replace-semantics for guidance (explicit null/"" clears to none).
    if (input.guidance !== undefined) m.guidance = input.guidance;
    // Model applies to all targets, so it is not re-nulled per target below.
    if (input.model !== undefined) m.model = input.model;
    // PRD #305: replace-semantics, applied when the key is present.
    if (input.override_subagent_model !== undefined)
      m.override_subagent_model = input.override_subagent_model;
    if (input.enabled !== undefined) m.enabled = input.enabled;
    // PRD #344 Feature A: a non-empty repo_id that differs from the current one repoints the
    // schedule. Mirror the server: an issue-target schedule is rejected (422, D4 restrict);
    // an unknown/unowned repo is a 404; otherwise move repo_id and refresh repo_path.
    if (input.repo_id !== undefined && input.repo_id !== "" && input.repo_id !== cur.repo_id) {
      if (m.target === "issue")
        throw new ApiError(422, "repointing an issue-target schedule is not supported; delete and recreate it against the new repo");
      const repo = repos.find((r) => r.id === input.repo_id);
      if (!repo) throw new ApiError(404, "repo not found");
      m.repo_id = repo.id;
      m.repo_path = repo.path_with_namespace;
    }
    // Re-null the fields the (possibly changed) target/timing does not use, so the
    // stored shape matches the DB's field-presence CHECK.
    m.issue_iid = m.target === "issue" ? m.issue_iid : null;
    m.labels = m.target === "sweep" ? m.labels : null;
    m.max_issues = m.target === "sweep" ? m.max_issues : null;
    // A prompt DEFAULT keeps its owner guidance (issue #662); a USER prompt schedule still
    // nulls it (guidance is issue/sweep-only for user rows, catalog-owned/baked otherwise).
    m.guidance =
      m.target === "issue" || m.target === "sweep" || (m.origin === "default" && m.target === "prompt")
        ? m.guidance
        : null;
    m.prompt = m.target === "prompt" ? m.prompt : "";
    m.cron_expr = m.timing === "recurring" ? m.cron_expr : "";
    m.run_at = m.timing === "once" ? m.run_at : null;
    // A default row that edits an editable field away from its catalog values reads as
    // "customized" (PRD #589 M2), which lights the indicator and Reset. Only the enable
    // pause-flip leaves it untouched (a bare { enabled } PATCH changes nothing else).
    if (m.origin === "default" && m.catalog_slug) {
      const entry = catalogBySlug(m.catalog_slug);
      if (entry) {
        m.customized =
          m.cron_expr !== entry.cron ||
          m.timezone !== entry.timezone ||
          (m.model ?? "") !== entry.model ||
          m.auto_approve !== entry.auto_approve ||
          m.wait_on_limit !== entry.wait_on_limit ||
          // mr_rework_enabled (PRD #841): the catalog baseline is inherit (null), so ANY
          // explicit override flips customized — mirroring the server's `if mrRework.Valid`
          // in defaultEditableDiverges (api/internal/handler/schedules.go).
          m.mr_rework_enabled != null ||
          (m.target === "sweep" && (m.max_issues ?? 0) !== entry.max_issues) ||
          // Owner guidance-overlay divergence for a prompt default (issue #662) or a sweep
          // default (issue #675): the OVERLAY (m.guidance) is null until the owner sets one,
          // so any non-empty overlay flips customized. The baked catalog guidance is NOT
          // compared (only the overlay's presence matters), matching the server.
          ((m.target === "prompt" || m.target === "sweep") && (m.guidance ?? "") !== "");
      }
    }
    m.updated_at = new Date().toISOString();
    schedules = schedules.map((x) => (x.id === id ? m : x));
    return delay(scheduleDTO(m));
  },
  deleteSchedule: async (id: string) => {
    requireSession();
    const s = schedules.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "schedule not found");
    schedules = schedules.filter((x) => x.id !== id);
    return delay(null);
  },
  runScheduleNow: async (id: string) => {
    requireSession();
    const s = schedules.find((x) => x.id === id);
    if (!s) throw new ApiError(404, "schedule not found");
    // The demo does not spin up a live worker run; it reports one fired, matching
    // the seam's typical single-run outcome for a pinned issue / prompt.
    const runId = nextRunId();
    return delay(
      {
        created: 1,
        run_ids: [runId],
        matched: 1,
        capped: false,
        started: [{ issue_iid: s.issue_iid, run_id: runId, title: s.prompt || `#${s.issue_iid ?? ""}` }],
        skips: [],
      },
      250,
    );
  },
  previewSchedule: async (input: SchedulePreviewInput) => {
    requireSession();
    const n = Math.min(Math.max(input.n ?? 3, 1), schedulePreviewCap);
    if (input.timing === "once") {
      const fires = input.run_at && new Date(input.run_at).getTime() > Date.now()
        ? [new Date(input.run_at).toISOString()]
        : input.run_at
          ? [new Date(input.run_at).toISOString()]
          : [];
      return delay({ fires }, 80);
    }
    return delay({ fires: mockScheduleFires(input.cron_expr ?? "", n) }, 80);
  },

  // ── Default scheduled jobs (PRD #589) ──────────────────────────────────────
  listScheduleCatalog: async () => {
    requireSession();
    // Derive enablements from the live default rows so enable/reset/clone stay in
    // sync (presence of a default row for (repo, slug) IS the enablement).
    const enablements = schedules
      .filter((s) => s.origin === "default" && s.catalog_slug)
      .map((s) => ({
        repo_id: s.repo_id,
        slug: s.catalog_slug as string,
        schedule_id: s.id,
        enabled: s.enabled,
      }));
    return delay({ entries: scheduleCatalog.map((e) => ({ ...e, labels: e.labels ? [...e.labels] : null })), enablements });
  },
  enableCatalogSchedule: async (repoId: string, slug: string, timezone?: string) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const entry = catalogBySlug(slug);
    if (!entry) throw new ApiError(404, "catalog entry not found");
    // Idempotent (server 200): an already-materialized (repo, slug) — even paused —
    // returns its existing row untouched, never a fresh enable. Mirroring the server's
    // ON CONFLICT DO NOTHING, a re-enable ignores any timezone override (issue #660).
    const existing = schedules.find(
      (s) => s.origin === "default" && s.catalog_slug === slug && s.repo_id === repoId,
    );
    if (existing) return delay(scheduleDTO(existing));
    const s = materializeDefault(entry, repoId, nextScheduleId());
    // On the fresh-materialize path only, an optional detected browser timezone (issue
    // #660) overrides the catalog zone; an empty/absent tz keeps the catalog zone. Mirror
    // the production handler: trim, and reject an invalid IANA name (Intl throws a
    // RangeError on both a bogus name and the "Local" sentinel) with a 400.
    const tz = timezone?.trim();
    if (tz) {
      try {
        Intl.DateTimeFormat(undefined, { timeZone: tz });
      } catch {
        throw new ApiError(400, "invalid timezone");
      }
      s.timezone = tz;
    }
    schedules = [s, ...schedules];
    return delay(scheduleDTO(s), 200);
  },
  resetSchedule: async (id: string) => {
    requireSession();
    const cur = schedules.find((x) => x.id === id);
    if (!cur) throw new ApiError(404, "schedule not found");
    if (cur.origin !== "default" || !cur.catalog_slug)
      throw new ApiError(422, "only a default schedule can be reset");
    const entry = catalogBySlug(cur.catalog_slug);
    if (!entry) throw new ApiError(404, "catalog entry not found");
    // Restore the editable fields to the catalog values, keep the pause flag + repo,
    // and clear customized. Prompt/labels/guidance are already the resolved catalog
    // values on a default row, so re-materializing is exact.
    const restored = materializeDefault(entry, cur.repo_id, cur.id, {
      enabled: cur.enabled,
      last_fired_at: cur.last_fired_at,
      last_fire: cur.last_fire,
      created_at: cur.created_at,
    });
    schedules = schedules.map((x) => (x.id === id ? restored : x));
    return delay(scheduleDTO(restored));
  },
  cloneSchedule: async (id: string, repoId?: string) => {
    requireSession();
    const src = schedules.find((x) => x.id === id);
    if (!src) throw new ApiError(404, "schedule not found");
    const targetRepoId = repoId && repoId.trim() !== "" ? repoId : src.repo_id;
    const repo = repos.find((r) => r.id === targetRepoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const now = new Date().toISOString();
    // A user-owned copy with the source's resolved fields baked in and catalog_slug
    // cleared, so its prompt/labels/guidance become editable (origin='user').
    const clone: Schedule = {
      ...src,
      id: nextScheduleId(),
      repo_id: repo.id,
      repo_path: repo.path_with_namespace,
      origin: "user",
      catalog_slug: null,
      customized: false,
      enabled: true,
      status: "active",
      last_fired_at: null,
      last_fire: null,
      created_at: now,
      updated_at: now,
      next_fires: [],
      // A clone is always a user row, which never carries the read-only baked catalog
      // guidance (issue #675). For a SWEEP default, fold the baked catalog guidance into
      // the editable guidance and discard the owner overlay, mirroring the server clone.
      guidance:
        src.origin === "default" && src.target === "sweep" ? src.baked_guidance : src.guidance,
      baked_guidance: null,
    };
    schedules = [clone, ...schedules];
    return delay(scheduleDTO(clone), 200);
  },
  addScheduleRepo: async (id: string, repoId: string) => {
    requireSession();
    // Owner-scoped, origin='user' sources only (PRD #636 Decision 5), matching the server
    // (handler/schedules.go AddScheduleRepo): a foreign/absent source 404s, but a non-user
    // source is a 409 ("only a custom schedule can add a repo; clone a default first"),
    // mirroring ResetSchedule's origin-mismatch conflict — NOT a 404.
    const src = schedules.find((x) => x.id === id);
    if (!src) throw new ApiError(404, "schedule not found");
    if (src.origin !== "user")
      throw new ApiError(409, "only a custom schedule can add a repo; clone a default schedule first");
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    // Coalesce a group id onto the source (the server does this under a row lock so two
    // racing calls settle on one id; a single-writer mock just assigns it once).
    const groupId = src.sibling_group_id ?? crypto.randomUUID();
    if (!src.sibling_group_id) {
      schedules = schedules.map((x) => (x.id === src.id ? { ...x, sibling_group_id: groupId } : x));
    }
    // The partial unique index (sibling_group_id, repo_id) rejects a second sibling on a
    // repo already in the group — an idempotent-safe 409, no duplicate row created.
    if (schedules.some((x) => x.sibling_group_id === groupId && x.repo_id === repoId)) {
      throw new ApiError(409, "that schedule is already on that repo");
    }
    const now = new Date().toISOString();
    // Copy the source's current config onto the new repo as an independent sibling. The
    // server copies the SOURCE's `enabled` (cur.Enabled) — so a repo added from a PAUSED
    // custom schedule yields a PAUSED sibling — while `status` takes the CreateRunSchedule
    // default ("active"), the same fresh-row status the clone/materialize paths use.
    const sibling: Schedule = {
      ...src,
      id: nextScheduleId(),
      repo_id: repo.id,
      repo_path: repo.path_with_namespace,
      sibling_group_id: groupId,
      enabled: src.enabled,
      status: "active",
      last_fired_at: null,
      last_fire: null,
      created_at: now,
      updated_at: now,
      next_fires: [],
    };
    schedules = [sibling, ...schedules];
    return delay(scheduleDTO(sibling), 200);
  },
  checkRepoLabels: async (repoId: string, labels: string[]) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const have = repoLabels[repoId] ?? [];
    const missing = [...new Set(labels.map((l) => l.trim()).filter(Boolean))].filter(
      (l) => !have.includes(l),
    );
    return delay({ missing }, 150);
  },
  ensureRepoLabels: async (repoId: string, labels: string[]) => {
    requireSession();
    const repo = repos.find((r) => r.id === repoId);
    if (!repo) throw new ApiError(404, "repo not found");
    const ensured = [...new Set(labels.map((l) => l.trim()).filter(Boolean))];
    const have = repoLabels[repoId] ?? (repoLabels[repoId] = []);
    for (const l of ensured) if (!have.includes(l)) have.push(l);
    return delay({ ensured }, 200);
  },
};

const CHAT_MAX_TURNS = 50;

// chatDTO derives the chatListDTO shape from a chat run + its message log: the
// title (the run's chat title, else derived from the first user turn), the
// user-turn count, and last activity from the newest message (PRD #39 wire). No
// max_turns here — that rides the list envelope as an instance constant.
function chatDTO(run: Run): Chat {
  const msgs: RunMessage[] = state.messages.get(run.id) ?? [];
  const firstUser = msgs.find((m) => m.kind === "user_message");
  const derived = (firstUser?.payload as { text?: string } | null)?.text;
  const title = run.title ?? (derived ? truncateChatTitle(derived) : run.issue_title || null);
  const turnCount = msgs.reduce((n, m) => (m.kind === "user_message" ? n + 1 : n), 0);
  return {
    id: run.id,
    title,
    status: run.status,
    turn_count: turnCount,
    resume_of_run_id: run.resume_of_run_id,
    last_message_at: msgs[msgs.length - 1]?.created_at ?? null,
    created_at: run.created_at,
    updated_at: run.updated_at,
  };
}

function truncateChatTitle(s: string): string {
  const t = s.trim().replace(/\s+/g, " ");
  return t.length > 60 ? `${t.slice(0, 59)}…` : t;
}

// A run patch helper other mock surfaces can use (kept for symmetry/tests).
export { patchRun };
// sameContent is imported by lib/agentTemplateDriftContract.test.ts through the index.
export { sameContent } from "./agents";
// Judge-backlog internals imported by the fidelity/truncation fixtures through the index.
export {
  bucketOf,
  filterGroups,
  groupJudgeRecommendations,
  capBacklogRows,
  MOCK_BACKLOG_MAX_ROWS,
  type JudgeBacklogRow,
} from "./judge";
