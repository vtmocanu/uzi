// Compact instance-health chip for the /overview/next header (PRD #3878). Admin-only by
// construction: it reads the ONE shared HealthStatusProvider, whose doc is null for a
// non-admin (no fetch) and before the first load, and the chip then renders nothing.
// It mirrors HealthOverviewCard's severity in one line and always opens a popover (#4281).
// Attention rows use the card's shared row (#4166); passing checks appear by area. The
// footer's Open health link reaches the full Health tab. Live verdict changes keep the
// popover open and move focus only if the previously focused item disappeared.

import { useCallback, useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { attentionChecks, healthVerdict } from "../lib/healthView";
import { formatAgo } from "../lib/rateLimits";
import { useHealthStatus } from "../lib/useAdminHealth";
import { useNow } from "../lib/useNow";
import { HealthCheckRow } from "./HealthCheckRow";
import { HealthPassingChecks } from "./HealthPassingChecks";
import { SeverityBadge } from "./healthSeverity";

const CHIP_CLASS =
  "inline-flex max-w-full items-center gap-2 rounded-full border border-edge bg-surface px-2.5 py-1 text-xs text-fg hover:bg-raised";

export function HealthChip() {
  const { doc } = useHealthStatus();
  const [open, setOpen] = useState(false);
  const hostRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  // Pending click-away refocus frame, cancelled on unmount.
  const refocusRaf = useRef(0);
  const focusedPanelItem = useRef<HTMLElement | null>(null);
  const panelId = useId();
  // Age the checked time even if the health provider's fetch stalls. This is only a clock;
  // it neither polls nor changes the health document.
  const now = useNow(10_000);

  const attention = doc ? attentionChecks(doc) : [];
  const passing = doc != null && attention.length === 0;

  // Remember which item held focus. A removed focused node can drop document.activeElement to
  // <body> before a layout effect can inspect it; a focus move outside the dialog clears this
  // ref, so a background health refresh never steals the user's new focus.
  useEffect(() => {
    if (!open) return;
    const trackFocus = (event: FocusEvent) => {
      const target = event.target;
      focusedPanelItem.current = target instanceof HTMLElement && panelRef.current?.contains(target) ? target : null;
    };
    document.addEventListener("focusin", trackFocus);
    return () => {
      document.removeEventListener("focusin", trackFocus);
      focusedPanelItem.current = null;
    };
  }, [open]);
  useLayoutEffect(() => {
    if (!open) return;
    const oldFocus = focusedPanelItem.current;
    const panel = panelRef.current;
    if (oldFocus && !oldFocus.isConnected && panel && !panel.contains(document.activeElement)) {
      panel.focus({ preventScroll: true });
    }
  }, [doc, passing, open]);

  const close = useCallback((returnFocus: boolean) => {
    setOpen(false);
    if (returnFocus) triggerRef.current?.focus();
  }, []);

  // At sm+ the Overview panel aligns with the shell's content column. A standalone chip has no
  // column anchor, so it keeps the original chip-attached placement. Below sm the fixed bottom
  // sheet places itself with CSS: no inline left or width may override its insets.
  const [panelPlacement, setPanelPlacement] = useState<{ left: number; width?: number } | null>(null);
  useLayoutEffect(() => {
    if (!open) {
      setPanelPlacement(null);
      return;
    }
    // jsdom has no matchMedia: treat it as "not sm+" and leave the CSS placement alone.
    const mq = window.matchMedia?.("(min-width: 40rem)");
    const place = () => {
      const host = hostRef.current;
      const panel = panelRef.current;
      if (!host || !panel || !mq?.matches) {
        setPanelPlacement(null);
        return;
      }
      const hostRect = host.getBoundingClientRect();
      const column = host.closest<HTMLElement>("[data-page-content-column]");
      if (column) {
        const rect = column.getBoundingClientRect();
        setPanelPlacement({ left: rect.left - hostRect.left, width: rect.width });
        return;
      }
      // No shell anchor (for example a standalone component): preserve the original placement.
      const width = panel.offsetWidth;
      // Floor at the content column (the shell's <main>), so a fixed sidebar is never covered.
      const minLeft = Math.max(16, host.closest("main")?.getBoundingClientRect().left ?? 0);
      const max = document.documentElement.clientWidth - 16 - width;
      const preferred = hostRect.right - width >= minLeft ? hostRect.right - width : hostRect.left;
      const abs = Math.max(16, Math.min(Math.max(preferred, minLeft), max));
      setPanelPlacement({ left: abs - hostRect.left });
    };
    place();
    window.addEventListener("resize", place);
    // The header is a wrapping flex row with live content. A sibling, the chip, or the row can
    // change size and move the chip without a window resize; the offset is relative to the chip.
    const ro = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(() => place());
    const row = hostRef.current?.parentElement;
    if (ro && row) {
      ro.observe(row);
      if (row.parentElement) ro.observe(row.parentElement);
      for (const el of Array.from(row.children)) ro.observe(el);
    }
    return () => {
      window.removeEventListener("resize", place);
      ro?.disconnect();
    };
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
  const checkedAt = Date.parse(doc.checked_at);
  const checked = Number.isFinite(checkedAt) ? `checked ${formatAgo(doc.checked_at, now)}` : null;
  const passingChecks = doc.checks.filter((c) => c.severity === "ok");

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
          style={panelPlacement == null ? undefined : { left: panelPlacement.left, width: panelPlacement.width, right: "auto" }}
          // Below the chip at sm+ (placed by the layout effect above), a bottom sheet below sm.
          className="absolute right-0 top-full z-30 mt-2 max-h-[70vh] w-[min(40rem,calc(100vw-32px))] overflow-y-auto rounded-xl border border-edge bg-raised text-left shadow-2xl outline-hidden max-sm:fixed max-sm:inset-x-4 max-sm:bottom-4 max-sm:top-auto max-sm:mt-0 max-sm:max-h-[calc(100dvh-2rem)] max-sm:w-auto"
        >
          <div className="space-y-3 p-3">
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <SeverityBadge severity={severity} />
              <h3 className="text-sm font-semibold text-fg">
                System health: {passing ? `all ${applicable} ${applicable === 1 ? "check" : "checks"} passing` : healthVerdict(doc).title}
              </h3>
              {checked && <span className="ml-auto text-xs tabular-nums text-faint">{checked}</span>}
            </div>
            {passing ? (
              <>
                <HealthPassingChecks checks={passingChecks} />
                {doc.counts.na > 0 && <p className="text-xs text-faint">{doc.counts.na} not applicable</p>}
              </>
            ) : (
              <>
                <ul data-health-attention-list className="space-y-1.5">
                  {attention.map((c) => (
                    <HealthCheckRow key={c.id} check={c} twoColumnDetails />
                  ))}
                </ul>
                {passingChecks.length > 0 && (
                  <details className="border-t border-edge pt-2 text-xs text-muted">
                    <summary className="cursor-pointer rounded-sm focus-visible:outline focus-visible:outline-2 focus-visible:outline-brand">
                      {passingChecks.length} other {passingChecks.length === 1 ? "check" : "checks"} passing
                    </summary>
                    <div className="mt-2"><HealthPassingChecks checks={passingChecks} /></div>
                  </details>
                )}
              </>
            )}
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
