// EnableJobDialog — a Job catalog card's "Enable on…" button and its repo-picker popover
// (PRD #1645 D7).
//
// The popover follows ExtendTimePopover's dialog pattern: focus moves into the panel when
// it opens, Escape and an outside click close it, and focus returns to the trigger. It
// lists the owner's repos as checkboxes. A repo already enabled for this entry renders
// checked, disabled and labelled "enabled", so Enable can never be a no-op; the primary
// button enables exactly the newly checked repos.
//
// The pre-enable label check (D7, binding): for a sweep entry with selector labels, a
// SweepLabelWarn renders for every newly checked repo and reports its check state. Enable
// is held ("Checking labels…") until every newly selected repo reports "done", by two
// redundant layers: the checkbox handler records the repo as "checking" in the SAME state
// update that selects it, and the hold reads any state other than "done", none recorded
// included, as checking. EnableJobDialog.test.tsx stubs the warn silent and pins that the
// dialog holds Enable without the warn's help; it does not tell the two layers apart.
// Once every selected repo is "done" Enable is offered whatever the result: the guardrail
// is advisory, never blocking.
//
// Partial failure: the dialog stays open, repos that succeeded lock as "enabled", failed
// ones stay checked with their error inline, and Enable retries only those.

import { useEffect, useId, useRef, useState } from "react";
import type { CatalogEntry, Repo } from "../lib/api";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";
import { SweepLabelWarn } from "./SweepLabelWarn";
import { Badge, Button, cx } from "./ui";

// The per-repo outcome of one enable fan-out (Schedules.tsx `enableDefault`).
export interface EnableResult {
  enabled: string[];
  failed: { repoId: string; message: string }[];
}

type CheckState = "checking" | "done";

// The filter input appears once the owner has more repos than fit at a glance (D7).
const FILTER_THRESHOLD = 8;

export function EnableJobDialog({
  entry,
  repos,
  enabledRepoIds,
  busy,
  open,
  onOpenChange,
  onEnable,
}: {
  entry: CatalogEntry;
  repos: Repo[];
  // Repos with an origin-default row for this entry (on or paused): locked in the list.
  enabledRepoIds: ReadonlySet<string>;
  // Another mutation is in flight on the page: Enable waits for it.
  busy: boolean;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  // Enable on exactly these repos. Resolves to the per-repo outcome, or null when the page
  // refused to start (another mutation in flight), which leaves the dialog as it was.
  onEnable: (repoIds: string[]) => Promise<EnableResult | null>;
}) {
  const hostRef = useRef<HTMLDivElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const refocusRaf = useRef(0);
  const [shiftPx, setShiftPx] = useState(0);

  const focusTrigger = () => hostRef.current?.querySelector<HTMLButtonElement>(":scope > button")?.focus();
  const close = () => {
    onOpenChange(false);
    focusTrigger();
  };

  // Focus in on open (the panel itself, tabIndex -1, so the first Tab lands on a control
  // inside), Escape and outside click close and return focus to the trigger. Copied from
  // ExtendTimePopover, including the deferred refocus after a native mousedown.
  useEffect(() => {
    if (!open) return;
    const panel = panelRef.current;
    panel?.scrollIntoView?.({ block: "nearest" });
    panel?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        onOpenChange(false);
        focusTrigger();
      }
    };
    const onDown = (e: MouseEvent) => {
      if (hostRef.current && !hostRef.current.contains(e.target as Node)) {
        onOpenChange(false);
        refocusRaf.current = requestAnimationFrame(focusTrigger);
      }
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onDown);
    };
    // onOpenChange is the parent's setter wrapper; re-subscribing on its identity is noise.
  }, [open]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(
    () => () => {
      if (refocusRaf.current) cancelAnimationFrame(refocusRaf.current);
    },
    [],
  );

  // Keep the panel on-screen at phone width (ExtendTimePopover's clamp; a no-op in jsdom).
  useEffect(() => {
    if (!open) {
      setShiftPx(0);
      return;
    }
    const clamp = () => {
      const host = hostRef.current;
      const panel = panelRef.current;
      if (!host || !panel) return;
      const vw = document.documentElement.clientWidth;
      if (!vw) return;
      const margin = 8;
      const hostRight = host.getBoundingClientRect().right;
      const naturalLeft = hostRight - panel.offsetWidth;
      let shift = 0;
      if (naturalLeft < margin) shift = margin - naturalLeft;
      else if (hostRight > vw - margin) shift = vw - margin - hostRight;
      setShiftPx(shift);
    };
    clamp();
    window.addEventListener("resize", clamp);
    return () => window.removeEventListener("resize", clamp);
  }, [open]);

  return (
    <div ref={hostRef} className="relative inline-block">
      <Button
        type="button"
        variant="secondary"
        size="sm"
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-label={`Enable ${entry.name} on…`}
        onClick={() => (open ? close() : onOpenChange(true))}
      >
        Enable on…
      </Button>
      {open && (
        <EnablePanel
          panelRef={panelRef}
          shiftPx={shiftPx}
          entry={entry}
          repos={repos}
          enabledRepoIds={enabledRepoIds}
          busy={busy}
          onEnable={onEnable}
          onDone={close}
        />
      )}
    </div>
  );
}

