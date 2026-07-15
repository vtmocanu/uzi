// Shared utilization meter (PRD #53). The track/fill atom and the tone
// thresholds were LIFTED out of WorkerStats' private Bar (PRD #49) so the worker
// resource gauges and the Claude rate-limit meters speak one visual language
// (Decision 6): ok < 80% ≤ warn < 95% ≤ danger. WorkerStats now composes
// MeterTrack; the rate-limit surfaces (Settings card, sidebar micro-meter, admin
// table) reuse the same atom with their own surrounding layout.

import { cx } from "./ui";

export type MeterTone = "ok" | "warn" | "danger";

// toneFor is the single threshold vocabulary (Decision 6): warn ≥80%, danger
// ≥95%. It keys off the SAME rounded integer the bar width uses, so a 94.99% that
// rounds to "95%" is danger, not warn — the colour can never disagree with the
// number the label shows.
export function toneFor(pct: number): MeterTone {
  if (pct >= 95) return "danger";
  if (pct >= 80) return "warn";
  return "ok";
}

// Clamp a percentage to a DOM-safe [0,100] bar width — a caller may store a value
// over 100 (the worker gauge accepts up to 6400% cpu_pct), so the bar must never
// overflow its track.
export function clampPct(pct: number): number {
  return Math.max(0, Math.min(100, pct));
}

const FILL: Record<MeterTone, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  danger: "bg-danger",
};

// MeterTrack is the accessible track + fill. tone, width, and aria-valuenow all
// derive from one clamped, rounded integer. heightClass sets the bar thickness
// (surfaces differ: 8px on the Settings card, 6px in the admin table, 5px in the
// sidebar). dim fades the fill for a stale reading (the surrounding label handles
// the rest). valueText is what a screen reader reads instead of the bare "N%"
// (e.g. the byte figures for a worker, or "no reading yet" on a first tick).
export function MeterTrack({
  label,
  fillPct,
  valueText,
  className = "",
  dim = false,
}: {
  label: string;
  fillPct: number;
  valueText: string;
  className?: string;
  dim?: boolean;
}) {
  const now = Math.round(clampPct(fillPct));
  const tone = toneFor(now);
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-valuenow={now}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuetext={valueText}
      className={cx("overflow-hidden rounded-full bg-raised", className)}
    >
      <div
        className={cx("h-full rounded-full", FILL[tone], dim && "opacity-40")}
        style={{ width: `${now}%` }}
      />
    </div>
  );
}
