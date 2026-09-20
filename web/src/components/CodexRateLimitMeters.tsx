// The two "my own Codex limits" surfaces of PRD #1209 M3: the Settings → "Codex limits"
// card and the sidebar-footer micro-meters. Both read GET /me/codex-rate-limits (the
// login never leaves the api — the SPA only ever sees percentages + labels) and reuse the
// shared MeterTrack + toneFor thresholds + anchored forecast, so Codex reads as a
// PROVIDER-LABELED SIBLING of the Claude meters, not a redesign. The admin table is a
// separate section on AdminRateLimits.
//
// The ONE Codex-specific rule vs. the Anthropic side: a Codex window carries its OWN
// server-reported length (limit_window_seconds), so the forecast anchors to THAT — never a
// hardcoded 5h/7d — and a 3-hour bucket renders a "3h" chip and a 3-hour projection. See
// lib/codexRateLimits.ts (codexWindowForecast / formatCodexWindowLabel).

import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import {
  api,
  type CodexAccountRateLimit,
  type CodexRateLimitBucket,
  type CodexRateLimitWindow,
} from "../lib/api";
import { onSidebarTokensChanged } from "../lib/sidebarTokens";
import { useAsyncData } from "../lib/useAsyncData";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { formatAgo, formatCountdown, formatResetLabel, useNow, type PaceForecast } from "../lib/rateLimits";
import {
  codexAccountLabel,
  codexResetEpoch,
  codexStatusBadge,
  codexWindowForecast,
  formatCodexWindowLabel,
  hasCodexReading,
  isCodexAccountShownInSidebar,
} from "../lib/codexRateLimits";
import { Badge, Card, cx, SectionTitle } from "./ui";
import { MeterTrack } from "./Meter";
import { RateLimitForecastMeter } from "./RateLimitForecast";

// useMyCodexRateLimits polls GET /me/codex-rate-limits while the tab is visible, exactly
// like useMyRateLimits on the Anthropic side. An empty array means the user has no linked
// subscription account (the no_subscription shape) — an API-key-only default supplies NO
// implicit subscription meter, since the read returns only linked accounts.
function useMyCodexRateLimits(intervalMs: number): {
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

function bucketDisplayName(bucket: CodexRateLimitBucket): string {
  return bucket.display_name?.trim() || bucket.id;
}

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
  win,
  dim,
  now,
}: {
  win: CodexRateLimitWindow;
  dim: boolean;
  now: number;
}) {
  const chip = formatCodexWindowLabel(win.limit_window_seconds);
  // A partial window (no percentage reported for this slot) reads "no reading yet" rather
  // than a bogus 0% bar.
  if (win.used_percent == null) {
    return (
      <div className="mt-3 first:mt-0">
        <div className="flex items-baseline justify-between gap-4 text-sm">
          <span className="font-medium text-muted">{chip} window</span>
          <span className="text-faint">no reading yet</span>
        </div>
        <MeterTrack className="mt-1.5 h-2" label={`${chip} window`} fillPct={0} valueText="no reading yet" dim />
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
        label={`${chip} window`}
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
  const badge = codexStatusBadge(account.status);
  // A non-fresh reading (stale / vault-locked / polling-off snapshot) is dimmed and draws
  // no forecast; pending / no_reading / action-required carry no buckets at all.
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
          {account.buckets.map((b) => (
            <div key={b.id}>
              <div className="flex items-center gap-2">
                <span className="text-xs font-medium text-muted">{bucketDisplayName(b)}</span>
                {b.limit_reached === true && <Badge tone="danger">limit reached</Badge>}
              </div>
              {bucketWindows(b).map((w, i) => (
                <CodexSettingsWindowRow key={`${b.id}:${i}`} win={w} dim={dim} now={now} />
              ))}
            </div>
          ))}
        </div>
      ) : (
        // No buckets to draw: state the explicit reason in place of the meters.
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
        <SectionTitle>Codex limits</SectionTitle>
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
    <div className="grid grid-cols-[2.2rem_1fr_2.6rem] items-center gap-2 text-[11px]">
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

// CodexAccountMicroMeters is one account's stack of 5px bars, the account named above them
// only when the user surfaces several. Only windows with an actual reading render; a stale
// account is dimmed.
function CodexAccountMicroMeters({
  account,
  showLabel,
  now,
}: {
  account: CodexAccountRateLimit;
  showLabel: boolean;
  now: number;
}) {
  const dim = account.status !== "fresh";
  const label = codexAccountLabel(account);
  const rows = account.buckets.flatMap((b) =>
    bucketWindows(b)
      .filter((w) => w.used_percent != null)
      .map((w) => ({ bucket: b, win: w, chip: formatCodexWindowLabel(w.limit_window_seconds) })),
  );
  if (rows.length === 0) return null;
  const title = [
    showLabel && label,
    ...rows.map((r) => {
      const c = formatCountdown(codexResetEpoch(r.win, now), now);
      return c ? `${r.chip} resets in ${c}` : null;
    }),
  ]
    .filter(Boolean)
    .join(" · ");
  return (
    <div className="space-y-1.5" title={title || undefined}>
      {showLabel && (
        <div className="truncate text-[10px] font-medium uppercase tracking-wide text-faint">
          {label}
        </div>
      )}
      {rows.map((r, i) => (
        <CodexMicroRow
          key={`${r.bucket.id}:${i}`}
          chip={r.chip}
          ariaLabel={`${bucketDisplayName(r.bucket)} ${r.chip} window`}
          win={r.win}
          dim={dim}
          now={now}
        />
      ))}
    </div>
  );
}

// SidebarCodexRateLimits is the Codex micro-bars, rendered beside the Claude ones under
// the user block. It carries an explicit "Codex" provider label (Claude renders unmarked,
// the harness-labeling convention). Hidden while loading and for a user with no linked
// account; only accounts with a reading (fresh/stale) render, a stale one dimmed. Refetches
// its selection on the shared sidebar-changed event so a Settings save updates it at once.
export function SidebarCodexRateLimits() {
  const { accounts } = useMyCodexRateLimits(60_000);
  const now = useNow();
  const [sidebarIds, setSidebarIds] = useState<string[]>([]);
  useEffect(() => {
    let alive = true;
    const load = () =>
      api
        .getMySettings()
        .then(({ settings }) => {
          if (alive) setSidebarIds(settings.sidebar_codex_account_ids ?? []);
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
  if (!accounts || accounts.length === 0) return null;
  const readable = accounts.filter((a) => hasCodexReading(a.status));
  if (readable.length === 0) return null;
  const shown = readable.filter((a) => isCodexAccountShownInSidebar(a, sidebarIds));
  const hidden = readable.length - shown.length;
  return (
    <div className="mt-2 space-y-1.5" aria-label="Codex rate limits">
      <div className="text-[10px] font-medium uppercase tracking-wide text-faint">Codex</div>
      <div className="space-y-2.5">
        {shown.map((a) => (
          <CodexAccountMicroMeters
            key={a.account_id}
            account={a}
            showLabel={readable.length > 1}
            now={now}
          />
        ))}
      </div>
      {hidden > 0 && (
        <Link
          to="/settings"
          className={cx(
            "block text-[11px] font-medium text-faint transition-colors hover:text-fg",
          )}
        >
          +{hidden} more Codex account{hidden === 1 ? "" : "s"} in Settings
        </Link>
      )}
    </div>
  );
}
