// Shared severity presentation for the admin-health surfaces (PRD #1484). Severity is
// encoded in FORM as well as colour: a distinct glyph and a text label per severity, so a
// screen reader and a colour-blind admin both get the signal — never colour alone. The
// Health page, the Overview admin card, the Health tab pip and the sidebar Admin pip all
// render from here so the language is identical across every surface.

import { cx } from "./ui";

export type Sev = "ok" | "warn" | "danger" | "unknown" | "na";

// Per-severity presentation: a glyph (form), a word (accessible text), and the pill classes.
// The glyph is aria-hidden; the word is the accessible name a test asserts on.
export const SEV: Record<Sev, { label: string; glyph: string; pill: string }> = {
  ok: { label: "OK", glyph: "●", pill: "border-ok/30 bg-ok/10 text-ok" },
  warn: { label: "Warn", glyph: "▲", pill: "border-warn/40 bg-warn/10 text-warn" },
  danger: { label: "Danger", glyph: "◆", pill: "border-danger/45 bg-danger/10 text-danger" },
  unknown: { label: "Unknown", glyph: "?", pill: "border-dashed border-edge-strong bg-raised text-muted" },
  na: { label: "N/A", glyph: "○", pill: "border-edge bg-transparent text-faint" },
};

export function sevOf(s: string): Sev {
  return (s in SEV ? s : "unknown") as Sev;
}

// SeverityPill is the labelled pill (glyph + word) the Health page and the Overview card
// share.
export function SeverityPill({ severity }: { severity: string }) {
  const s = sevOf(severity);
  const m = SEV[s];
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-semibold whitespace-nowrap",
        m.pill,
      )}
    >
      <span aria-hidden="true" className="w-2.5 text-center text-[10px]">
        {m.glyph}
      </span>
      {m.label}
    </span>
  );
}

// HealthPip is the "needs attention" marker the Health tab (AdminShell) and the sidebar
// Admin link both carry. A coloured dot (form: a round pip) plus the attention count.
// Severity is NOT colour-only — the count is visible text and the aria-label names the
// severity in words, so a screen reader and a colour-blind admin both get the signal.
export function HealthPip({ count, danger }: { count: number; danger: boolean }) {
  const word = danger ? "danger" : "warning";
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1 rounded-full px-1.5 text-[11px] font-semibold tabular-nums",
        danger ? "bg-danger/15 text-danger" : "bg-warn/15 text-warn",
      )}
      aria-label={`${count} health ${count === 1 ? "check" : "checks"} need attention (${word})`}
    >
      <span aria-hidden="true" className={cx("h-1.5 w-1.5 rounded-full", danger ? "bg-danger" : "bg-warn")} />
      {count}
    </span>
  );
}
