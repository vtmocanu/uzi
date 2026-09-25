// Admin → Rate limits (PRD #53, mockup frame C): every user's Claude 5h/7d
// utilization on one page. Rows sort danger → warn → ok → stale → no-reading so
// the capacity view leads with who is near a wall. Same table conventions as
// Admin → Users; the meters reuse the shared MeterTrack + toneFor thresholds.

import {
  api,
  type AdminRateLimitUser,
  type CodexAccountRateLimit,
  type CodexAdminRateLimitRow,
  type CodexRateLimitBucket,
  type CodexRateLimitWindow,
  type MyRateLimits,
  type TokenRateLimits,
} from "../lib/api";
import { useAsyncData } from "../lib/useAsyncData";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import {
  formatAgo,
  formatCountdown,
  formatResetLabel,
  rowForecast,
  sortAdminRows,
  statusBadge,
  useNow,
  WINDOW_DURATION,
  type PaceForecast,
} from "../lib/rateLimits";
import {
  codexAccountLabel,
  codexBucketDisplayName,
  codexResetEpoch,
  codexStatusBadge,
  codexWindowForecast,
  formatCodexWindowLabel,
  hasCodexReading,
  sortCodexAdminRows,
} from "../lib/codexRateLimits";
import { Alert, Badge, Card, cx, EmptyState, ListSkeleton, SectionTitle } from "../components/ui";
import { AdminShell } from "../components/AdminShell";
import { DocLink } from "../components/DocLink";
import { DOC_RATE_LIMITS } from "../lib/doclinks";
import { RateLimitForecastMeter } from "../components/RateLimitForecast";
import { MeterTrack } from "../components/Meter";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail, maskName } from "../lib/demoMask";

// One stacked meter row inside the Utilization cell: a mono window chip (5h/7d),
// the RateLimitForecastMeter (the shared MeterTrack plus the PRD #309/#310 anchored
// forecast ghost overlay), its percent, and the reset countdown. The visible chip is
// short, but MeterTrack's aria-label carries the FULL window name ("5-hour window"
// / "7-day window") for screen readers (Decision 4 — the test selects bars by that
// name), and the chip stays text-muted (not the mock's faint) so it clears WCAG AA
// at its small size: it is the only visible window identity a sighted low-vision
// admin has now that the full column headers are gone.
type OkWindow = Extract<MyRateLimits, { status: "ok" }>["five_hour"];

function WindowRow({
  win,
  chip,
  label,
  stale,
  now,
  forecast,
}: {
  win: OkWindow;
  chip: string;
  label: string;
  stale: boolean;
  now: number;
  forecast: PaceForecast;
}) {
  const reset = stale ? "stale" : (formatCountdown(win.resets_at, now) ?? "—");
  return (
    <div className="grid grid-cols-[1.5rem_minmax(4rem,1fr)_2.5rem_4.25rem] items-center gap-2.5">
      <span className="font-mono text-xs text-muted">{chip}</span>
      <RateLimitForecastMeter className="h-1.5" label={label} pct={win.pct} valueText={`${win.pct}%`} forecast={forecast} dim={stale} />
      <span className={cx("text-right font-mono tabular-nums", stale ? "text-faint" : "text-muted")}>
        {win.pct}%
      </span>
      {/* The live countdown is data → text-muted for WCAG AA at 12px (web-ux); a
          stale row's "stale" label stays faint (de-emphasised, and the dimmed bar
          + badge already carry the staleness). Fixed-width, right-aligned so a long
          countdown ("23h 59m") never jogs the 5h/7d percents out of a clean column. */}
      <span
        className={cx(
          "whitespace-nowrap text-right text-xs tabular-nums",
          stale ? "text-faint" : "text-muted",
        )}
      >
        {reset}
      </span>
    </div>
  );
}

// Utilization stacks the 5-hour and 7-day meters as two thin rows in ONE column
// (PRD #240): what used to be two ~280px side-by-side columns, halving the table's
// width so it fits its card. A non-ok reading collapses to a single em-dash — never
// two — so the row stays one Utilization cell.
function UtilizationCell({ token, now }: { token: TokenRateLimits; now: number }) {
  const { limits } = token;
  if (limits.status !== "ok") return <span className="text-faint">—</span>;
  return (
    <div className="flex max-w-[22rem] flex-col gap-2">
      <WindowRow
        win={limits.five_hour}
        chip="5h"
        label="5-hour window"
        stale={limits.stale}
        now={now}
        forecast={rowForecast(limits.stale, limits.five_hour.pct, limits.five_hour.resets_at, WINDOW_DURATION["5h"], now, limits.source)}
      />
      <WindowRow
        win={limits.seven_day}
        chip="7d"
        label="7-day window"
        stale={limits.stale}
        now={now}
        forecast={rowForecast(limits.stale, limits.seven_day.pct, limits.seven_day.resets_at, WINDOW_DURATION["7d"], now, limits.source)}
      />
    </div>
  );
}

