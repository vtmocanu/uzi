// Admin → Rate limits (PRD #53, mockup frame C): every user's Claude 5h/7d
// utilization on one page. Rows sort danger → warn → ok → stale → no-reading so
// the capacity view leads with who is near a wall. Same table conventions as
// Admin → Users; the meters reuse the shared MeterTrack + toneFor thresholds.

import { useCallback, useEffect, useState } from "react";
import { api, ApiError, type AdminRateLimitUser, type MyRateLimits } from "../lib/api";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { formatAgo, formatCountdown, sortAdminRows, statusBadge, useNow } from "../lib/rateLimits";
import { Alert, Badge, Card, cx, EmptyState, ListSkeleton, PageHeader } from "../components/ui";
import { MeterTrack } from "../components/Meter";

function WindowCell({
  limits,
  pick,
  label,
  now,
}: {
  limits: MyRateLimits;
  pick: "five_hour" | "seven_day";
  label: string;
  now: number;
}) {
  if (limits.status !== "ok") return <span className="text-faint">—</span>;
  const win = limits[pick];
  const reset = limits.stale ? "stale" : (formatCountdown(win.resets_at, now) ?? "—");
  return (
    <div className="grid grid-cols-[minmax(5rem,9rem)_3rem_5.5rem] items-center gap-2.5">
      <MeterTrack className="h-1.5" label={label} fillPct={win.pct} valueText={`${win.pct}%`} dim={limits.stale} />
      <span className={cx("text-right font-mono tabular-nums", limits.stale ? "text-faint" : "text-muted")}>
        {win.pct}%
      </span>
      {/* The live countdown is data → text-muted for WCAG AA at 12px (web-ux); a
          stale row's "stale" label stays faint (de-emphasised, and the dimmed bar
          + badge already carry the staleness). */}
      <span className={cx("whitespace-nowrap text-xs", limits.stale ? "text-faint" : "text-muted")}>
        {reset}
      </span>
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
                  <th className="px-4 py-3 font-medium">5-hour window</th>
                  <th className="px-4 py-3 font-medium">7-day window</th>
                  <th className="px-4 py-3 font-medium">Status</th>
                  <th className="px-4 py-3 font-medium">Updated</th>
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
                        // the status badge under the window headers and break the
                        // table's alignment for the one row a reader is most likely
                        // to be scanning past.
                        <tr key={u.id} className="transition-colors hover:bg-raised/30">
                          <UserCell user={u} rowSpan={1} showIdentity />
                          <td className="px-4 py-3 text-xs text-faint">—</td>
                          <td className="px-4 py-3 text-xs text-faint">—</td>
                          <td className="px-4 py-3 text-xs text-faint">—</td>
                          <td className="px-4 py-3">
                            <Badge tone="neutral">no token</Badge>
                          </td>
                          <td className="px-4 py-3 text-xs text-muted">—</td>
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
                            <td className="px-4 py-3">
                              <div className="flex items-center gap-2">
                                <span className="text-xs font-medium text-fg">{t.label}</span>
                                {t.is_default && <Badge tone="neutral">default</Badge>}
                              </div>
                            </td>
                            <td className="px-4 py-3">
                              <WindowCell limits={t.limits} pick="five_hour" label="5-hour window" now={now} />
                            </td>
                            <td className="px-4 py-3">
                              <WindowCell limits={t.limits} pick="seven_day" label="7-day window" now={now} />
                            </td>
                            <td className="px-4 py-3">
                              <Badge tone={badge.tone} dot={badge.dot}>
                                {badge.label}
                              </Badge>
                            </td>
                            {/* text-muted (not faint): the "updated Xm ago" timestamp
                                must clear WCAG AA at 12px (web-ux finding). */}
                            <td className="px-4 py-3 text-xs tabular-nums text-muted">
                              {t.limits.status === "ok" ? formatAgo(t.limits.synced_at, now) : "—"}
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
