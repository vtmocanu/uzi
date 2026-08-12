// Admin → Rate limits (PRD #53, mockup frame C): every user's Claude 5h/7d
// utilization on one page. Rows sort danger → warn → ok → stale → no-reading so
// the capacity view leads with who is near a wall. Same table conventions as
// Admin → Users; the meters reuse the shared MeterTrack + toneFor thresholds.

import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type AdminRateLimitUser, type MyRateLimits, type TokenRateLimits } from "../lib/api";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import {
  burnForecast,
  forecastKey,
  forecastReadingsFor,
  formatAgo,
  formatCountdown,
  sortAdminRows,
  statusBadge,
  useNow,
  useReadingSeries,
  type BurnForecast,
} from "../lib/rateLimits";
import { Alert, Badge, Card, cx, EmptyState, ListSkeleton, PageHeader } from "../components/ui";
import { RateLimitForecastMeter } from "../components/RateLimitForecast";

// Series is the accumulation getter useReadingSeries returns, threaded to each
// window row for burnForecast (the admin view accumulates across ALL users' tokens).
type Series = ReturnType<typeof useReadingSeries>;

// One stacked meter row inside the Utilization cell: a mono window chip (5h/7d),
// the shared MeterTrack, its percent, and the reset countdown. The visible chip is
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
  forecast: BurnForecast;
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
function UtilizationCell({ token, now, series }: { token: TokenRateLimits; now: number; series: Series }) {
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
        forecast={burnForecast(series(forecastKey(token.secret_id, "5h")), limits.five_hour.resets_at, now, limits.source)}
      />
      <WindowRow
        win={limits.seven_day}
        chip="7d"
        label="7-day window"
        stale={limits.stale}
        now={now}
        forecast={burnForecast(series(forecastKey(token.secret_id, "7d")), limits.seven_day.resets_at, now, limits.source)}
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
  if (!showIdentity) return null;
  return (
    <td className="px-4 py-3 align-top" rowSpan={rowSpan}>
      <div className="font-medium text-fg">
        {user.name || <span className="italic text-faint">no name</span>}
      </div>
      <div className="text-xs text-faint">{user.email}</div>
    </td>
  );
}

export function AdminRateLimits() {
  const [users, setUsers] = useState<AdminRateLimitUser[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const now = useNow();
  // One accumulation store across every user's tokens — secret_id keys are globally
  // unique, so cross-user rows never collide (forecastKey).
  const series = useReadingSeries(forecastReadingsFor(users.flatMap((u) => u.tokens)), now);

  const load = useCallback(() => {
    api
      .getAdminRateLimits()
      .then(({ users }) => {
        setUsers(users);
        setLoading(false);
      })
      .catch((err) => {
        setError(err instanceof ApiError ? err.message : "Failed to load rate limits");
        setLoading(false);
      });
  }, []);

  useEffect(() => {
    load();
  }, [load]);
  usePollWhileVisible(load, 60_000);

  const rows = sortAdminRows(users);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Rate limits"
        description="Each token's Claude 5-hour and 7-day utilization, read server-side with the owner's own credentials. Anthropic meters per credential, so a user with several tokens has a row each. Users nearest a limit sort first."
      />
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
                  <th className="px-4 py-3 font-medium">Utilization</th>
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
                            </td>
                            <td className="px-4 py-3 align-top">
                              <UtilizationCell token={t} now={now} series={series} />
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
    </div>
  );
}
