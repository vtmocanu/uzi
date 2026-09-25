// The two "my own Codex limits" surfaces of PRD #1209 M3: the Settings → "Codex limits"
// card and one account's sidebar micro-meters (CodexAccountMicroMeters, placed in the
// shared account list by SidebarUsageLimits since PRD #1653). Both read GET
// /me/codex-rate-limits (the login never leaves the api — the SPA only ever sees
// percentages + labels) and reuse the shared MeterTrack + toneFor thresholds + anchored
// forecast, so Codex reads as a sibling of the Claude meters, not a redesign. The admin
// table is a separate section on AdminRateLimits.
//
// The ONE Codex-specific rule vs. the Anthropic side: a Codex window carries its OWN
// server-reported length (limit_window_seconds), so the forecast anchors to THAT — never a
// hardcoded 5h/7d — and a 3-hour bucket renders a "3h" chip and a 3-hour projection. See
// lib/codexRateLimits.ts (codexWindowForecast / formatCodexWindowLabel).

import {
  api,
  type CodexAccountRateLimit,
  type CodexRateLimitBucket,
  type CodexRateLimitWindow,
} from "../lib/api";
import { MICRO_METER_GRID_COLS } from "../lib/rateLimitLayout";
import { useAsyncData } from "../lib/useAsyncData";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { formatAgo, formatCountdown, formatResetLabel, useNow, type PaceForecast } from "../lib/rateLimits";
import {
  codexAccountLabel,
  codexBucketCaption,
  codexBucketDisplayName,
  codexLoginRejected,
  codexResetEpoch,
  codexStatusBadge,
  codexWindowForecast,
  formatCodexWindowLabel,
  isCodexAccountShownInSidebar,
} from "../lib/codexRateLimits";
import { Badge, Card, SectionTitle } from "./ui";
import { MeterTrack } from "./Meter";
import { OpenAIIcon } from "./icons";
import { RateLimitForecastMeter } from "./RateLimitForecast";

// useMyCodexRateLimits polls GET /me/codex-rate-limits while the tab is visible, exactly
// like useMyRateLimits on the Anthropic side. An empty array means the user has no linked
// subscription account (the no_subscription shape) — an API-key-only default supplies NO
// implicit subscription meter, since the read returns only linked accounts. Exported for
// the sidebar account list (SidebarUsageLimits), which polls it at the same 60s cadence.
export function useMyCodexRateLimits(intervalMs: number): {
  accounts: CodexAccountRateLimit[] | null;
  loading: boolean;
} {
  const { data, loading, reload } = useAsyncData(
    async () => (await api.getMyCodexRateLimits()).accounts,
    [],
  );
  usePollWhileVisible(reload, intervalMs);
  return { accounts: data, loading };
}

const CARD_BLURB =
  "Live utilization of each linked Codex subscription account. Each bucket is metered on its own window, so the reset countdown and the burn-rate forecast use the length Codex reports for that window. API keys have no subscription windows and do not appear here.";

// The two windows of a bucket, in order, skipping absent slots. A window with a null
// used_percent is a PARTIAL window — kept so the surface can say "no reading" for it,
// rather than silently dropping it.
function bucketWindows(bucket: CodexRateLimitBucket): CodexRateLimitWindow[] {
  return [bucket.primary, bucket.secondary].filter(
    (w): w is CodexRateLimitWindow => w != null,
  );
}

// accountResetLabel is the absolute "resets <Day HH:MM>" line under an account name,
// derived from the account's LONGEST-duration window with a reset — the widest quota a
// user plans around, the Codex analog of the Anthropic card's 7-day reset label. Only for
// a fresh account: a stale/locked snapshot's absolute reset would read as current when it
// is not.
function accountResetLabel(account: CodexAccountRateLimit, nowMs: number): string | null {
  if (account.status !== "fresh") return null;
  let best: { dur: number; epoch: number } | null = null;
  for (const b of account.buckets) {
    for (const w of bucketWindows(b)) {
      const epoch = codexResetEpoch(w, nowMs);
      if (epoch == null || w.limit_window_seconds == null) continue;
      if (!best || w.limit_window_seconds > best.dur) best = { dur: w.limit_window_seconds, epoch };
    }
  }
  return best ? formatResetLabel(best.epoch) : null;
}

// ── Settings card ────────────────────────────────────────────────────────────

