// The two "my own limits" surfaces of PRD #53: the Settings → "Claude limits"
// card and the sidebar-footer micro-meters. Both read GET /me/rate-limits (the
// token never leaves the api — the SPA only ever sees percentages) and reuse the
// shared MeterTrack + toneFor thresholds. The admin table is a separate page.

import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { isShownInSidebar, onSidebarTokensChanged } from "../lib/sidebarTokens";
import { api, type RateLimitWindow, type TokenRateLimits } from "../lib/api";
import { useAsyncData } from "../lib/useAsyncData";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import {
  formatAgo,
  formatCountdown,
  formatResetLabel,
  rowForecast,
  statusBadge,
  useNow,
  WINDOW_DURATION,
  worstTokenReading,
  worstWindow,
  type PaceForecast,
} from "../lib/rateLimits";
import { Badge, Card, SectionTitle } from "./ui";
import { MeterTrack, toneFor, type MeterTone } from "./Meter";
import { RateLimitForecastMeter } from "./RateLimitForecast";

// useMyRateLimits polls GET /me/rate-limits while the tab is visible. A failed
// fetch keeps the last reading (never blanks the meters); the surfaces decide what
// to render per token. intervalMs differs per surface — the data changes at most
// once per server poll interval, so a page's usual 10s cadence would be pure
// amplification.
//
// Since PRD #104 M5 the endpoint returns ONE READING PER TOKEN, so `tokens` is an
// array. An empty array means the user holds no credential at all — the surfaces
// render nothing rather than a "no token" meter, which is what they already did
// for the old no_token status.
export function useMyRateLimits(intervalMs: number): {
  tokens: TokenRateLimits[] | null;
  loading: boolean;
} {
  // Reimplemented over useAsyncData (PRD #950 M3, Tier C) with the exact old
  // contract: `data` null-initial matches the old `tokens` null, and NO error is
  // ever surfaced (the old catch only cleared loading) — so `error` is not
  // destructured. The poll is composed on the hook's reload, not absorbed.
  const { data, loading, reload } = useAsyncData(
    async () => (await api.getMyRateLimits()).tokens,
    [],
  );
  usePollWhileVisible(reload, intervalMs);
  return { tokens: data, loading };
}

const CARD_BLURB =
  "Live utilization of each token's two rate-limit windows. Anthropic meters these per credential, so every token has its own pair. Runs queue when a window is exhausted and resume after it resets.";

function SettingsWindowRow({
  label,
  win,
  now,
  forecast,
  dim = false,
}: {
  label: string;
  win: RateLimitWindow;
  now: number;
  forecast: PaceForecast;
  dim?: boolean;
}) {
  const countdown = formatCountdown(win.resets_at, now);
  return (
    <div className="mt-4 first:mt-0">
      <div className="flex items-baseline justify-between gap-4 text-sm">
        <span className="font-medium text-fg">{label}</span>
        <span className="tabular-nums text-muted">
          <b className="font-semibold text-fg">{win.pct}%</b>
          {countdown && <span> · resets in {countdown}</span>}
        </span>
      </div>
      {/* dim greys the bar on a stale reading (Decision 3), matching the sidebar
          micro-meters and the admin table. The forecast ghost/» draws only when the
          anchored projection heads past the safe band; otherwise this is the plain
          bar (PRD #309 D3, #310). */}
      <RateLimitForecastMeter
        className="mt-1.5 h-2"
        label={label}
        pct={win.pct}
        valueText={`${win.pct}%${countdown ? `, resets in ${countdown}` : ""}`}
        forecast={forecast}
        dim={dim}
      />
    </div>
  );
}

function UnavailableWindowRow({ label }: { label: string }) {
  return (
    <div className="mt-4 first:mt-0">
      <div className="flex items-baseline justify-between gap-4 text-sm">
        <span className="font-medium text-muted">{label}</span>
        <span className="text-faint">no reading yet</span>
      </div>
      <MeterTrack className="mt-1.5 h-2" label={label} fillPct={0} valueText="no reading yet" dim />
    </div>
  );
}