// UserCell is the identity column, rendered once per USER even when they hold
// several tokens (the extra rows sit under it via rowSpan). The name must stay in
// the first div — the sort test reads it via querySelector("div") — and a faint
// placeholder fills it when the user has no name so the email doesn't float under
// an empty line (PRD #54).
function UserCell({
  user,
  rowSpan,
  showIdentity,
}: {
  user: AdminRateLimitUser;
  rowSpan: number;
  showIdentity: boolean;
}) {
  const demo = useDemoMode();
  if (!showIdentity) return null;
  return (
    <td className="px-4 py-3 align-top" rowSpan={rowSpan}>
      <div className="font-medium text-fg">
        {maskName(user.name, demo) || <span className="italic text-faint">no name</span>}
      </div>
      <div className="text-xs text-faint">{maskEmail(user.email, demo)}</div>
    </td>
  );
}

// ── Codex accounts section (PRD #1209 M3) ────────────────────────────────────
// A provider-labeled sibling of the Claude table above, grouped BY USER then BY
// ACCOUNT, sorted by valid utilization (sortCodexAdminRows). Alias capacity is never
// double-counted (one account = one budget); only the safe DTO fields are read.

function bucketWindows(bucket: CodexRateLimitBucket): CodexRateLimitWindow[] {
  return [bucket.primary, bucket.secondary].filter(
    (w): w is CodexRateLimitWindow => w != null,
  );
}

