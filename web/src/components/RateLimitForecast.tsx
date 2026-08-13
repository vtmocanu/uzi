// RateLimitForecastMeter (PRD #309): the shared MeterTrack atom PLUS a burn-rate
// forecast overlay — a translucent "ghost" extending the bar to its projected
// landing point, and a » overflow marker when that projection lands past the cap.
//
// When the forecast is "safe" (headroom, or suppressed / not enough signal), it
// renders EXACTLY the plain MeterTrack — a quiet row is byte-identical to today
// (D3, silence = safe). The atom is COMPOSED, never modified (D9), so WorkerStats'
// cpu/mem gauges are untouched. The projected % is hover/aria-only (D4): it rides
// the atom's aria-valuetext and the container title, never printed inline.
import { clampPct, MeterTrack } from "./Meter";
import { cx } from "./ui";
import type { PaceForecast } from "../lib/rateLimits";

// Ghost tone per forecast state — translucent so the track shows through, mirroring
// the atom's bg-warn / bg-danger fills (Meter.tsx): gold on pace, coral over.
const GHOST: Record<"over" | "on_pace", string> = {
  on_pace: "bg-warn/40",
  over: "bg-danger/40",
};

export function RateLimitForecastMeter({
  label,
  pct,
  valueText,
  forecast,
  className = "",
  dim = false,
}: {
  label: string;
  pct: number;
  valueText: string;
  forecast: PaceForecast;
  className?: string;
  dim?: boolean;
}) {
  // Safe / silent → the plain atom, unchanged. No wrapper, no overlay, no title.
  if (forecast.state === "safe") {
    return <MeterTrack label={label} fillPct={pct} valueText={valueText} className={className} dim={dim} />;
  }

  const fill = clampPct(pct);
  const target = clampPct(forecast.projectedPct); // ghost's right edge, capped at the cap (100)
  const ghostWidth = Math.max(0, target - fill);
  const over = forecast.state === "over";
  // » appears whenever the projection lands PAST the cap (D2/M2: projected > 100),
  // whether the band is "over" (>115) or a high "on pace" (100–115).
  const showMarker = forecast.projectedPct > 100;
  const forecastText = `${valueText} — projected ${forecast.projectedPct}% by reset, ${over ? "over" : "on pace"}`;

  return (
    <div className={cx("relative", className)} title={forecastText}>
      <MeterTrack label={label} fillPct={pct} valueText={forecastText} className="h-full" dim={dim} />
      {/* Ghost: translucent extension from the current fill to the projected landing
          point, in the pace tone. A subtle grow, disabled under reduced motion. */}
      <div
        aria-hidden
        className={cx(
          "pointer-events-none absolute inset-y-0 rounded-full transition-[width,left] duration-500 ease-out motion-reduce:transition-none",
          GHOST[forecast.state],
        )}
        style={{ left: `${fill}%`, width: `${ghostWidth}%` }}
      />
      {/* Overflow marker: a » at the cap — a non-color SHAPE cue, redundant with the
          coral/gold tone (accessibility). May be clipped visually at the tightest
          sidebar width without losing the colour signal (PRD). */}
      {showMarker && (
        <span
          aria-hidden
          // -translate-y-[62%], not -1/2: the » ink is bottom-heavy in its line box
          // under leading-none, so centering the BOX leaves the glyph ~1.24px low on
          // the bar midline. -62% (≈-6.2px on the 10px box) re-centers the ink.
          className={cx(
            "pointer-events-none absolute right-0 top-1/2 -translate-y-[62%] text-[10px] font-bold leading-none",
            over ? "text-danger" : "text-warn",
          )}
        >
          »
        </span>
      )}
    </div>
  );
}