// TokenMeters renders ONE token's meter pair plus its status line. `showLabel`
// names the credential — omitted when the user holds a single token, because
// "default" above a lone meter pair is noise; with several it is the only thing
// telling the pairs apart.
function TokenMeters({
  token,
  now,
  showLabel,
}: {
  token: TokenRateLimits;
  now: number;
  showLabel: boolean;
}) {
  const { limits } = token;
  const heading = showLabel ? (
    <div className="flex items-center gap-2">
      <span className="text-sm font-medium text-fg">{token.label}</span>
      {token.is_default && <Badge tone="neutral">default</Badge>}
    </div>
  ) : null;

  if (limits.status !== "ok") {
    // A token with no reading yet (fresh save, polling off, refused credential).
    return (
      <div className="border-t border-edge pt-4 first:border-t-0 first:pt-0">
        {heading}
        <div className={showLabel ? "mt-2" : ""}>
          <UnavailableWindowRow label="5-hour window" />
          <UnavailableWindowRow label="7-day window" />
        </div>
        <div className="mt-3 flex items-center gap-2 text-xs text-faint">
          <Badge tone="neutral">No reading yet</Badge>
          <span>a reading appears within a few minutes of saving your token</span>
        </div>
      </div>
    );
  }

  // /me/rate-limits carries no vault_locked field, so vaultLocked is false — a
  // stale self reading reads "stale" (the single-source-of-truth wording).
  const badge = statusBadge(limits, false);
  // The mock's absolute "resets <Day HH:MM>" line under the token name (PRD #310 M3),
  // from the 7-day reset — the weekly quota users plan around. Omitted when the 7-day
  // resets_at is null (formatResetLabel returns null).
  const resetLabel = formatResetLabel(limits.seven_day.resets_at);
  return (
    <div className="border-t border-edge pt-4 first:border-t-0 first:pt-0">
      {heading}
      {resetLabel && <div className="mt-0.5 text-xs text-faint">{resetLabel}</div>}
      <div className={showLabel ? "mt-2" : ""}>
        <SettingsWindowRow
          label="5-hour window"
          win={limits.five_hour}
          now={now}
          dim={limits.stale}
          forecast={rowForecast(limits.stale, limits.five_hour.pct, limits.five_hour.resets_at, WINDOW_DURATION["5h"], now, limits.source)}
        />
        <SettingsWindowRow
          label="7-day window"
          win={limits.seven_day}
          now={now}
          dim={limits.stale}
          forecast={rowForecast(limits.stale, limits.seven_day.pct, limits.seven_day.resets_at, WINDOW_DURATION["7d"], now, limits.source)}
        />
      </div>
      {/* text-muted (not text-faint) so the "updated Xm ago" timestamp — data
          this page leans on — clears WCAG AA 4.5:1 at 12px (web-ux finding). A
          limit_report reading (PRD #217 D6) is a park-time inference, not a poll:
          the extra badge + phrase disclose that the 100% bar was recorded when the
          token hit its limit, AFTER the "updated Xm ago" timestamp (D3), so the
          timestamp does not read as the age of the 100%. flex-wrap so the second
          badge never overflows a narrow card; the other sources render one line as
          before. */}
      <div className="mt-3 flex flex-wrap items-center gap-2 text-xs text-muted">
        <Badge tone={badge.tone} dot={badge.dot}>{badge.label}</Badge>
        {limits.source === "limit_report" && <Badge tone="neutral">Recorded at usage limit</Badge>}
        <span>
          updated {formatAgo(limits.synced_at, now)}
          {limits.source === "limit_report"
            ? " · recorded when this token hit its usage limit, after the last poll"
            : limits.stale
              ? " · reading is stale (vault locked or polling off)"
              : " · refreshes every few minutes"}
        </span>
      </div>
    </div>
  );
}

// RateLimitCard is the Settings → Claude limits card. Hidden entirely when the
// user holds no token (and while the first read is in flight, to avoid a flash
// under the token card). Since PRD #104 it renders ONE METER PAIR PER TOKEN,
// default first (the server orders them), each named by its label.
export function RateLimitCard() {
  const { tokens, loading } = useMyRateLimits(60_000);
  const now = useNow();

  if (loading || !tokens || tokens.length === 0) return null;
  const showLabels = tokens.length > 1;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Claude limits</SectionTitle>
        <p className="mt-2 text-sm text-muted">{CARD_BLURB}</p>
      </div>
      <div className="space-y-4">
        {tokens.map((t) => (
          <TokenMeters key={t.secret_id} token={t} now={now} showLabel={showLabels} />
        ))}
      </div>
    </Card>
  );
}