function CodexSettingsWindowRow({
  bucketName,
  win,
  dim,
  now,
}: {
  bucketName: string;
  win: CodexRateLimitWindow;
  dim: boolean;
  now: number;
}) {
  const chip = formatCodexWindowLabel(win.limit_window_seconds);
  // The accessible name carries the bucket name too (like the sidebar and admin surfaces),
  // so two same-duration windows of a 2-bucket account ("Requests 7d" vs "Tokens 7d") read
  // distinctly to a screen reader. The visible chip stays the short "5h window".
  const label = `${bucketName} ${chip} window`;
  // A partial window (no percentage reported for this slot) reads "no reading yet" rather
  // than a bogus 0% bar.
  if (win.used_percent == null) {
    return (
      <div className="mt-3 first:mt-0">
        <div className="flex items-baseline justify-between gap-4 text-sm">
          <span className="font-medium text-muted">{chip} window</span>
          <span className="text-faint">no reading yet</span>
        </div>
        <MeterTrack className="mt-1.5 h-2" label={label} fillPct={0} valueText="no reading yet" dim />
      </div>
    );
  }
  const resetsAt = codexResetEpoch(win, now);
  const countdown = formatCountdown(resetsAt, now);
  // The forecast anchors to THIS window's reported duration (via codexWindowForecast),
  // suppressed for a dim/stale reading and for impossible timing. Labeled an estimate by
  // the card footer.
  const forecast: PaceForecast = codexWindowForecast(dim, win, now);
  return (
    <div className="mt-3 first:mt-0">
      <div className="flex items-baseline justify-between gap-4 text-sm">
        <span className="font-medium text-fg">{chip} window</span>
        <span className="tabular-nums text-muted">
          <b className="font-semibold text-fg">{win.used_percent}%</b>
          {countdown && <span> · resets in {countdown}</span>}
        </span>
      </div>
      <RateLimitForecastMeter
        className="mt-1.5 h-2"
        label={label}
        pct={win.used_percent}
        valueText={`${win.used_percent}%${countdown ? `, resets in ${countdown}` : ""}`}
        forecast={forecast}
        dim={dim}
      />
    </div>
  );
}

function CodexAccountBlock({
  account,
  now,
  sidebarShown,
  onToggle,
  busy,
}: {
  account: CodexAccountRateLimit;
  now: number;
  sidebarShown: boolean;
  onToggle: (accountId: string, shown: boolean) => void;
  busy: boolean;
}) {
  const badge = codexStatusBadge(account);
  // A non-fresh reading (stale / vault-locked / polling-off snapshot) is dimmed and draws
  // no forecast; pending / no_reading carry no buckets, and any other status may still carry
  // the last stored buckets.
  const dim = account.status !== "fresh";
  const resetLabel = accountResetLabel(account, now);
  const label = codexAccountLabel(account);
  const hintId = `codex-acct-status-${account.account_id}`;

  return (
    <div className="border-t border-edge pt-4 first:border-t-0 first:pt-0">
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-fg">{label}</span>
        {account.is_default && <Badge tone="ok">default</Badge>}
        <Badge tone={badge.tone} title={badge.hint} aria-describedby={hintId}>
          {badge.label}
        </Badge>
        <span id={hintId} className="sr-only">
          {badge.hint}
        </span>
      </div>
      {resetLabel && <div className="mt-0.5 text-xs text-faint">{resetLabel}</div>}

      {account.buckets.length > 0 ? (
        <div className="mt-2 space-y-3">
          {account.buckets.map((b) => {
            // The main bucket ("codex") draws no caption (PRD #1653 D-W3): its name would
            // only repeat the provider. Its window rows keep the name in their accessible
            // label, and a limit-reached badge still gets its own line.
            const caption = codexBucketCaption(b);
            return (
              <div key={b.id}>
                {(caption != null || b.limit_reached === true) && (
                  <div className="flex items-center gap-2">
                    {caption != null && <span className="text-xs font-medium text-muted">{caption}</span>}
                    {b.limit_reached === true && <Badge tone="danger">limit reached</Badge>}
                  </div>
                )}
                {bucketWindows(b).map((w, i) => (
                  <CodexSettingsWindowRow key={`${b.id}:${i}`} bucketName={codexBucketDisplayName(b)} win={w} dim={dim} now={now} />
                ))}
              </div>
            );
          })}
        </div>
      ) : (
        // No buckets to draw: state the explicit reason in place of the meters.
        <p className="mt-2 text-xs text-muted">{badge.hint}</p>
      )}
      {/* A rejected login keeps its (dimmed) last buckets, so the bucket-less hint above never
          renders: state the re-login guidance under the meters instead (#1594). */}
      {account.buckets.length > 0 && codexLoginRejected(account) && (
        <p className="mt-2 text-xs text-muted">{badge.hint}</p>
      )}

      {/* "Show in sidebar and TUI" — the Codex sibling of the Anthropic sidebar checkbox.
          The default account is pinned (checked + disabled with a tooltip): the rail and
          TUI always surface it, so hiding the box would make the pinning unreadable. */}
      <label
        className="mt-3 flex items-center gap-2 text-sm text-muted"
        title={account.is_default ? "The default Codex account always shows in the sidebar and TUI." : undefined}
      >
        <input
          type="checkbox"
          className="h-4 w-4 accent-brand"
          checked={sidebarShown}
          disabled={busy || account.is_default}
          aria-label={`Show ${label} in the sidebar and TUI`}
          onChange={(e) => onToggle(account.account_id, e.target.checked)}
        />
        <span aria-hidden="true">Show in sidebar and TUI</span>
      </label>

      {account.last_success_at && (
        <div className="mt-1 text-xs text-muted">
          updated {formatAgo(account.last_success_at, now)}
          {dim ? " · reading may be behind" : " · refreshes every few minutes"}
        </div>
      )}
    </div>
  );
}

