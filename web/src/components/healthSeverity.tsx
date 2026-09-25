// Shared severity presentation for the admin-health surfaces (PRD #1484, reshaped by PRD
// #1648 D6). Severity is encoded in FORM as well as colour: a distinct SHAPE and a text word
// per severity, so a screen reader and a colour-blind admin both get the signal, never
// colour alone. The shape is an 8px inline SVG drawn in `currentColor` (no font glyph, whose
// size and baseline drift per typeface):
//
//   ok      filled circle        warn     filled triangle      danger   filled diamond
//   unknown hollow circle        na       dashed hollow circle
//
// OK deliberately has a second, quieter mark: the `check` variant (no fill, no border), used
// on passing inventory chips so they do not compete with the attention marks.
//
// SeverityBadge (the shared `Badge` anatomy + shape + word) is the labelled mark every health
// surface renders; SeverityShape is exported on its own for the attention band, the non-OK
// inventory chips and the danger banner. The attention pips (sidebar Admin item and the
// Admin tab strip) use `CountPill` from ui.tsx with `healthPipLabel` from lib/healthView.

import { Badge, cx, type BadgeTone } from "./ui";

export type Sev = "ok" | "warn" | "danger" | "unknown" | "na";

// Per-severity presentation. `label` is the visible word tests assert on (and the accessible
// name of a bare SeverityShape on the Health inventory); `tone` is the shared Badge tone.
export const SEV: Record<Sev, { label: string; tone: BadgeTone }> = {
  ok: { label: "OK", tone: "ok" },
  warn: { label: "Warn", tone: "warning" },
  danger: { label: "Danger", tone: "danger" },
  unknown: { label: "Unknown", tone: "neutral" },
  na: { label: "N/A", tone: "neutral" },
};

export function sevOf(s: string): Sev {
  return (s in SEV ? s : "unknown") as Sev;
}

// The drawn form per severity, on an 8x8 viewBox. Hollow shapes inset their stroke so it
// is not clipped at the viewBox edge.
function shapePath(s: Sev) {
  switch (s) {
    case "ok":
      return <circle cx="4" cy="4" r="4" fill="currentColor" />;
    case "warn":
      return <polygon points="4,0.4 7.9,7.6 0.1,7.6" fill="currentColor" />;
    case "danger":
      return <polygon points="4,0 8,4 4,8 0,4" fill="currentColor" />;
    case "unknown":
      return <circle cx="4" cy="4" r="3.25" fill="none" stroke="currentColor" strokeWidth="1.5" />;
    case "na":
      return (
        <circle cx="4" cy="4" r="3.25" fill="none" stroke="currentColor" strokeWidth="1.5" strokeDasharray="1.7 1.3" />
      );
  }
}

// SeverityShape is the bare mark. Decorative (aria-hidden) by default, because it normally
// sits next to its word; pass `label` when the shape stands alone and must carry the
// severity itself (then it is role="img" with that aria-label). `variant="check"` draws the
// quiet OK check mark instead of the severity shape. `size` is the edge in px (default 8,
// the h-2 w-2 mark; the all-clear icon passes a larger one).
export function SeverityShape({
  severity,
  variant = "shape",
  label,
  size,
  className,
}: {
  severity: string;
  variant?: "shape" | "check";
  label?: string;
  size?: number;
  className?: string;
}) {
  const s = sevOf(severity);
  const a11y = label ? { role: "img", "aria-label": label } : { "aria-hidden": true as const };
  return (
    <svg
      viewBox="0 0 8 8"
      focusable="false"
      {...a11y}
      width={size}
      height={size}
      className={cx("inline-block shrink-0", size == null && "h-2 w-2", className)}
    >
      {variant === "check" ? (
        <path
          d="M1 4.3 3.2 6.4 7 1.8"
          fill="none"
          stroke="currentColor"
          strokeWidth="1.4"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      ) : (
        shapePath(s)
      )}
    </svg>
  );
}

// SeverityBadge is the labelled mark (shape + optional count + word) on the shared Badge
// anatomy. An unrecognised severity renders as Unknown (a stale or unexpected signal is
// never shown as green).
export function SeverityBadge({ severity, count }: { severity: string; count?: number }) {
  const s = sevOf(severity);
  const m = SEV[s];
  return (
    <Badge tone={m.tone}>
      <SeverityShape severity={s} />
      <span>{count != null ? `${count} ${m.label}` : m.label}</span>
    </Badge>
  );
}