// Tone severity ordering so the announcer fires only on a *step up* (Decision 3).
const TONE_RANK: Record<MeterTone, number> = { ok: 0, warn: 1, danger: 2 };

// RateLimitAnnouncer is the app-wide screen-reader alert (PRD #54, Decisions 3 &
// 4): a visually-hidden aria-live region mounted once by AppShell (not scoped to
// the Settings card) that speaks a message ONLY when the worst window's tone
// steps up (ok→warn, warn→danger, ok→danger). A ref holds the last announced
// tone; step-downs update it silently (no "you're fine now" chatter), the first
// read seeds it without speaking, and stale readings never transition it (a
// frozen 3h-old "96%" must not announce as if fresh). It runs its own
// useMyRateLimits(60s) read so it is independent of whether the meters are on
// screen. useNow keeps it re-rendering on the 30s clock like the card; the
// effect keys off the reading, so a bare clock tick never re-announces.
//
// Since PRD #115 the danger TONE steps at 85, so crossing 95 no longer changes
// the tone and would otherwise go unannounced. A dedicated ≥95 "nearly out"
// emergency announcement (Decision 3) fires the moment the worst window crosses
// 95 — a distinct, more urgent message than the tone step-up — so the badge's
// 95% "nearly out" moment keeps a screen-reader signal. A second ref tracks the
// critical state; dropping back below 95 clears it silently.
export function RateLimitAnnouncer() {
  const { tokens } = useMyRateLimits(60_000);
  useNow();
  const lastTone = useRef<MeterTone | null>(null);
  const wasCritical = useRef<boolean | null>(null);
  const [message, setMessage] = useState("");

  useEffect(() => {
    if (!tokens || tokens.length === 0) return;
    // Announce the user's MOST URGENT token (PRD #104): with several credentials
    // the one nearest a wall is the one worth interrupting for, and naming it is
    // what makes the announcement actionable — "at 96%" is useless when the user
    // holds three tokens and cannot tell which is throttling.
    const worstToken = worstTokenReading(tokens);
    const limits = worstToken?.limits;
    // Only a live, non-stale reading transitions the refs (Decision 3).
    if (!limits || limits.status !== "ok" || limits.stale) return;
    const worst = worstWindow(limits);
    const tone = toneFor(worst.pct);
    const critical = worst.pct >= 95;
    const prev = lastTone.current;
    const prevCritical = wasCritical.current;
    lastTone.current = tone;
    wasCritical.current = critical;
    // First read seeds BOTH refs (prev === null) and never announces.
    if (prev === null) return;
    const countdown = formatCountdown(worst.resets_at);
    // The label is named only when the user holds more than one token, so a
    // single-token user hears exactly what they heard before this feature.
    const which = tokens.length > 1 && worstToken ? `${worstToken.label} ` : "";
    // ≥95 emergency takes priority: the tone no longer steps at 95, so this is the
    // only signal for the badge's "nearly out" moment. A distinct wording marks it
    // apart from the tone step-up. Dropping below 95 clears the ref silently.
    if (critical && !prevCritical) {
      setMessage(
        `${which}${worst.label} window nearly out at ${worst.pct}%` +
          (countdown ? `, resets in ${countdown}` : ""),
      );
      return;
    }
    // Otherwise announce only when the tone rose above the last seen one.
    if (TONE_RANK[tone] <= TONE_RANK[prev]) return;
    setMessage(
      `${which}${worst.label} window at ${worst.pct}%` + (countdown ? `, resets in ${countdown}` : ""),
    );
  }, [tokens]);

  return (
    <div className="sr-only" aria-live="polite" role="status">
      {message}
    </div>
  );
}

// MicroRow is one window's 5px forecast bar, showing its percentage and folding
// the "resets in <countdown>" text into the bar's forecast tooltip.
function MicroRow({
  label,
  win,
  dim,
  forecast,
  now,
}: {
  label: string;
  win: RateLimitWindow;
  dim: boolean;
  forecast: PaceForecast;
  now: number;
}) {
  const countdown = formatCountdown(win.resets_at, now);
  return (
    <div className="grid grid-cols-[1.4rem_1fr_2.6rem] items-center gap-2 text-[11px]">
      <span className="font-mono text-faint">{label}</span>
      <RateLimitForecastMeter
        className="h-[5px]"
        label={`${label} window`}
        pct={win.pct}
        valueText={`${win.pct}%${countdown ? `, resets in ${countdown}` : ""}`}
        forecast={forecast}
        dim={dim}
      />
      <span className="text-right font-mono tabular-nums text-muted">{win.pct}%</span>
    </div>
  );
}