// CodexRateLimitCard is the Settings → "Codex limits" card. Hidden entirely when the user
// holds no linked subscription account (and while the first read is in flight, to avoid a
// flash under the credential card). It retains the FULL account inventory regardless of the
// sidebar (compact-view) selection: the checkbox drives the rail, not what this card shows.
export function CodexRateLimitCard({
  sidebarAccountIds,
  onToggleSidebarAccount,
  busy = false,
}: {
  sidebarAccountIds: string[];
  onToggleSidebarAccount: (accountId: string, shown: boolean) => void;
  busy?: boolean;
}) {
  const { accounts, loading } = useMyCodexRateLimits(60_000);
  const now = useNow();

  if (loading || !accounts || accounts.length === 0) return null;

  return (
    <Card className="space-y-5">
      <div>
        {/* The provider logo sits in the title only (PRD #1653 D-W4): every account in
            this card is Codex, so no per-account logo. aria-hidden; the title names it. */}
        <SectionTitle className="flex items-center gap-2">
          <OpenAIIcon className="h-4 w-4 flex-none text-fg" />
          Codex limits
        </SectionTitle>
        <p className="mt-2 text-sm text-muted">{CARD_BLURB}</p>
      </div>
      <div className="space-y-4">
        {accounts.map((a) => (
          <CodexAccountBlock
            key={a.account_id}
            account={a}
            now={now}
            sidebarShown={isCodexAccountShownInSidebar(a, sidebarAccountIds)}
            onToggle={onToggleSidebarAccount}
            busy={busy}
          />
        ))}
      </div>
      <p className="text-xs text-faint">
        Forecasts are estimates projected from each window&rsquo;s reported usage and reset — not a
        guarantee. They are hidden for partial or stale readings.
      </p>
    </Card>
  );
}

// ── Sidebar micro-meters ───────────────────────────────────────────────────────

function CodexMicroRow({
  chip,
  ariaLabel,
  win,
  dim,
  now,
}: {
  chip: string;
  ariaLabel: string;
  win: CodexRateLimitWindow;
  dim: boolean;
  now: number;
}) {
  const pct = win.used_percent ?? 0;
  const resetsAt = codexResetEpoch(win, now);
  const countdown = formatCountdown(resetsAt, now);
  const forecast = codexWindowForecast(dim, win, now);
  return (
    <div className={`grid ${MICRO_METER_GRID_COLS} items-center gap-2 text-[11px]`}>
      <span className="truncate font-mono text-faint">{chip}</span>
      <RateLimitForecastMeter
        className="h-[5px]"
        label={ariaLabel}
        pct={pct}
        valueText={`${pct}%${countdown ? `, resets in ${countdown}` : ""}`}
        forecast={forecast}
        dim={dim}
      />
      <span className="text-right font-mono tabular-nums text-muted">{pct}%</span>
    </div>
  );
}

// CodexAccountMicroMeters is one account's stack of 5px bars for the sidebar account list
// (SidebarUsageLimits renders the account's logo + name header and the group around it).
// Only windows with an actual reading render; a stale account is dimmed. Rows are grouped by
// bucket: every bucket but the main one ("codex") gets a small faint caption line above its
// rows (PRD #1653 D-W3), and every row's accessible name keeps the bucket name. Returns null
// when no window has a reading; callers skip such an account (hasCodexMicroReading).
export function CodexAccountMicroMeters({
  account,
  now,
}: {
  account: CodexAccountRateLimit;
  now: number;
}) {
  const dim = account.status !== "fresh";
  const label = codexAccountLabel(account);
  const groups = account.buckets
    .map((b) => ({
      bucket: b,
      caption: codexBucketCaption(b),
      rows: bucketWindows(b)
        .filter((w) => w.used_percent != null)
        .map((w) => ({ win: w, chip: formatCodexWindowLabel(w.limit_window_seconds) })),
    }))
    .filter((g) => g.rows.length > 0);
  if (groups.length === 0) return null;
  const title = [
    label,
    ...groups.flatMap((g) =>
      g.rows.map((r) => {
        const c = formatCountdown(codexResetEpoch(r.win, now), now);
        return c ? `${r.chip} resets in ${c}` : null;
      }),
    ),
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <div className="space-y-1.5" title={title}>
      {groups.map((g) => (
        <div key={g.bucket.id} className="space-y-1.5">
          {g.caption != null && <div className="truncate text-[10px] text-faint">{g.caption}</div>}
          {g.rows.map((r, i) => (
            <CodexMicroRow
              key={`${g.bucket.id}:${i}`}
              chip={r.chip}
              ariaLabel={`${codexBucketDisplayName(g.bucket)} ${r.chip} window`}
              win={r.win}
              dim={dim}
              now={now}
            />
          ))}
        </div>
      ))}
    </div>
  );
}