// The panel's state lives here, so each opening starts fresh while a partial failure
// (the panel stays mounted) keeps its selection and errors.
function EnablePanel({
  panelRef,
  shiftPx,
  entry,
  repos,
  enabledRepoIds,
  busy,
  onEnable,
  onDone,
}: {
  panelRef: React.RefObject<HTMLDivElement | null>;
  shiftPx: number;
  entry: CatalogEntry;
  repos: Repo[];
  enabledRepoIds: ReadonlySet<string>;
  busy: boolean;
  onEnable: (repoIds: string[]) => Promise<EnableResult | null>;
  onDone: () => void;
}) {
  const demo = useDemoMode();
  const headingId = useId();
  // The selection and each selected repo's label-check state are ONE state value, so
  // checking a repo and marking it "checking" can never land in different renders.
  const [pick, setPick] = useState<{ selected: string[]; checks: Record<string, CheckState> }>({
    selected: [],
    checks: {},
  });
  const [errors, setErrors] = useState<Record<string, string>>({});
  // Repos this dialog enabled that the page's list may not show yet (a failed reload):
  // locked like the enabled ones, so a retry never re-sends them.
  const [justEnabled, setJustEnabled] = useState<string[]>([]);
  const [query, setQuery] = useState("");
  const [submitting, setSubmitting] = useState(false);
  // Set synchronously on submit: a second click delivered before the re-render that
  // disables Enable would still see the old `canEnable` and fan out twice.
  const inFlight = useRef(false);
  // After a partial failure: the failed repos, whose first listed checkbox takes focus once
  // the re-render has enabled it again (Enable, which held focus, stays disabled).
  const [focusFailed, setFocusFailed] = useState<string[] | null>(null);
  const alive = useRef(true);
  useEffect(
    () => () => {
      alive.current = false;
    },
    [],
  );

  // A selected repo that dropped out of `repos` (a reload after it was disconnected) is
  // pruned from the selection (and its error) whenever the list changes, so it neither
  // counts in "Enable N", nor holds Enable on a check whose warn no longer renders, nor
  // comes back selected if it reappears.
  useEffect(() => {
    const present = new Set(repos.map((r) => r.id));
    setPick((p) =>
      p.selected.every((id) => present.has(id))
        ? p
        : {
            selected: p.selected.filter((id) => present.has(id)),
            checks: Object.fromEntries(Object.entries(p.checks).filter(([id]) => present.has(id))),
          },
    );
    setErrors((e) =>
      Object.keys(e).every((id) => present.has(id))
        ? e
        : Object.fromEntries(Object.entries(e).filter(([id]) => present.has(id))),
    );
  }, [repos]);

  const labels = entry.target === "sweep" ? (entry.labels ?? []) : [];
  const needsCheck = labels.length > 0;
  const locked = (id: string) => enabledRepoIds.has(id) || justEnabled.includes(id);
  const newly = pick.selected.filter((id) => !locked(id));
  const checking = needsCheck && newly.some((id) => pick.checks[id] !== "done");
  const canEnable = newly.length > 0 && !checking && !busy && !submitting;

  const toggle = (repoId: string, on: boolean) => {
    setPick((p) => {
      if (on) {
        if (p.selected.includes(repoId)) return p;
        return {
          selected: [...p.selected, repoId],
          checks: needsCheck ? { ...p.checks, [repoId]: "checking" } : p.checks,
        };
      }
      const { [repoId]: _dropped, ...checks } = p.checks;
      return { selected: p.selected.filter((id) => id !== repoId), checks };
    });
    if (!on) setErrors(({ [repoId]: _cleared, ...rest }) => rest);
  };

  const reportCheck = (repoId: string, state: CheckState) =>
    setPick((p) =>
      // A report for a repo no longer selected (unchecked meanwhile) is dropped.
      p.selected.includes(repoId) && p.checks[repoId] !== state
        ? { ...p, checks: { ...p.checks, [repoId]: state } }
        : p,
    );

  const submit = async () => {
    if (!canEnable || inFlight.current) return;
    inFlight.current = true;
    const ids = newly;
    setSubmitting(true);
    let res: EnableResult | null;
    try {
      res = await onEnable(ids);
    } finally {
      inFlight.current = false;
    }
    if (!alive.current) return;
    setSubmitting(false);
    if (!res) return;
    if (res.failed.length === 0) {
      onDone();
      return;
    }
    const ok = new Set(res.enabled);
    setJustEnabled((cur) => [...cur, ...res.enabled]);
    setPick((p) => ({ ...p, selected: p.selected.filter((id) => !ok.has(id)) }));
    setErrors(Object.fromEntries(res.failed.map((f) => [f.repoId, f.message])));
    setFocusFailed(res.failed.map((f) => f.repoId));
  };

  const pathOf = (r: Repo) => maskRepoPath(r.path_with_namespace, demo);
  const q = query.trim().toLowerCase();
  const listed = q === "" ? repos : repos.filter((r) => pathOf(r).toLowerCase().includes(q));
  const warnRepos = needsCheck ? repos.filter((r) => newly.includes(r.id)) : [];

  useEffect(() => {
    if (!focusFailed) return;
    setFocusFailed(null);
    // The first failed repo the filter still shows; with every failed repo filtered out,
    // focus stays on the panel itself. Matched on the dataset rather than a selector, so no
    // repo id needs CSS escaping.
    const first = listed.find((r) => focusFailed.includes(r.id));
    const box = [...(panelRef.current?.querySelectorAll<HTMLInputElement>("input[data-repo-id]") ?? [])].find(
      (el) => el.dataset.repoId === first?.id,
    );
    (box ?? panelRef.current)?.focus();
  }, [focusFailed]); // eslint-disable-line react-hooks/exhaustive-deps

  const primaryLabel = submitting
    ? "Enabling…"
    : checking
      ? "Checking labels…"
      : `Enable ${newly.length}`;

  return (
    <div
      ref={panelRef}
      role="dialog"
      aria-labelledby={headingId}
      tabIndex={-1}
      style={shiftPx ? { transform: `translateX(${shiftPx}px)` } : undefined}
      className="absolute right-0 top-full z-20 mt-2 flex w-80 max-w-[calc(100vw-1rem)] flex-col rounded-xl border border-edge bg-raised text-left shadow-2xl outline-hidden"
    >
      <div className="px-3 pt-3">
        <p id={headingId} className="text-sm font-semibold text-fg">
          Enable {entry.name} on
        </p>
        {repos.length > FILTER_THRESHOLD && (
          <input
            type="text"
            aria-label="Filter repos"
            placeholder="Filter repos"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            className="mt-2 w-full rounded-md border border-edge bg-surface px-2 py-1 text-xs text-fg placeholder:text-faint outline-hidden focus:border-brand/70"
          />
        )}
      </div>

      {/* The list scrolls under the fixed action footer. */}
      <div className="mt-2 max-h-64 overflow-y-auto px-3 pb-2">
        {repos.length === 0 ? (
          <p className="py-2 text-xs text-muted">Connect a repo first, then enable this job on it.</p>
        ) : listed.length === 0 ? (
          <p className="py-2 text-xs text-muted">No repo matches “{query.trim()}”.</p>
        ) : (
          <ul className="space-y-0.5">
            {listed.map((r) => {
              const isLocked = locked(r.id);
              const err = errors[r.id];
              return (
                <li key={r.id}>
                  <label
                    className={cx(
                      "flex items-center gap-2 rounded-md px-1.5 py-1 text-[12.5px]",
                      isLocked ? "text-muted" : "cursor-pointer text-fg hover:bg-surface",
                    )}
                  >
                    <input
                      type="checkbox"
                      checked={isLocked || pick.selected.includes(r.id)}
                      disabled={isLocked || submitting}
                      onChange={(e) => toggle(r.id, e.target.checked)}
                      data-repo-id={r.id}
                      aria-describedby={err ? `${headingId}-err-${r.id}` : undefined}
                      className="accent-brand"
                    />
                    <span className="min-w-0 flex-1 break-all font-mono">{pathOf(r)}</span>
                    {isLocked && <Badge tone="ok">enabled</Badge>}
                  </label>
                  {err && (
                    <p id={`${headingId}-err-${r.id}`} className="ml-7 text-[11.5px] text-danger">
                      {err}
                    </p>
                  )}
                </li>
              );
            })}
          </ul>
        )}
        {warnRepos.length > 0 && (
          <div className="mt-2 space-y-2">
            {warnRepos.map((r) => (
              <SweepLabelWarn
                key={r.id}
                repoId={r.id}
                repoPath={pathOf(r)}
                labels={labels}
                onCheckStateChange={(state) => reportCheck(r.id, state)}
              />
            ))}
          </div>
        )}
      </div>

      <div className="flex items-center justify-between gap-2 border-t border-edge px-3 py-2.5">
        <span className="text-[11px] text-faint">
          {newly.length === 0 ? "Pick the repos to enable on" : `${newly.length} new schedule${newly.length === 1 ? "" : "s"}`}
        </span>
        <Button type="button" size="sm" disabled={!canEnable} onClick={submit}>
          {primaryLabel}
        </Button>
      </div>
    </div>
  );
}