// TokenMicroMeters is one token's pair of 5px bars, with the credential named
// above them only when the user holds several.
function TokenMicroMeters({
  token,
  showLabel,
  now,
}: {
  token: TokenRateLimits;
  showLabel: boolean;
  now: number;
}) {
  const { limits } = token;
  if (limits.status !== "ok") return null;
  const c5 = formatCountdown(limits.five_hour.resets_at);
  const c7 = formatCountdown(limits.seven_day.resets_at);
  const title = [
    showLabel && token.label,
    c5 && `5h resets in ${c5}`,
    c7 && `7d resets in ${c7}`,
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <div className="space-y-1.5" title={title || undefined}>
      {showLabel && (
        <div className="truncate text-[10px] font-medium uppercase tracking-wide text-faint">
          {token.label}
        </div>
      )}
      <MicroRow
        label="5h"
        win={limits.five_hour}
        dim={limits.stale}
        forecast={rowForecast(limits.stale, limits.five_hour.pct, limits.five_hour.resets_at, WINDOW_DURATION["5h"], now, limits.source)}
        now={now}
      />
      <MicroRow
        label="7d"
        win={limits.seven_day}
        dim={limits.stale}
        forecast={rowForecast(limits.stale, limits.seven_day.pct, limits.seven_day.resets_at, WINDOW_DURATION["7d"], now, limits.source)}
        now={now}
      />
    </div>
  );
}

// SidebarRateLimits is the micro-bars under the signed-in user block (mockup frame
// B). Hidden while loading and for a token-less user (no dead chrome); a token with
// no reading renders nothing rather than an empty bar, and a stale one is dimmed.
//
// WHICH tokens ride the rail is the USER'S choice, not a heuristic. This slot has
// now been all three things: every readable token (PRD #104 — 8 meter rows on a
// four-token account, pushing the Factory nav group under the scroll fold at
// 1440x900), then an automatic most-constrained pick (round 2), and now the
// explicit set (round-3 feedback): the DEFAULT token always, plus any token the
// user checked "Show in sidebar" on in Settings (UserSettings.sidebar_token_ids).
// Everything else stays reachable through the "+N more" link to the full
// per-token meters on Settings. Note the rail no longer self-escalates when an
// UNCHECKED token runs hot — that is the deliberate trade for control, and the
// app-wide RateLimitAnnouncer still announces tone crossings for every token.
export function SidebarRateLimits() {
  const { tokens } = useMyRateLimits(60_000);
  const now = useNow();
  // The chosen extras. Fetched on mount and again on the Settings toggle's change
  // event, so a save on the Settings page reaches this separate mount immediately.
  const [sidebarIds, setSidebarIds] = useState<string[]>([]);
  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .getMySettings()
        .then(({ settings }) => {
          if (alive) setSidebarIds(settings.sidebar_token_ids ?? []);
        })
        // A failed fetch keeps the last known set — degrading to default-only on a
        // transient error would look like the user's choice being reverted.
        .catch(() => {});
    load();
    const off = onSidebarTokensChanged(load);
    return () => {
      alive = false;
      off();
    };
  }, []);
  if (!tokens || tokens.length === 0) return null;
  const readable = tokens.filter((t) => t.limits.status === "ok");
  if (readable.length === 0) return null;
  const shown = readable.filter((t) => isShownInSidebar(t, sidebarIds));
  const hidden = readable.length - shown.length;
  return (
    <div className="mt-2 space-y-1.5" aria-label="Claude rate limits">
      <div className="space-y-2.5">
        {shown.map((t) => (
          <TokenMicroMeters key={t.secret_id} token={t} showLabel={readable.length > 1} now={now} />
        ))}
      </div>
      {hidden > 0 && (
        <Link
          to="/settings"
          className="block text-[11px] font-medium text-faint transition-colors hover:text-fg"
        >
          +{hidden} more token{hidden === 1 ? "" : "s"} in Settings
        </Link>
      )}
    </div>
  );
}
