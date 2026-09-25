// Schedules — the /schedules page (PRD #241 M5; restructured by PRD #1645).
//
// Two tabs (D1). **Schedules** is the one operational list: every schedule the owner has,
// catalog-derived and user-authored, one flat row per schedule (ScheduleListRow) with its
// own cadence, next/last fire, options, inline actions and toggle (D2-D4), sorted per D3.
// **Job catalog** is where shipped jobs are discovered and enabled. `?tab=` deep-links a
// tab; without it the page lands on Schedules when the owner has any, else the catalog.
// The "New schedule" button and each row's Edit open the shared modal.

import { useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import {
  api,
  ApiError,
  type CatalogEntry,
  type Repo,
  type Schedule,
  type ScheduleCatalog,
  type SchedulePauseDTO,
} from "../lib/api";
import { PauseAllButton, PausePanel, PausedBanner } from "../components/SchedulePauseControl";
import { errorMessage } from "../lib/apiError";
import { useAsyncData } from "../lib/useAsyncData";
import { ScheduleModal } from "../components/ScheduleModal";
import { browserTimezone } from "../lib/timezone";
import { DefaultJobs, catalogEntryId } from "../components/DefaultJobs";
import { ScheduleListRow, scheduleNameId } from "../components/ScheduleListRow";
import { formatStamp } from "../components/LastRun";
import { scheduleDisplayName, sortSchedules } from "../lib/scheduleList";
import {
  Alert,
  Badge,
  Button,
  ListSkeleton,
  PageHeader,
  cx,
} from "../components/ui";
import { ClockIcon, PlusIcon } from "../components/icons";

type Tab = "schedules" | "catalog";

// The tab order, for the APG roving-tabindex arrow-key handler.
const TAB_ORDER: Tab[] = ["schedules", "catalog"];
const tabId = (t: Tab) => `sched-tab-${t}`;
const panelId = (t: Tab) => `sched-panel-${t}`;
const isTab = (v: string | null): v is Tab => v === "schedules" || v === "catalog";

// TAB_CLASS mirrors AdminShell's tab strip (issue #204 overflow contract) so the two
// in-page tabs read identically to the app's other tabbed surfaces.
const TAB_BASE =
  "-mb-px shrink-0 whitespace-nowrap border-b-2 px-3 py-2 text-sm font-medium transition-colors";
const TAB_ACTIVE = "border-brand text-fg";
const TAB_INACTIVE = "border-transparent text-muted hover:border-edge-strong hover:text-fg";

export function Schedules() {
  // The active tab lives in the URL (?tab=schedules|catalog, D1), so a tab is shareable
  // and back-navigable; a change replaces the entry rather than pushing one. Without a
  // valid ?tab the page lands on Schedules when the owner has ≥1 schedule, else the Job
  // catalog, decided ONCE on the first successful load (`landing`) so removing the last
  // schedule later does not yank the page to the other tab.
  const [searchParams, setSearchParams] = useSearchParams();
  const tabParam = searchParams.get("tab");
  const [landing, setLanding] = useState<Tab | null>(null);
  const tab: Tab = isTab(tabParam) ? tabParam : (landing ?? "schedules");
  const setTab = (t: Tab) =>
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.set("tab", t);
        return next;
      },
      { replace: true },
    );
  const [schedules, setSchedules] = useState<Schedule[] | null>(null);
  const [catalog, setCatalog] = useState<ScheduleCatalog | null>(null);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<Schedule | null>(null);
  const [busyId, setBusyId] = useState<string>("");
  // The user-level "pause all schedules" switch (PRD #1093). `pause` is the loaded,
  // NORMALIZED state (an expired `until` reads paused:false); `pickerOpen` drives the
  // inline preset panel; `pauseBusy` gates the pause/resume writes. Nothing on this path
  // ever writes a per-row `enabled`, so resuming restores exactly the prior set (D4).
  const [pause, setPause] = useState<SchedulePauseDTO | null>(null);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [pauseBusy, setPauseBusy] = useState(false);
  // Failed expiry re-reads in the current streak (drives the 60s retry backoff in the
  // expiry effect below); reset to 0 whenever a new pause state is applied.
  const [pauseRefreshAttempt, setPauseRefreshAttempt] = useState(0);
  // Revision of the pause state as seen by READS. Every mutation bumps it synchronously
  // before its request starts; a read (the initial load, a reload, the expiry re-read)
  // captures it at start and applies its result only if it is unchanged. The effect's
  // cancellation flag alone is not enough: it flips at commit, so a stale read and a
  // mutation resolving in ONE React batch would still let the read land last.
  const pauseRev = useRef(0);
  // D12: a row to bring into view once it is rendered on the Schedules tab (after a clone
  // or add-repo), and whether to move focus to its name cell. Consumed by the effect below.
  const [pendingReveal, setPendingReveal] = useState<{ id: string; focus: boolean } | null>(null);
  // The cloned row whose name cell takes focus when the edit modal it opened closes (D12).
  const [focusAfterEdit, setFocusAfterEdit] = useState<string | null>(null);
  // A catalog entry to focus once the Job catalog tab has rendered ("from catalog").
  const [pendingCatalogFocus, setPendingCatalogFocus] = useState<string | null>(null);
  // Session-scoped clone provenance: the DTO carries no "cloned from" field (a clone
  // bakes its source in with catalog_slug=null), so this shows the source name for a
  // clone made THIS session. Persistent provenance would need a backend field (seam).
  const [clonedFrom, setClonedFrom] = useState<Record<string, string>>({});
  // Refs to the tab buttons so the arrow-key handler can move focus with selection
  // (APG tabs, automatic activation): Left/Right wrap, Home/End jump to the ends.
  const tabRefs = useRef<Partial<Record<Tab, HTMLButtonElement | null>>>({});
  const onTabKeyDown = (e: React.KeyboardEvent) => {
    const idx = TAB_ORDER.indexOf(tab);
    let next = idx;
    if (e.key === "ArrowRight") next = (idx + 1) % TAB_ORDER.length;
    else if (e.key === "ArrowLeft") next = (idx - 1 + TAB_ORDER.length) % TAB_ORDER.length;
    else if (e.key === "Home") next = 0;
    else if (e.key === "End") next = TAB_ORDER.length - 1;
    else return;
    e.preventDefault();
    const target = TAB_ORDER[next];
    setTab(target);
    tabRefs.current[target]?.focus();
  };

  // schedules/catalog/repos are seeded here as fetcher side effects because all three
  // are read AND written outside the load — the optimistic enable toggle rewrites
  // schedules — so they stay local state rather than being derived from the hook's
  // data. The hook owns the load error (surfaced as loadError, unioned with the
  // mutation error below) and keeps the last-good rows on a reload failure, matching
  // the old catch's `cur ?? []` (which kept the existing rows). No loading flag: the
  // null schedules/catalog still gate the skeleton exactly as before, and on a
  // first-load failure the fetcher throws before seeding so both stay null, which
  // renders the skeleton the same as the old code did with catalog still null.
  const { error: loadError, reload } = useAsyncData(
    async ({ isCurrent }) => {
      const rev = pauseRev.current;
      const [rows, cat, repoList, pauseState] = await Promise.all([
        api.listSchedules(),
        api.listScheduleCatalog(),
        api.listRepos().then((r) => r.repos),
        api.getSchedulePause(),
      ]);
      if (!isCurrent()) return;
      setSchedules(rows);
      setLanding((cur) => cur ?? (rows.length > 0 ? "schedules" : "catalog"));
      setCatalog(cat);
      setRepos(repoList);
      // A pause mutation that started after this read began owns the newer state.
      if (rev === pauseRev.current) setPause(pauseState);
    },
    [],
    { fallback: "Could not load schedules", onFetchStart: () => setError("") },
  );

  const toggleEnabled = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    // Optimistic flip so the switch responds immediately.
    setSchedules((rows) =>
      rows ? rows.map((r) => (r.id === s.id ? { ...r, enabled: !r.enabled } : r)) : rows,
    );
    try {
      const updated = await api.updateSchedule(s.id, { enabled: !s.enabled });
      setSchedules((rows) => (rows ? rows.map((r) => (r.id === s.id ? updated : r)) : rows));
      // Keep the catalog view's enablement pause-flags in sync.
      reload();
    } catch (err) {
      setError(errorMessage(err, "Could not update the schedule"));
      // Revert on failure.
      setSchedules((rows) =>
        rows ? rows.map((r) => (r.id === s.id ? { ...r, enabled: s.enabled } : r)) : rows,
      );
    } finally {
      setBusyId("");
    }
  };

  const runNow = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    setNotice("");
    try {
      const { created } = await api.runScheduleNow(s.id);
      setNotice(
        created > 0
          ? `Started ${created} run${created === 1 ? "" : "s"} from this schedule.`
          : "Nothing fired — a matching run is already active (skipped by dedup).",
      );
      reload();
    } catch (err) {
      setError(errorMessage(err, "Could not run the schedule now"));
    } finally {
      setBusyId("");
    }
  };

  // Enable a catalog default on a set of repos — one call per repo (client fan-out,
  // matching the CLI), reporting a partial failure rather than swallowing it.
  const enableDefault = async (entry: CatalogEntry, repoIds: string[]) => {
    if (repoIds.length === 0) return;
    // In-flight guard (matches every other mutating action here): a double-click would
    // otherwise fan out duplicate enableCatalogSchedule calls. busyId keys off the entry
    // slug (distinct from any schedule id), which disables this entry's Enable buttons.
    if (busyId) return;
    setBusyId(entry.slug);
    setError("");
    setNotice("");
    // Seed each new schedule's zone from the browser's detected IANA timezone (issue #660),
    // parity with the create modal. Resolved once so every repo in the fan-out gets the same
    // zone. browserTimezone() always returns a valid IANA name (falling back to "UTC"), so
    // the web always sends a body; the catalog zone is kept only on the no-body path used by
    // CLI/headless enable.
    const tz = browserTimezone();
    try {
      const results = await Promise.allSettled(
        repoIds.map((rid) => api.enableCatalogSchedule(rid, entry.slug, tz)),
      );
      const ok = results.filter((r) => r.status === "fulfilled").length;
      const failed = results.length - ok;
      if (failed > 0) {
        setError(`Enabled “${entry.name}” on ${ok} of ${repoIds.length} repos; ${failed} failed.`);
      } else {
        setNotice(
          `Enabled “${entry.name}” on ${ok} repo${ok === 1 ? "" : "s"} → ${ok} schedule${ok === 1 ? "" : "s"}.`,
        );
      }
      await reload();
    } finally {
      setBusyId("");
    }
  };

  const resetDefault = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    setNotice("");
    try {
      await api.resetSchedule(s.id);
      setNotice("Reset to the catalog default.");
      await reload();
    } catch (err) {
      setError(errorMessage(err, "Could not reset the schedule"));
    } finally {
      setBusyId("");
    }
  };

  // Clone a schedule (default or user) into an editable user copy, then open the edit
  // modal on it so the owner can immediately edit the now-unlocked prompt.
  const cloneSchedule = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    setNotice("");
    try {
      const clone = await api.cloneSchedule(s.id);
      const label = s.origin === "default" && s.catalog_slug ? catalogName(catalog, s.catalog_slug) : "a schedule";
      setClonedFrom((m) => ({ ...m, [clone.id]: label }));
      await reload();
      setEditing(clone);
      // D12: show the clone on the Schedules tab behind the modal; its name cell takes
      // focus when the modal closes (the modal holds focus while open).
      revealSchedule(clone.id, false);
      setFocusAfterEdit(clone.id);
    } catch (err) {
      setError(errorMessage(err, "Could not clone the schedule"));
    } finally {
      setBusyId("");
    }
  };

  const removeSchedule = async (s: Schedule) => {
    setBusyId(s.id);
    setError("");
    try {
      await api.deleteSchedule(s.id);
      await reload();
    } catch (err) {
      setError(errorMessage(err, "Could not remove the schedule"));
    } finally {
      setBusyId("");
    }
  };

  // revealSchedule brings a just-created row into view (D12): switch to the Schedules tab,
  // then (once the row is rendered) scroll it into view and optionally focus its name
  // cell. M2 extends this to clear any filter that would hide the row and to expand the
  // fired-one-shot fold when the row lands in it.
  const revealSchedule = (id: string, focus: boolean) => {
    setTab("schedules");
    setPendingReveal({ id, focus });
  };

  // "from catalog" (and, until M3 wires its picker, "Enable on another repo"): switch to
  // the Job catalog tab and move focus to that entry.
  const showInCatalog = (slug: string) => {
    setTab("catalog");
    setPendingCatalogFocus(slug);
  };

  // Closing the edit modal: a clone's modal hands focus to the cloned row (D12).
  const closeEditing = () => {
    setEditing(null);
    if (focusAfterEdit) {
      setPendingReveal({ id: focusAfterEdit, focus: true });
      setFocusAfterEdit(null);
    }
  };

  // Add another repo to a custom schedule (PRD #636 M3): replicate the source's config
  // onto a new repo via the add-repo endpoint. On success, refresh and reveal the new
  // row, focusing its name cell (D12). A 409 (the repo already carries a sibling of this
  // schedule) is friendly and non-fatal, not an error.
  const addRepo = async (source: Schedule, repoId: string) => {
    if (busyId) return;
    setBusyId(source.id);
    setError("");
    setNotice("");
    try {
      const sibling = await api.addScheduleRepo(source.id, repoId);
      setNotice("Added on another repo.");
      await reload();
      revealSchedule(sibling.id, true);
    } catch (err) {
      if (err instanceof ApiError && err.status === 409) {
        setNotice("That schedule is already on that repo.");
      } else {
        setError(errorMessage(err, "Could not add the repo"));
      }
    } finally {
      setBusyId("");
    }
  };

  // After a FAILED mutation the page holds its pre-mutation state, but the mutation's
  // revision bump discarded any expiry re-read that was in flight, and nothing else
  // would schedule another one. Re-read the server state now (revision-guarded); on a
  // failed re-read, bump the attempt counter so the expiry effect re-arms its retry.
  const revalidatePause = () => {
    const rev = pauseRev.current;
    api
      .getSchedulePause()
      .then((next) => {
        if (rev !== pauseRev.current) return;
        setPauseRefreshAttempt(0);
        setPause(next);
      })
      .catch(() => {
        if (rev === pauseRev.current) setPauseRefreshAttempt((n) => n + 1);
      });
  };

  // Pause every schedule until `until` (RFC3339, or null for indefinite). PUT then adopt
  // the returned normalized state and close the picker.
  const submitPause = async (until: string | null) => {
    setPauseBusy(true);
    setError("");
    try {
      pauseRev.current += 1; // invalidate every read started before this mutation
      const next = await api.putSchedulePause(until);
      setPauseRefreshAttempt(0); // a fresh state gets a fresh (immediate) expiry refresh
      setPause(next);
      setPickerOpen(false);
    } catch (err) {
      setError(errorMessage(err, "Could not pause your schedules"));
      revalidatePause();
    } finally {
      setPauseBusy(false);
    }
  };

  // Resume (idempotent DELETE). Per-row `enabled` is never touched by the switch, so the
  // rows read exactly as they did before the pause.
  const resumePause = async () => {
    setPauseBusy(true);
    setError("");
    try {
      pauseRev.current += 1; // invalidate every read started before this mutation
      const next = await api.deleteSchedulePause();
      setPauseRefreshAttempt(0);
      setPause(next);
      setPickerOpen(false);
    } catch (err) {
      setError(errorMessage(err, "Could not resume your schedules"));
      revalidatePause();
    } finally {
      setPauseBusy(false);
    }
  };

  const onSaved = () => {
    setCreating(false);
    closeEditing();
    reload();
  };

  // D12 reveal: runs once the target row exists in the DOM (the list may still be
  // reloading, and the tab switch lands a render later), then clears itself.
  useEffect(() => {
    if (!pendingReveal || tab !== "schedules") return;
    const el = document.getElementById(scheduleNameId(pendingReveal.id));
    if (!el) return;
    el.scrollIntoView?.({ block: "nearest" });
    if (pendingReveal.focus) el.focus();
    setPendingReveal(null);
  }, [pendingReveal, tab, schedules]);

  useEffect(() => {
    if (!pendingCatalogFocus || tab !== "catalog") return;
    const el = document.getElementById(catalogEntryId(pendingCatalogFocus));
    if (!el) return;
    el.scrollIntoView?.({ block: "nearest" });
    el.focus();
    setPendingCatalogFocus(null);
  }, [pendingCatalogFocus, tab, catalog]);

  // Every schedule, both origins (D2), named (D4) and ordered (D3).
  const entriesBySlug = useMemo(
    () => new Map((catalog?.entries ?? []).map((e) => [e.slug, e])),
    [catalog],
  );
  const nameOf = (s: Schedule) => scheduleDisplayName(s, entriesBySlug);
  const all = schedules ?? [];
  const rows = sortSchedules(all, nameOf);
  const total = all.length;
  const enabledCount = all.filter((s) => s.enabled).length;
  const activeCount = all.filter((s) => s.enabled && s.status === "active").length;
  // Catalog entries enabled on none of the owner's repos (the "K not enabled" pill, D1).
  const enabledSlugs = new Set(all.filter((s) => s.origin === "default" && s.catalog_slug).map((s) => s.catalog_slug));
  const notEnabled = (catalog?.entries ?? []).filter((e) => !enabledSlugs.has(e.slug)).length;
  const loaded = schedules !== null && catalog !== null;

  // The pre-formatted "paused until <stamp>" line every Next-run cell shows while the
  // switch is on (warn-coloured at the render site); null when not paused. Computed once
  // here so the row components stay dump — they only render the string when present.
  const pauseActive = pause?.paused ?? false;
  // The server normalizes an expired `until` on every read, but an open page does not
  // re-read on its own: arm one timer for the auto-resume instant and re-fetch then, so
  // the banner and the row notes clear at the moment the scheduler stops honouring the
  // pause. Re-armed whenever the state changes; cleared on unmount. setTimeout's delay
  // is a 32-bit signed int, so a far-off until is clamped and re-armed on fire. A failed
  // re-read keeps the LAST KNOWN state (the server may still be honouring the pause) and
  // retries a minute later via pauseRefreshAttempt; it never guesses "not paused". The
  // re-read applies only while it is still current: any mutation (Resume now, Change…)
  // sets `pause`, which re-runs this effect and cancels the in-flight read, so a stale
  // GET can never overwrite the mutation's own response.
  useEffect(() => {
    if (!pause?.paused || !pause.until) return;
    let cancelled = false;
    const remaining = new Date(pause.until).getTime() - Date.now();
    const delay = remaining > 0 ? Math.min(remaining, 2_147_483_647) : pauseRefreshAttempt > 0 ? 60_000 : 0;
    const id = setTimeout(() => {
      const rev = pauseRev.current;
      api
        .getSchedulePause()
        .then((next) => {
          if (cancelled || rev !== pauseRev.current) return;
          setPauseRefreshAttempt(0); // the backoff is per failure streak, not per page
          setPause(next);
        })
        .catch(() => {
          if (!cancelled && rev === pauseRev.current) setPauseRefreshAttempt((n) => n + 1);
        });
    }, delay);
    return () => {
      cancelled = true;
      clearTimeout(id);
    };
  }, [pause, pauseRefreshAttempt]);
  const pauseNote = pauseActive
    ? pause?.until
      ? `paused until ${formatStamp(pause.until)}`
      : "paused until you resume"
    : null;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Schedules"
        description="Everything that runs on a clock, in one place. Enable a shipped job from the Job catalog, or author your own against a pinned issue, a label sweep, or an ad-hoc prompt."
        actions={
          <Button onClick={() => setCreating(true)}>
            <PlusIcon /> New schedule
          </Button>
        }
      />

      {/* Pause-all slot (PRD #1093 D9): the inline preset picker or the paused banner
          lives here, between the header and the tab row, so it covers nothing on the
          page. One control per state: while paused the tab-row button yields to the banner. */}
      {pickerOpen ? (
        <PausePanel onSubmit={submitPause} onCancel={() => setPickerOpen(false)} busy={pauseBusy} />
      ) : pauseActive && pause ? (
        <PausedBanner
          pause={pause}
          onChange={() => setPickerOpen(true)}
          onResume={resumePause}
          busy={pauseBusy}
        />
      ) : null}

      {/* Two tabs (D1): the operational list, then the catalog of shipped jobs. */}
      <div className="flex items-center gap-1 overflow-x-auto border-b border-edge" role="tablist" aria-label="Schedules">
        <button
          type="button"
          role="tab"
          id={tabId("schedules")}
          aria-selected={tab === "schedules"}
          aria-controls={panelId("schedules")}
          tabIndex={tab === "schedules" ? 0 : -1}
          ref={(el) => {
            tabRefs.current.schedules = el;
          }}
          onKeyDown={onTabKeyDown}
          onClick={() => setTab("schedules")}
          className={cx(TAB_BASE, tab === "schedules" ? TAB_ACTIVE : TAB_INACTIVE)}
        >
          Schedules{loaded ? ` · ${total}` : ""}
        </button>
        <button
          type="button"
          role="tab"
          id={tabId("catalog")}
          aria-selected={tab === "catalog"}
          aria-controls={panelId("catalog")}
          tabIndex={tab === "catalog" ? 0 : -1}
          ref={(el) => {
            tabRefs.current.catalog = el;
          }}
          onKeyDown={onTabKeyDown}
          onClick={() => setTab("catalog")}
          className={cx(TAB_BASE, "inline-flex items-center gap-2", tab === "catalog" ? TAB_ACTIVE : TAB_INACTIVE)}
        >
          Job catalog{loaded ? ` · ${catalog.entries.length}` : ""}
          {loaded && notEnabled > 0 && <Badge tone="neutral">{notEnabled} not enabled</Badge>}
        </button>
        {/* Running state only: the button sits at the right end of the tab row (ml-auto),
            directly under "New schedule", so no header height is added. It disappears
            while paused (the banner above carries Resume) or while the picker is open,
            and it does not render until the pause state has LOADED (pause !== null):
            an unresolved read must not offer a control that could overwrite a pause the
            server already holds. */}
        {pause !== null && !pauseActive && !pickerOpen && (
          <PauseAllButton onClick={() => setPickerOpen(true)} disabled={pauseBusy} />
        )}
      </div>

      {/* One tabpanel per tab, each labelled by its tab; the inactive one is unmounted,
          so its id/aria-controls link is live only for the shown panel (APG tabs). */}
      {!loaded ? (
        <>
          {(error || loadError) && <Alert message={error || loadError} />}
          <ListSkeleton rows={4} />
        </>
      ) : tab === "catalog" ? (
        <div role="tabpanel" id={panelId("catalog")} aria-labelledby={tabId("catalog")}>
          <DefaultJobs
            catalog={catalog}
            schedules={schedules}
            repos={repos}
            busyId={busyId}
            onEnable={enableDefault}
            onTogglePause={toggleEnabled}
            onRunNow={runNow}
            onReset={resetDefault}
            onClone={cloneSchedule}
            onRemove={removeSchedule}
            onEdit={setEditing}
            pauseNote={pauseNote}
            notice={notice}
            error={error || loadError}
          />
        </div>
      ) : (
        <div
          className="space-y-6"
          role="tabpanel"
          id={panelId("schedules")}
          aria-labelledby={tabId("schedules")}
        >
          {(error || loadError) && <Alert message={error || loadError} />}
          {notice && <Alert message={notice} tone="info" />}

          {rows.length === 0 ? (
            <div className="rounded-xl border border-dashed border-edge p-10 text-center">
              <div className="mx-auto mb-3 flex h-10 w-10 items-center justify-center rounded-lg bg-raised text-lg text-muted">
                <ClockIcon />
              </div>
              {/* D8, verbatim. */}
              <p className="mx-auto max-w-sm text-sm text-muted">
                Nothing runs on a clock yet. Enable a shipped job from the Job catalog, or create your own.
              </p>
              <div className="mt-4 flex flex-wrap justify-center gap-2">
                <Button variant="secondary" onClick={() => setTab("catalog")}>
                  Open the Job catalog
                </Button>
                <Button onClick={() => setCreating(true)}>
                  <PlusIcon /> New schedule
                </Button>
              </div>
            </div>
          ) : (
            <>
              <p className="text-sm text-muted">
                {`${total} schedule${total === 1 ? "" : "s"} · ${activeCount} active · ${enabledCount} enabled`}
              </p>
              {/* Below md the table restyles into stacked cards (D11): no horizontal scroll. */}
              <div className="rounded-xl border border-edge bg-surface md:overflow-x-auto">
                <table className="block w-full text-left text-sm md:table">
                  <thead className="hidden md:table-header-group">
                    <tr className="border-b border-edge text-[12.5px] text-muted">
                      <th className="px-4 py-3 font-medium">Target · repo</th>
                      <th className="px-4 py-3 font-medium">When</th>
                      <th className="px-4 py-3 font-medium">Next run</th>
                      <th className="px-4 py-3 font-medium">Last run</th>
                      <th className="px-4 py-3 font-medium">Options</th>
                      <th className="px-4 py-3 text-right font-medium">Actions · On</th>
                    </tr>
                  </thead>
                  <tbody className="block md:table-row-group">
                    {rows.map((s) => (
                      <ScheduleListRow
                        key={s.id}
                        s={s}
                        name={nameOf(s)}
                        busy={busyId === s.id}
                        addBusy={busyId !== ""}
                        clonedFrom={clonedFrom[s.id]}
                        pauseNote={pauseNote}
                        repos={repos}
                        onToggle={() => toggleEnabled(s)}
                        onRunNow={() => runNow(s)}
                        onEdit={() => setEditing(s)}
                        onReset={() => resetDefault(s)}
                        onClone={() => cloneSchedule(s)}
                        onRemove={() => removeSchedule(s)}
                        onAddRepo={(repoId) => addRepo(s, repoId)}
                        onShowInCatalog={() => s.catalog_slug && showInCatalog(s.catalog_slug)}
                      />
                    ))}
                  </tbody>
                </table>
              </div>
            </>
          )}

          <Legend />
        </div>
      )}

      {creating && <ScheduleModal onClose={() => setCreating(false)} onSaved={onSaved} />}
      {editing && (
        <ScheduleModal
          editing={editing}
          onClose={closeEditing}
          onSaved={onSaved}
          onCloneToEdit={(s) => {
            setEditing(null);
            setFocusAfterEdit(null);
            cloneSchedule(s);
          }}
        />
      )}
    </div>
  );
}

// catalogName resolves a slug to its display name for the "cloned from" label.
function catalogName(catalog: ScheduleCatalog | null, slug: string): string {
  return catalog?.entries.find((e) => e.slug === slug)?.name ?? "a default";
}

function Legend() {
  return (
    <div className="flex flex-wrap gap-x-5 gap-y-2 text-[12px] text-muted">
      <span className="flex items-center gap-1.5">
        <Badge tone="brand" dot>
          once
        </Badge>
        fires a single time, then goes terminal
      </span>
      <span className="flex items-center gap-1.5">
        <Badge tone="info" dot>
          sweep
        </Badge>
        fires on issues matching a label (default: uzi)
      </span>
      <span className="flex items-center gap-1.5">
        <Badge tone="ok" dot>
          prompt
        </Badge>
        issue-less run, opens an MR
      </span>
      <span className="flex items-center gap-1.5">
        <span className="text-brand">from catalog</span>
        a shipped job; its prompt and labels are sealed
      </span>
    </div>
  );
}
