// The two "my own limits" surfaces of PRD #53: the Settings → "Claude limits"
// card and the sidebar-footer micro-meters. Both read GET /me/rate-limits (the
// token never leaves the api — the SPA only ever sees percentages) and reuse the
// shared MeterTrack + toneFor thresholds. The admin table is a separate page.

import { useCallback, useEffect, useRef, useState } from "react";
import { api, type RateLimitWindow, type TokenRateLimits } from "../lib/api";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import {
  forecastKey,
  forecastReadingsFor,
  formatAgo,
  formatCountdown,
  rowForecast,
  statusBadge,
  useNow,
  useReadingSeries,
  worstTokenReading,
  worstWindow,
  type BurnForecast,
} from "../lib/rateLimits";
import { Badge, Card, SectionTitle } from "./ui";
import { MeterTrack, toneFor, type MeterTone } from "./Meter";
import { RateLimitForecastMeter } from "./RateLimitForecast";

// Series is the accumulation getter useReadingSeries returns, threaded to each
// window row so it can read its trailing samples for burnForecast.
type Series = ReturnType<typeof useReadingSeries>;

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
  const [tokens, setTokens] = useState<TokenRateLimits[] | null>(null);
  const [loading, setLoading] = useState(true);
  const load = useCallback(() => {
    api
      .getMyRateLimits()
      .then((d) => {
        setTokens(d.tokens);
        setLoading(false);
      })
      .catch(() => setLoading(false));
  }, []);
  useEffect(() => {
    load();
  }, [load]);
  usePollWhileVisible(load, intervalMs);
  return { tokens, loading };
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
  forecast: BurnForecast;
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
          trailing samples project past the safe band; otherwise this is the plain
          bar (PRD #309 D3). */}
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
  series,
}: {
  token: TokenRateLimits;
  now: number;
  showLabel: boolean;
  series: Series;
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
      <div className="border-t border-line pt-4 first:border-t-0 first:pt-0">
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
  return (
    <div className="border-t border-line pt-4 first:border-t-0 first:pt-0">
      {heading}
      <div className={showLabel ? "mt-2" : ""}>
        <SettingsWindowRow
          label="5-hour window"
          win={limits.five_hour}
          now={now}
          dim={limits.stale}
          forecast={rowForecast(limits.stale, series(forecastKey(token.secret_id, "5h")), limits.five_hour.resets_at, now, limits.source)}
        />
        <SettingsWindowRow
          label="7-day window"
          win={limits.seven_day}
          now={now}
          dim={limits.stale}
          forecast={rowForecast(limits.stale, series(forecastKey(token.secret_id, "7d")), limits.seven_day.resets_at, now, limits.source)}
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
  const series = useReadingSeries(forecastReadingsFor(tokens ?? []), now);

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
          <TokenMeters key={t.secret_id} token={t} now={now} showLabel={showLabels} series={series} />
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

function MicroRow({
  label,
  win,
  dim,
  forecast,
}: {
  label: string;
  win: RateLimitWindow;
  dim: boolean;
  forecast: BurnForecast;
}) {
  return (
    <div className="grid grid-cols-[1.4rem_1fr_2.6rem] items-center gap-2 text-[11px]">
      <span className="font-mono text-faint">{label}</span>
      <RateLimitForecastMeter
        className="h-[5px]"
        label={`${label} window`}
        pct={win.pct}
        valueText={`${win.pct}%`}
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
  series,
}: {
  token: TokenRateLimits;
  showLabel: boolean;
  now: number;
  series: Series;
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
        forecast={rowForecast(limits.stale, series(forecastKey(token.secret_id, "5h")), limits.five_hour.resets_at, now, limits.source)}
      />
      <MicroRow
        label="7d"
        win={limits.seven_day}
        dim={limits.stale}
        forecast={rowForecast(limits.stale, series(forecastKey(token.secret_id, "7d")), limits.seven_day.resets_at, now, limits.source)}
      />
    </div>
  );
}

// SidebarRateLimits is the micro-bars under the signed-in user block (mockup frame
// B) — one PAIR PER TOKEN since PRD #104, each named when the user holds several.
// Hidden while loading and for a token-less user (no dead chrome); a token with no
// reading renders nothing rather than an empty bar, and a stale one is dimmed.
export function SidebarRateLimits() {
  const { tokens } = useMyRateLimits(60_000);
  const now = useNow();
  const series = useReadingSeries(forecastReadingsFor(tokens ?? []), now);
  if (!tokens || tokens.length === 0) return null;
  const readable = tokens.filter((t) => t.limits.status === "ok");
  if (readable.length === 0) return null;
  const showLabels = readable.length > 1;
  return (
    <div className="mt-2 space-y-2.5" aria-label="Claude rate limits">
      {readable.map((t) => (
        <TokenMicroMeters key={t.secret_id} token={t} showLabel={showLabels} now={now} series={series} />
      ))}
    </div>
  );
}
