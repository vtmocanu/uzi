// RateLimitForecastMeter (PRD #309): a utilization bar PLUS a burn-rate forecast —
// a translucent "ghost" extending the bar to its projected landing point, and a »
// overflow marker when that projection lands past the cap.
//
// When the forecast is "safe" (headroom, or suppressed / not enough signal), it
// renders EXACTLY the plain MeterTrack — a quiet row is byte-identical to today
// (D3, silence = safe), and the shared atom + WorkerStats' cpu/mem gauges are
// untouched (D9). When there IS a forecast, the wrapper OWNS the track+ghost+fill
// itself (it cannot compose MeterTrack here — the atom bundles the track and fill
// as one opaque unit, leaving no seam to slip a ghost BETWEEN them; see the layer
// note on the non-safe return). This keeps D9's real guarantee — Meter.tsx and the
// worker gauges never change — while letting the projection ghost sit behind an
// opaque fill so the two never blend or notch at their junction. The projected % is
// hover/aria-only (D4): it rides aria-valuetext and the container title, never inline.
import { clampPct, FILL, MeterTrack, toneFor } from "./Meter";
import { ChevronsRightIcon } from "./icons";
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
  const now = Math.round(fill); // aria-valuenow + tone band, from the same clamped value
  const target = clampPct(forecast.projectedPct); // ghost's right edge, capped at the cap (100)
  const over = forecast.state === "over";
  // » appears whenever the projection lands PAST the cap (D2/M2: projected > 100),
  // whether the band is "over" (>115) or a high "on pace" (100–115).
  const showMarker = forecast.projectedPct > 100;
  const forecastText = `${valueText} — projected ${forecast.projectedPct}% by reset, ${over ? "over" : "on pace"}`;

  return (
    <div className={cx("relative", className)} title={forecastText}>
      {/* Wrapper-owned track (NOT MeterTrack) so the ghost can be LAYERED behind an
          opaque fill: the fill hides the 0→fill overlap (no cross-tone orange blend)
          and its rounded cap emerges from over the continuous ghost (no same-tone dark
          notch). The atom + worker gauges are untouched; the safe branch above still
          renders the plain <MeterTrack/>. aria mirrors the atom's, from the same
          clamped/rounded value, so a screen reader reads one progressbar. */}
      <div
        role="progressbar"
        aria-label={label}
        aria-valuenow={now}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuetext={forecastText}
        className="relative h-full overflow-hidden rounded-full bg-raised"
      >
        {/* Ghost — painted FIRST (behind): a continuous run from 0 to the projected
            landing point (capped at the cap), rounded on the far/projected end. A
            subtle grow, disabled under reduced motion. */}
        <div
          aria-hidden
          className={cx(
            "absolute inset-y-0 left-0 rounded-r-full transition-[width] duration-500 ease-out motion-reduce:transition-none",
            GHOST[forecast.state],
          )}
          style={{ width: `${target}%` }}
        />
        {/* Fill — current usage, painted AFTER (opaque, on top). MUST be positioned:
            an in-flow block paints BEFORE positioned siblings, so an in-flow fill
            would render UNDER the ghost and get tinted (orange in the cross-tone
            case). Absolute + DOM order (ghost, then fill) gives the right paint order. */}
        <div
          aria-hidden
          className={cx("absolute inset-y-0 left-0 rounded-full", FILL[toneFor(now)], dim && "opacity-40")}
          style={{ width: `${fill}%` }}
        />
      </div>
      {/* Overflow marker: a » flagging a projection past the cap, whose POSITION
          encodes severity (design mock). "over" (coral) sits just OUTSIDE the bar's
          right edge, in the row's dark margin — reads as "overshooting past the end",
          and stays visible on the dark background even at a near-full coral fill (where
          a marker on the bar end would vanish into the fill). "on pace" (gold) sits
          just after the FILL's end (current-usage boundary), pointing into the ghost
          toward the projection — reads as "here, drifting toward the cap". An SVG glyph
          is viewBox-centred, so vertical alignment needs no font-metric nudge; flex
          centres it on the bar midline. The outside marker may clip at the tightest
          sidebar width without losing the colour signal (PRD). */}
      {showMarker && (
        <span
          aria-hidden
          data-testid="forecast-overflow-marker"
          className={cx(
            "pointer-events-none absolute top-1/2 flex -translate-y-1/2 items-center text-[10px]",
            over ? "left-full ml-1 text-danger" : "text-warn",
          )}
          // on pace → anchored flush at the fill's end (no gap); over → the left-full
          // class handles it, so no inline left.
          style={over ? undefined : { left: `${fill}%` }}
        >
          <ChevronsRightIcon />
        </span>
      )}
    </div>
  );
}