function CodexWindowRow({
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
  const label = `${bucketName} ${chip} window`;
  if (win.used_percent == null) {
    // A partial window: no percentage for this slot. A grey 0-width bar keeps the
    // column aligned; the "—" reset says there is nothing to project.
    return (
      <div className="grid grid-cols-[1.5rem_minmax(4rem,1fr)_2.5rem_4.25rem] items-center gap-2.5">
        <span className="font-mono text-xs text-muted">{chip}</span>
        <MeterTrack className="h-1.5" label={label} fillPct={0} valueText="no reading" dim />
        <span className="text-right font-mono tabular-nums text-faint">—</span>
        <span className="whitespace-nowrap text-right text-xs tabular-nums text-faint">no reading</span>
      </div>
    );
  }
  const reset = dim ? "stale" : (formatCountdown(codexResetEpoch(win, now), now) ?? "—");
  const forecast = codexWindowForecast(dim, win, now);
  return (
    <div className="grid grid-cols-[1.5rem_minmax(4rem,1fr)_2.5rem_4.25rem] items-center gap-2.5">
      <span className="font-mono text-xs text-muted">{chip}</span>
      <RateLimitForecastMeter
        className="h-1.5"
        label={label}
        pct={win.used_percent}
        valueText={`${win.used_percent}%`}
        forecast={forecast}
        dim={dim}
      />
      <span className={cx("text-right font-mono tabular-nums", dim ? "text-faint" : "text-muted")}>
        {win.used_percent}%
      </span>
      <span
        className={cx(
          "whitespace-nowrap text-right text-xs tabular-nums",
          dim ? "text-faint" : "text-muted",
        )}
      >
        {reset}
      </span>
    </div>
  );
}

// CodexUtilizationCell stacks an account's buckets/windows as thin rows in ONE column,
// mirroring the Claude UtilizationCell. An account with no reading (pending, no_reading,
// vault_locked, credential_action_required, polling_disabled) collapses to a single
// em-dash — never a bogus bar.
function CodexUtilizationCell({ account, now }: { account: CodexAccountRateLimit; now: number }) {
  if (!hasCodexReading(account.status)) return <span className="text-faint">—</span>;
  const dim = account.status !== "fresh";
  const rows = account.buckets.flatMap((b) =>
    bucketWindows(b).map((w, i) => (
      <CodexWindowRow key={`${b.id}:${i}`} bucketName={codexBucketDisplayName(b)} win={w} dim={dim} now={now} />
    )),
  );
  if (rows.length === 0) return <span className="text-faint">—</span>;
  return <div className="flex max-w-[22rem] flex-col gap-2">{rows}</div>;
}

function CodexUserCell({
  user,
  rowSpan,
}: {
  user: CodexAdminRateLimitRow;
  rowSpan: number;
}) {
  const demo = useDemoMode();
  return (
    <td className="px-4 py-3 align-top" rowSpan={rowSpan}>
      <div className="font-medium text-fg">
        {maskName(user.name, demo) || <span className="italic text-faint">no name</span>}
      </div>
      <div className="text-xs text-faint">{maskEmail(user.email, demo)}</div>
    </td>
  );
}

function CodexAdminSection() {
  const { data, loading, error, reload } = useAsyncData(
    async () => (await api.getAdminCodexRateLimits()).users,
    [],
    { fallback: "Failed to load Codex rate limits" },
  );
  const now = useNow();
  usePollWhileVisible(reload, 60_000);

  const users = data ?? [];
  // Self-hide until there is a Codex user to show: an instance with no linked Codex
  // subscription accounts gets no dead chrome under the Claude table. Only take the
  // alert-only branch when there is NO data at all yet (data === null: the initial load
  // has never succeeded), so an admin is never told "none" when the read failed with no
  // prior data. A CONFIRMED-empty load (data === [], a non-Codex instance) self-hides
  // even on a later transient poll error — it falls through to the null-return below —
  // so a poll blip never flashes a Codex error banner where there are no Codex accounts.
  // Once there ARE last-good rows, a transient poll error keeps the table rendered with
  // the alert shown above it (mirroring the Claude table), rather than blanking the whole
  // capacity view for up to a poll interval on a single failed poll.
  if (error && data === null) {
    return (
      <section aria-label="Codex accounts" className="mt-10">
        <SectionTitle>Codex accounts</SectionTitle>
        <div className="mt-3">
          <Alert message={error} />
        </div>
      </section>
    );
  }
  if (loading || users.length === 0) return null;
  const rows = sortCodexAdminRows(users);

  return (
    <section aria-label="Codex accounts" className="mt-10">
      <div className="mb-3">
        <SectionTitle>Codex accounts</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Each linked Codex subscription account&rsquo;s per-bucket utilization, read server-side
          with the owner&rsquo;s own login. Each bucket is metered on its own window, so its reset and
          forecast use the length Codex reports. Users nearest a limit sort first.
        </p>
      </div>
      {error && (
        <div className="mb-3">
          <Alert message={error} />
        </div>
      )}
      <Card className="p-0">
        <div className="overflow-x-auto">
          <table className="w-full text-left text-sm">
            <thead className="border-b border-edge text-muted">
              <tr>
                <th className="px-4 py-3 font-medium">User</th>
                <th className="px-4 py-3 font-medium">Account</th>
                <th className="px-4 py-3 font-medium">Utilization &amp; Forecast</th>
                <th className="px-4 py-3 font-medium">Status</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-edge">
              {rows.flatMap((u) =>
                u.accounts.map((a, i) => {
                  const badge = codexStatusBadge(a);
                  return (
                    <tr key={`${u.id}:${a.account_id}`} className="transition-colors hover:bg-raised/30">
                      {i === 0 ? <CodexUserCell user={u} rowSpan={u.accounts.length} /> : null}
                      <td className="px-4 py-3 align-top">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="text-xs font-medium text-fg">{codexAccountLabel(a)}</span>
                          {a.is_default && <Badge tone="neutral">default</Badge>}
                        </div>
                      </td>
                      <td className="px-4 py-3 align-top">
                        <CodexUtilizationCell account={a} now={now} />
                      </td>
                      <td className="px-4 py-3 align-top">
                        <Badge tone={badge.tone} title={badge.hint}>
                          {badge.label}
                        </Badge>
                        {a.last_success_at && (
                          <div className="mt-1.5 text-xs tabular-nums text-muted">
                            updated {formatAgo(a.last_success_at, now)}
                          </div>
                        )}
                      </td>
                    </tr>
                  );
                }),
              )}
            </tbody>
          </table>
        </div>
      </Card>
    </section>
  );
}

export function AdminRateLimits() {
  // The poll reuses the hook's reload, so a poll failure surfaces the same error
  // the initial load would (PRD #950 M3, Tier C). skeleton stays "initial" (the
  // hook default), so the 60s poll's reload never re-shows the ListSkeleton — as
  // today's poll never set loading true.
  const { data, loading, error, reload } = useAsyncData(
    async () => (await api.getAdminRateLimits()).users,
    [],
    { fallback: "Failed to load rate limits" },
  );
  const users = data ?? [];
  const now = useNow();

  usePollWhileVisible(reload, 60_000);

  const rows = sortAdminRows(users);

  return (
    <AdminShell
      description={
        <>
          Each token's Claude 5-hour and 7-day utilization, read server-side with the owner's own
          credentials. Anthropic meters per credential, so a user with several tokens has a row
          each. Users nearest a limit sort first. See the{" "}
          <DocLink slug={DOC_RATE_LIMITS}>Claude rate limits</DocLink> guide.
        </>
      }
    >
      {error && <Alert message={error} />}
      {loading ? (
        <ListSkeleton rows={5} />
      ) : rows.length === 0 ? (
        <EmptyState
          title="No users yet"
          description="Rate-limit readings appear here once users save an Anthropic token."
        />
      ) : (
        <Card className="p-0">
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-edge text-muted">
                <tr>
                  <th className="px-4 py-3 font-medium">User</th>
                  <th className="px-4 py-3 font-medium">Token</th>
                  <th className="px-4 py-3 font-medium">Utilization &amp; Forecast</th>
                  <th className="px-4 py-3 font-medium">Status</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-edge">
                {rows.flatMap((u) =>
                  // One ROW PER TOKEN since PRD #104: a user's credentials are
                  // metered separately, so collapsing them to one row would show a
                  // number that describes only one of the accounts they spend. A
                  // token-less user still gets exactly one row (no_token), so every
                  // user remains present and countable.
                  u.tokens.length === 0
                    ? [
                        // Every cell keeps its column: a colSpan here would slide
                        // the status badge under the Utilization header and break
                        // the table's alignment for the one row a reader is most
                        // likely to be scanning past. Token + Utilization are each a
                        // single em-dash (two dashes total for a no_token row).
                        <tr key={u.id} className="transition-colors hover:bg-raised/30">
                          <UserCell user={u} rowSpan={1} showIdentity />
                          <td className="px-4 py-3 text-xs text-faint">—</td>
                          <td className="px-4 py-3 text-xs text-faint">—</td>
                          <td className="px-4 py-3">
                            <Badge tone="neutral">no token</Badge>
                          </td>
                        </tr>,
                      ]
                    : u.tokens.map((t, i) => {
                        const badge = statusBadge(t.limits, u.vault_locked);
                        // The mock's absolute "resets <Day HH:MM>" line under the token
                        // name (PRD #310 M3), from the 7-day reset — the weekly quota
                        // users plan around. The map also runs for non-ok tokens (which
                        // carry no seven_day), so the status guard is required; omitted
                        // when the 7-day resets_at is null.
                        const resetLabel =
                          t.limits.status === "ok" ? formatResetLabel(t.limits.seven_day.resets_at) : null;
                        return (
                          <tr key={`${u.id}:${t.secret_id}`} className="transition-colors hover:bg-raised/30">
                            {/* The identity cell renders on the user's FIRST token
                                row only; the rest are visually grouped under it. */}
                            {i === 0 ? (
                              <UserCell user={u} rowSpan={u.tokens.length} showIdentity />
                            ) : null}
                            <td className="px-4 py-3 align-top">
                              <div className="flex items-center gap-2">
                                <span className="text-xs font-medium text-fg">{t.label}</span>
                                {t.is_default && <Badge tone="neutral">default</Badge>}
                              </div>
                              {resetLabel && (
                                <div className="mt-0.5 text-xs text-faint">{resetLabel}</div>
                              )}
                            </td>
                            <td className="px-4 py-3 align-top">
                              <UtilizationCell token={t} now={now} />
                            </td>
                            <td className="px-4 py-3 align-top">
                              <Badge tone={badge.tone} dot={badge.dot}>
                                {badge.label}
                              </Badge>
                              {/* PRD #217 D6: a limit_report reading is a park-time
                                  inference, not a poll — an operator must be able to
                                  tell it from a usage_endpoint/header_probe row. An
                                  inline badge (no new column: the table is pinned to
                                  four) sits above the "updated" line it qualifies,
                                  because that 100% was recorded AFTER that timestamp
                                  (D3). Other sources render no badge, exactly as today. */}
                              {t.limits.status === "ok" && t.limits.source === "limit_report" && (
                                <div className="mt-1.5">
                                  <Badge tone="neutral">Recorded at usage limit</Badge>
                                </div>
                              )}
                              {/* "Updated" folds under the Status pill (PRD #240 Decision 2):
                                  the relocated Updated column, NOT a new element, so it keeps
                                  text-muted — it must clear WCAG AA at 12px (web-ux finding). */}
                              {t.limits.status === "ok" && (
                                <div className="mt-1.5 text-xs tabular-nums text-muted">
                                  updated {formatAgo(t.limits.synced_at, now)}
                                </div>
                              )}
                            </td>
                          </tr>
                        );
                      }),
                )}
              </tbody>
            </table>
          </div>
        </Card>
      )}
      {/* Codex accounts (PRD #1209 M3): a separate, provider-labeled section below the
          Claude table. Self-hides until an instance has a linked Codex account. */}
      <CodexAdminSection />
    </AdminShell>
  );
}
