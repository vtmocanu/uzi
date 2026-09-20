// App-wide instance-health danger banner (PRD #1484 M5, D2). Mounted once in AppShell beside
// UpdateEscalationBanner, so it appears on every page. Admins only, DANGER only. It reads the
// ONE shared HealthStatusProvider, so a non-admin never fetches the admin endpoint (doc is
// null for them) and there is no second poll here.
//
// It shows the verdict line, the top danger cause, an "Open health" link, and a "Snooze 1 h"
// action. The Snooze button renders ONLY while an episode is open (episode_id non-null). The
// snooze is TIME-BASED and keyed to the episode: it honours 1 h (D2 — the server stores
// snoozed_until = now + 1 h), not the whole episode, so once that hour lapses the banner
// returns if the instance is still in danger. A NEW episode (a different episode_id, whose
// snoozed_until is null) shows it again immediately.
//
// The hide is `now < effectiveSnoozedUntil`, where effectiveSnoozedUntil is the LATER of the
// server's snoozed_until (parsed) and a LOCAL optimistic snoozed-until (now + 1 h) set on the
// Snooze click and keyed to the current episode_id — so the click hides at once (optimistic),
// the next poll's server snoozed_until keeps it hidden, both lapse together after the hour,
// and a new episode_id drops the stale local value.
//
// `now` is injectable so the 1 h lapse is testable without waiting; it defaults to Date.now.

import { useState } from "react";
import { Link } from "react-router-dom";

import { api } from "../lib/api";
import { healthVerdict } from "../lib/healthView";
import { useHealthStatus } from "../lib/useAdminHealth";
import { Button, cx } from "./ui";

const SNOOZE_MS = 60 * 60 * 1000; // 1 h (D2)

export function HealthDangerBanner({ now = Date.now }: { now?: () => number } = {}) {
  const { doc } = useHealthStatus();
  // The optimistic snooze this session set on the Snooze click: the episode it applies to and
  // the instant (now + 1 h) it lapses at. Keyed to the episode so a NEW episode (different id)
  // is never covered by a stale optimistic hide — the banner returns at once, before the
  // snooze round-trip, and the next poll's snoozed_until confirms it.
  const [optimistic, setOptimistic] = useState<{ episode: string; until: number } | null>(null);
  const [snoozing, setSnoozing] = useState(false);

  // null doc = a non-admin (no fetch) or before the first load. Follow `status` so the banner
  // appears the moment the instance crosses into danger.
  if (!doc || doc.status !== "danger") return null;

  const nowMs = now();
  // The caller's own server-confirmed snooze for the current episode (0 when absent), and the
  // local optimistic snooze, honoured ONLY while its episode matches the current one.
  const serverUntil = doc.snoozed_until != null ? Date.parse(doc.snoozed_until) : 0;
  const localUntil = optimistic != null && optimistic.episode === doc.episode_id ? optimistic.until : 0;
  // Hidden while now is before the LATER of the two snoozes; after 1 h both lapse and, if the
  // instance is still in danger, the banner returns.
  const effectiveSnoozedUntil = Math.max(serverUntil, localUntil);
  if (effectiveSnoozedUntil > nowMs) return null;

  const verdict = healthVerdict(doc.status, doc.counts);
  // The cause line is the worst check's own server-authored summary (danger sorts first). It
  // is never free text: the server composes every summary from a fixed template plus numbers.
  const topDanger = doc.checks.find((c) => c.severity === "danger");

  const snooze = async () => {
    setSnoozing(true);
    // Optimistically hide right away for 1 h, keyed to this episode; the next poll's
    // snoozed_until confirms it, and a new episode drops this stale value.
    if (doc.episode_id != null) {
      setOptimistic({ episode: doc.episode_id, until: now() + SNOOZE_MS });
    }
    try {
      await api.snoozeAdminHealth();
    } catch {
      // The local hide already took effect; a failed snooze just means the banner may return
      // on the next poll — acceptable for a nudge that also lives on the Health tab.
    } finally {
      setSnoozing(false);
    }
  };

  return (
    <div
      role="alert"
      // Scoped handle: RateLimitAnnouncer owns a role="status" and UpdateEscalationBanner a
      // role="alert", so tests select this banner by its label, not a bare role query.
      aria-label="Instance health"
      className={cx("mb-6 rounded-lg border px-4 py-3", "border-danger/40 bg-danger/10")}
    >
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <div className="min-w-0">
          <p className="text-sm font-semibold text-fg">
            <span aria-hidden="true" className="text-danger">
              ◆
            </span>{" "}
            {verdict.title}
          </p>
          {topDanger && <p className="mt-0.5 text-sm text-muted">{topDanger.summary}</p>}
        </div>
        <div className="flex shrink-0 flex-wrap items-center gap-3">
          <Link to="/admin/health" className="text-sm text-info hover:underline">
            Open health
          </Link>
          {/* The snooze is keyed to the open episode, so the button only makes sense while one
              is open. episode_id opens within one evaluation of danger (M2 evaluator). */}
          {doc.episode_id != null && (
            <Button variant="secondary" size="sm" onClick={snooze} disabled={snoozing}>
              {snoozing ? "Snoozing…" : "Snooze 1 h"}
            </Button>
          )}
        </div>
      </div>
    </div>
  );
}
