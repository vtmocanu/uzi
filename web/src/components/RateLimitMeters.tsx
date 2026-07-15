// The two "my own limits" surfaces of PRD #53: the Settings → "Claude limits"
// card and the sidebar-footer micro-meters. Both read GET /me/rate-limits (the
// token never leaves the api — the SPA only ever sees percentages) and reuse the
// shared MeterTrack + toneFor thresholds. The admin table is a separate page.

import { useCallback, useEffect, useState } from "react";
import { api, type MyRateLimits, type RateLimitWindow } from "../lib/api";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { formatAgo, formatCountdown, useNow } from "../lib/rateLimits";
import { Badge, Card, SectionTitle } from "./ui";
import { MeterTrack } from "./Meter";

// useMyRateLimits polls GET /me/rate-limits while the tab is visible. A failed
// fetch keeps the last reading (never blanks the meters); the surfaces decide what
// to render per status. intervalMs differs per surface — the data changes at most
// once per server poll interval, so a page's usual 10s cadence would be pure
// amplification.
export function useMyRateLimits(intervalMs: number): { data: MyRateLimits | null; loading: boolean } {
  const [data, setData] = useState<MyRateLimits | null>(null);
  const [loading, setLoading] = useState(true);
  const load = useCallback(() => {
    api
      .getMyRateLimits()
      .then((d) => {
        setData(d);
        setLoading(false);
      })
      .catch(() => setLoading(false));
  }, []);
  useEffect(() => {
    load();
  }, [load]);
  usePollWhileVisible(load, intervalMs);
  return { data, loading };
}

const CARD_BLURB =
  "Live utilization of your Anthropic account's two rate-limit windows. Runs queue when a window is exhausted and resume after it resets.";

function SettingsWindowRow({ label, win, now }: { label: string; win: RateLimitWindow; now: number }) {
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
      <MeterTrack
        className="mt-1.5 h-2"
        label={label}
        fillPct={win.pct}
        valueText={`${win.pct}%${countdown ? `, resets in ${countdown}` : ""}`}
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

// RateLimitCard is the Settings → Account & token card (mockup frame A). Hidden
// entirely when the user has no token stored (and while the first read is in
// flight, to avoid a flash under the token card). On "unavailable" it shows the
// two windows greyed with a neutral badge; on "ok" it renders the live meters.
export function RateLimitCard() {
  const { data, loading } = useMyRateLimits(60_000);
  const now = useNow();

  if (loading || !data || data.status === "no_token") return null;

  if (data.status === "unavailable") {
    return (
      <Card className="space-y-5">
        <div>
          <SectionTitle>Claude limits</SectionTitle>
          <p className="mt-2 text-sm text-muted">{CARD_BLURB}</p>
        </div>
        <div>
          <UnavailableWindowRow label="5-hour window" />
          <UnavailableWindowRow label="7-day window" />
        </div>
        <div className="flex items-center gap-2 text-xs text-faint">
          <Badge tone="neutral">No reading yet</Badge>
          <span>a reading appears within a few minutes of saving your token</span>
        </div>
      </Card>
    );
  }

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Claude limits</SectionTitle>
        <p className="mt-2 text-sm text-muted">{CARD_BLURB}</p>
      </div>
      <div>
        <SettingsWindowRow label="5-hour window" win={data.five_hour} now={now} />
        <SettingsWindowRow label="7-day window" win={data.seven_day} now={now} />
      </div>
      {/* text-muted (not text-faint) so the "updated Xm ago" timestamp — data
          this page leans on — clears WCAG AA 4.5:1 at 12px (web-ux finding). */}
      <div className="flex items-center gap-2 text-xs text-muted">
        {data.stale ? <Badge tone="neutral">Stale</Badge> : <Badge tone="ok" dot>Live</Badge>}
        <span>
          updated {formatAgo(data.synced_at, now)}
          {data.stale ? " · reading is stale (vault locked or polling off)" : " · refreshes every few minutes"}
        </span>
      </div>
    </Card>
  );
}

function MicroRow({ label, win, dim }: { label: string; win: RateLimitWindow; dim: boolean }) {
  return (
    <div className="grid grid-cols-[1.4rem_1fr_2.6rem] items-center gap-2 text-[11px]">
      <span className="font-mono text-faint">{label}</span>
      <MeterTrack className="h-[5px]" label={`${label} window`} fillPct={win.pct} valueText={`${win.pct}%`} dim={dim} />
      <span className="text-right font-mono tabular-nums text-muted">{win.pct}%</span>
    </div>
  );
}

// SidebarRateLimits is the two 5px micro-bars under the signed-in user block
// (mockup frame B). Hidden for no_token / unavailable (no dead chrome) and while
// loading; a stale reading is shown dimmed. Hover title carries the reset
// countdowns.
export function SidebarRateLimits() {
  const { data } = useMyRateLimits(60_000);
  if (!data || data.status !== "ok") return null;
  const c5 = formatCountdown(data.five_hour.resets_at);
  const c7 = formatCountdown(data.seven_day.resets_at);
  const title = [c5 && `5h resets in ${c5}`, c7 && `7d resets in ${c7}`].filter(Boolean).join(" · ");
  return (
    <div className="mt-2 space-y-1.5" title={title || undefined} aria-label="Claude rate limits">
      <MicroRow label="5h" win={data.five_hour} dim={data.stale} />
      <MicroRow label="7d" win={data.seven_day} dim={data.stale} />
    </div>
  );
}
