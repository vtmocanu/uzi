// Compact instance-health chip for the /overview/next header (PRD #3878). Admin-only by
// construction: it reads the ONE shared HealthStatusProvider, whose doc is null for a
// non-admin (no fetch) and before the first load, and the chip then renders nothing.
// It mirrors the severity HealthOverviewCard shows, in one line. While every check passes it
// is a plain link to the Health tab. While anything needs attention it is a popover trigger
// (issue #4166): the panel lists EVERY attention check with the same row the Overview card
// draws, and ends with an Open health link. The open/close model, focus handling and panel
// follow PeopleChip. If the last attention item clears while the panel is open, the panel
// closes and focus moves to the link the chip has become.

import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { attentionChecks, healthVerdict } from "../lib/healthView";
import { useHealthStatus } from "../lib/useAdminHealth";
import { HealthCheckRow } from "./HealthCheckRow";
import { SeverityBadge } from "./healthSeverity";

const CHIP_CLASS =
  "inline-flex max-w-full items-center gap-2 rounded-full border border-edge bg-surface px-2.5 py-1 text-xs text-fg hover:bg-raised";

export function HealthChip() {
  const { doc } = useHealthStatus();
  const [open, setOpen] = useState(false);
  const hostRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const linkRef = useRef<HTMLAnchorElement>(null);
  // Pending click-away refocus frame, cancelled on unmount and on a live clear.
  const refocusRaf = useRef(0);
  const panelId = useId();

  const attention = doc ? attentionChecks(doc) : [];
  const passing = doc != null && attention.length === 0;

  const close = useCallback((returnFocus: boolean) => {
    setOpen(false);
    if (returnFocus) triggerRef.current?.focus();
  }, []);

  // The chip sits left of centre in the header, so a panel right-aligned to it can run past the
  // viewport's left edge at sm+. Prefer the chip's right edge, then its left edge, and only then
  // clamp into the visible width, so the panel stays attached to the chip and off the sidebar.
  // Below sm the fixed bottom sheet places itself, so no inline style there (it would override
  // inset-x-4). The query uses Tailwind's rem breakpoint so both switch at the same width.
  const [panelLeft, setPanelLeft] = useState<number | null>(null);
  useLayoutEffect(() => {
    if (!open) {
      setPanelLeft(null);
      return;
    }
    // jsdom has no matchMedia: treat it as "not sm+" and leave the CSS placement alone.
    const mq = window.matchMedia?.("(min-width: 40rem)");
    const place = () => {
      const host = hostRef.current;
      const panel = panelRef.current;
      if (!host || !panel || !mq?.matches) {
        setPanelLeft(null);
        return;
      }
      const hostRect = host.getBoundingClientRect();
      const width = panel.offsetWidth;
      const max = document.documentElement.clientWidth - 16 - width;
      const preferred = hostRect.right - width >= 16 ? hostRect.right - width : hostRect.left;
      const abs = Math.max(16, Math.min(preferred, max));
      setPanelLeft(abs - hostRect.left);
    };
    place();
    window.addEventListener("resize", place);
    return () => window.removeEventListener("resize", place);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    panelRef.current?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close(true);
    };
    const onDown = (e: MouseEvent) => {
      if (hostRef.current && !hostRef.current.contains(e.target as Node)) {
        setOpen(false);
        // The native mousedown moves focus to <body> after this handler, so refocus a frame later,
        // but only when focus was left on nothing: a control the user clicked keeps it.
        refocusRaf.current = requestAnimationFrame(() => {
          const a = document.activeElement;
          if (a == null || a === document.body) triggerRef.current?.focus({ preventScroll: true });
        });
      }
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onDown);
    };
  }, [open, close]);

  // Live clear: the panel was open and the last attention item went away. The chip has already
  // rendered as the link in this commit, so move focus to it. Gated on `open`, so a poll that
  // clears attention while the panel is closed never moves focus.
  useEffect(() => {
    if (!passing || !open) return;
    setOpen(false);
    if (refocusRaf.current) cancelAnimationFrame(refocusRaf.current);
    linkRef.current?.focus({ preventScroll: true });
  }, [passing, open]);

  useEffect(
    () => () => {
      if (refocusRaf.current) cancelAnimationFrame(refocusRaf.current);
    },
    [],
  );

  if (!doc) return null;

  const applicable = doc.checks.length - doc.counts.na;
  const text = passing
    ? `${doc.counts.ok} of ${applicable} checks passing`
    : healthVerdict(doc).title;
  // Same severity the card colours by: danger, otherwise warn for any attention item.
  const severity = passing ? "ok" : doc.status === "danger" ? "danger" : "warn";

  if (passing) {
    return (
      <Link ref={linkRef} to="/admin/health" aria-label={`System health: ${text}`} className={CHIP_CLASS}>
        <SeverityBadge severity={severity} />
        <span className="min-w-0 truncate">{text}</span>
      </Link>
    );
  }

  return (
    <div ref={hostRef} className="relative inline-block min-w-0 max-w-full">
      <button
        ref={triggerRef}
        type="button"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={open ? panelId : undefined}
        aria-label={`System health: ${text}`}
        onClick={() => setOpen((v) => !v)}
        className={CHIP_CLASS}
      >
        <SeverityBadge severity={severity} />
        <span className="min-w-0 truncate">{text}</span>
      </button>
      {open && (
        <div
          id={panelId}
          ref={panelRef}
          role="dialog"
          aria-label="System health"
          tabIndex={-1}
          style={panelLeft == null ? undefined : { left: panelLeft, right: "auto" }}
          // Below the chip at sm+ (placed by the layout effect above), a bottom sheet below sm.
          className="absolute right-0 top-full z-30 mt-2 max-h-[70vh] w-[min(40rem,calc(100vw-32px))] overflow-y-auto rounded-xl border border-edge bg-raised text-left shadow-2xl outline-hidden max-sm:fixed max-sm:inset-x-4 max-sm:bottom-4 max-sm:top-auto max-sm:mt-0 max-sm:max-h-[calc(100dvh-2rem)] max-sm:w-auto"
        >
          <div className="space-y-3 p-3">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <SeverityBadge severity={severity} />
              <h3 className="text-sm font-semibold text-fg">System health: {healthVerdict(doc).title}</h3>
            </div>
            <ul className="space-y-1.5">
              {attention.map((c) => (
                <HealthCheckRow key={c.id} check={c} />
              ))}
            </ul>
          </div>
          <div className="sticky bottom-0 flex items-center justify-end border-t border-edge bg-raised px-3 py-2">
            <Link to="/admin/health" className="shrink-0 text-sm text-info hover:underline">
              Open health
            </Link>
          </div>
        </div>
      )}
    </div>
  );
}
