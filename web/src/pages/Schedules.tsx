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
import { JobCatalog, catalogEntryId } from "../components/JobCatalog";
import type { EnableResult } from "../components/EnableJobDialog";
import { ScheduleListRow, scheduleNameId } from "../components/ScheduleListRow";
import { formatStamp } from "../components/LastRun";
import { FILTER_ALL_ID, ScheduleFilters } from "../components/ScheduleFilters";
import {
  NO_FILTER,
  clearHidingFilters,
  foldRefs,
  isFoldedOnce,
  pruneFilter,
  repoOptions,
  scheduleDisplayName,
  sourceCounts,
  splitFold,
  type ScheduleFilter,
} from "../lib/scheduleList";
import {
  Alert,
  Badge,
  Button,
  ListSkeleton,
  PageHeader,
  cx,
} from "../components/ui";
import { ChevronRightIcon, ClockIcon, PlusIcon } from "../components/icons";

type Tab = "schedules" | "catalog";

// A pending D12 reveal (see `pendingReveal` in the page).
type Reveal = { id: string; focus: boolean; afterSeq: number };

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
  // The Job catalog card whose enable dialog is open (D7), at most one; page-owned so a
  // default row's "Enable on another repo" can open it from the Schedules tab (D4).
  // Leaving the catalog tab by any path (a click, the tablist's arrow keys, a link)
  // closes it, so coming back never re-opens a dialog whose open effect would take focus
  // from the tab the user just moved to.
  const [enableDialogSlug, setEnableDialogSlug] = useState<string | null>(null);
  const setTab = (t: Tab) => {
    if (t !== "catalog") setEnableDialogSlug(null);
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.set("tab", t);
        return next;
      },
      { replace: true },
    );
  };
  const [schedules, setSchedules] = useState<Schedule[] | null>(null);
  const [catalog, setCatalog] = useState<ScheduleCatalog | null>(null);
  const [repos, setRepos] = useState<Repo[]>([]);
  const [error, setError] = useState("");
  // The page notice (role=status). `jobSlug` is set only by a fully successful catalog
  // enable, whose notice then offers the Schedules tab filtered to that job (D7).
  const [noticeState, setNoticeState] = useState<{ text: string; jobSlug: string | null }>({ text: "", jobSlug: null });
  const notice = noticeState.text;
  const setNotice = (text: string, jobSlug: string | null = null) => setNoticeState({ text, jobSlug });
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
  // or add-repo), and whether to move focus to its name cell. `afterSeq` is the load
  // sequence of the reload that followed the mutation: the request is dropped as "the row
  // is gone" only by a list loaded at or after it, never by an older one still on screen
  // (an overlapping reload can supersede the awaited one, so `await reload()` returning
  // does not mean its list was applied). Consumed by the effect below.
  const [pendingReveal, setPendingReveal] = useState<Reveal | null>(null);
  // The cloned row whose name cell takes focus when the edit modal it opened closes (D12).
  const [focusAfterEdit, setFocusAfterEdit] = useState<{ id: string; afterSeq: number } | null>(null);
  // D5 filters: page-level state, so they survive switching tabs and back; not persisted
  // and not in the URL. `foldOpen` is the D6 fold's disclosure: null follows the derived
  // default (open only when the fold holds every match), a boolean is the user's own
  // choice, honoured until the filters change.
  const [filter, setFilter] = useState<ScheduleFilter>(NO_FILTER);
  const [foldOpen, setFoldOpen] = useState<boolean | null>(null);
  const applyFilter = (next: ScheduleFilter) => {
    setFilter(next);
    setFoldOpen(null);
  };
  // Load sequence: `loadSeq` is bumped as each list load starts; `loadedSeq` is the
  // sequence of the latest current load to SETTLE: its list is in `schedules` when it
  // succeeded, and when it failed the list on screen is older (see pendingReveal).
  const loadSeq = useRef(0);
  const [loadedSeq, setLoadedSeq] = useState(0);
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
      const seq = ++loadSeq.current;
      const rev = pauseRev.current;
      let loadedAll: [Schedule[], ScheduleCatalog, Repo[], SchedulePauseDTO];
      try {
        loadedAll = await Promise.all([
          api.listSchedules(),
          api.listScheduleCatalog(),
          api.listRepos().then((r) => r.repos),
          api.getSchedulePause(),
        ]);
      } catch (err) {
        // A failed load still settles its sequence, so a reveal awaiting it is dropped
        // rather than left armed to steal focus on some later render (pendingReveal).
        if (isCurrent()) setLoadedSeq(seq);
        throw err;
      }
      const [rows, cat, repoList, pauseState] = loadedAll;
      if (!isCurrent()) return;
      setSchedules(rows);
      setLoadedSeq(seq);
      setLanding((cur) => cur ?? (rows.length > 0 ? "schedules" : "catalog"));
      setCatalog(cat);
      setRepos(repoList);
      // A pause mutation that started after this read began owns the newer state.
      if (rev === pauseRev.current) setPause(pauseState);
    },
    [],
    { fallback: "Could not load schedules", onFetchStart: () => setError("") },
  );

  // reloadMarked reloads and returns that load's sequence. The fetcher bumps loadSeq
  // synchronously when reload() starts it, so reading it right after the call names
  // exactly this load.
  const reloadMarked = async () => {
    const done = reload();
    const seq = loadSeq.current;
    await done;
    return seq;
  };

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
  // matching the CLI), reporting a partial failure rather than swallowing it. Resolves to
  // the per-repo outcome so the enable dialog can lock the repos that succeeded and keep
  // the failed ones for a retry (D7); null when it did not start (nothing to enable, or
  // another mutation in flight). It never switches tabs (D12).
  const enableDefault = async (entry: CatalogEntry, repoIds: string[]): Promise<EnableResult | null> => {
    if (repoIds.length === 0) return null;
    // In-flight guard (matches every other mutating action here): a double-click would
    // otherwise fan out duplicate enableCatalogSchedule calls. busyId keys off the entry
    // slug (distinct from any schedule id), which disables every card's Enable.
    if (busyId) return null;
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
      const out: EnableResult = { enabled: [], failed: [] };
      results.forEach((r, i) => {
        if (r.status === "fulfilled") out.enabled.push(repoIds[i]);
        else out.failed.push({ repoId: repoIds[i], message: errorMessage(r.reason, "Could not enable on this repo") });
      });
      // The refreshed list is what flips the succeeded repos to "enabled" in the dialog.
      // Reload FIRST: every load clears the page error as it starts (onFetchStart), so an
      // error set before it would be wiped before it was ever seen.
      await reload();
      const ok = out.enabled.length;
      const failed = out.failed.length;
      if (failed > 0) {
        setError(`Enabled “${entry.name}” on ${ok} of ${repoIds.length} repos; ${failed} failed.`);
      } else {
        setNotice(
          `Enabled “${entry.name}” on ${ok} repo${ok === 1 ? "" : "s"} → ${ok} schedule${ok === 1 ? "" : "s"}.`,
          entry.slug,
        );
      }
      return out;
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
      const afterSeq = await reloadMarked();
      setEditing(clone);
      // D12: show the clone on the Schedules tab behind the modal; its name cell takes
      // focus when the modal closes (the modal holds focus while open).
      revealSchedule(clone.id, false, afterSeq);
      setFocusAfterEdit({ id: clone.id, afterSeq });
    } catch (err) {
      setError(errorMessage(err, "Could not clone the schedule"));
    } finally {
      setBusyId("");
    }
  };

  // Remove, then hand focus to where the row was instead of letting it fall to <body>
  // with the row's menu button: the next row's name cell, else the previous row's, else
  // (the list is now empty) the Schedules tab. A remove started from the Job catalog tab
  // keeps its own focus handling.
  const removeSchedule = async (s: Schedule) => {
    const fromList = tab === "schedules";
    const at = displayed.findIndex((r) => r.id === s.id);
    const neighbour = at < 0 ? undefined : (displayed[at + 1] ?? displayed[at - 1]);
    setBusyId(s.id);
    setError("");
    try {
      await api.deleteSchedule(s.id);
      await reload();
      if (fromList && neighbour) setPendingReveal({ id: neighbour.id, focus: true, afterSeq: 0 });
      else if (fromList) tabRefs.current.schedules?.focus();
    } catch (err) {
      setError(errorMessage(err, "Could not remove the schedule"));
    } finally {
      setBusyId("");
    }
  };

  // revealSchedule brings a just-created row into view (D12): switch to the Schedules tab,
  // then, once the row is in the loaded list, clear any filter that would hide it, expand
  // the fired-one-shot fold if it lands there, scroll it into view and optionally focus
  // its name cell (the effect below). `afterSeq` is the mutation's reload (reloadMarked).
  const revealSchedule = (id: string, focus: boolean, afterSeq: number) => {
    setTab("schedules");
    setPendingReveal({ id, focus, afterSeq });
  };

  // "from catalog": switch to the Job catalog tab and move focus to that entry's card.
  const showInCatalog = (slug: string) => {
    setTab("catalog");
    setPendingCatalogFocus(slug);
  };

  // "Enable on another repo" (D4): switch to the Job catalog tab and open that entry's
  // enable dialog, which moves focus into itself.
  const enableElsewhere = (slug: string) => {
    setTab("catalog");
    setEnableDialogSlug(slug);
  };

  // A card's "Enabled on N repos" link, and the enable notice's link (D7): the Schedules
  // tab filtered to that job, every other dimension cleared.
  const showJobInSchedules = (slug: string) => {
    setTab("schedules");
    applyFilter({ ...NO_FILTER, jobSlug: slug });
  };

  // Closing the edit modal: a clone's modal hands focus to the cloned row (D12).
  const closeEditing = () => {
    setEditing(null);
    if (focusAfterEdit) {
      setPendingReveal({ ...focusAfterEdit, focus: true });
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
      revealSchedule(sibling.id, true, await reloadMarked());
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

  useEffect(() => {
    if (!pendingCatalogFocus || tab !== "catalog") return;
    const el = document.getElementById(catalogEntryId(pendingCatalogFocus));
    if (!el) {
      // A slug the loaded catalog does not carry will never render: drop the request.
      if (catalog && !catalog.entries.some((e) => e.slug === pendingCatalogFocus)) setPendingCatalogFocus(null);
      return;
    }
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
  // Only a default whose slug the loaded catalog still carries has an entry to show; any
  // other row gets no "from catalog" link and no "Enable on another repo" item.
  const catalogSlugOf = (s: Schedule) => {
    const slug = s.origin === "default" ? s.catalog_slug : null;
    return slug && entriesBySlug.has(slug) ? slug : null;
  };
  const catalogLinkFor = (s: Schedule) => {
    const slug = catalogSlugOf(s);
    return slug ? () => showInCatalog(slug) : undefined;
  };
  const enableElsewhereFor = (s: Schedule) => {
    const slug = catalogSlugOf(s);
    return slug ? () => enableElsewhere(slug) : undefined;
  };
  const all = schedules ?? [];
  // D5 → D6 → D3: filter, fold the fired one-shots that survived, sort each part.
  const { shown, folded } = splitFold(all, filter, nameOf);
  // The fold opens by itself when it holds every match, so a filter never looks empty
  // while hiding its matches; a user's own open/close stands until the filters change.
  const foldExpanded = foldOpen ?? (shown.length === 0 && folded.length > 0);
  // The rows on screen, in order (Remove hands focus to a displayed neighbour).
  const displayed = foldExpanded ? [...shown, ...folded] : shown;
  const counts = sourceCounts(all);
  const repoChoices = repoOptions(all);
  const jobName = filter.jobSlug === null ? null : (entriesBySlug.get(filter.jobSlug)?.name ?? filter.jobSlug);
  const total = all.length;
  const enabledCount = all.filter((s) => s.enabled).length;
  const activeCount = all.filter((s) => s.enabled && s.status === "active").length;
  // Catalog entries enabled on none of the owner's repos (the "K not enabled" pill, D1).
  const enabledSlugs = new Set(all.filter((s) => s.origin === "default" && s.catalog_slug).map((s) => s.catalog_slug));
  const notEnabled = (catalog?.entries ?? []).filter((e) => !enabledSlugs.has(e.slug)).length;
  const loaded = schedules !== null && catalog !== null;

  // A Repo or Job filter whose target left the page (its last row removed, the entry gone
  // from the catalog) is pruned, so no filter stays active while nothing shows it.
  useEffect(() => {
    if (!schedules || !catalog) return;
    const pruned = pruneFilter(filter, schedules, new Set(catalog.entries.map((e) => e.slug)));
    if (pruned !== filter) applyFilter(pruned);
  }, [schedules, catalog, filter]);

  const renderRow = (s: Schedule) => (
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
      onShowInCatalog={catalogLinkFor(s)}
      onEnableElsewhere={enableElsewhereFor(s)}
    />
  );

  // D12 reveal: runs once the target row is in the loaded list (the list may still be
  // reloading, and the tab switch lands a render later). First it clears the filters in
  // the row's way and opens the fold it lands in, one render each; then it scrolls to and
  // optionally focuses the row, and clears itself.
  useEffect(() => {
    if (!pendingReveal || tab !== "schedules") return;
    const row = schedules?.find((r) => r.id === pendingReveal.id);
    if (row) {
      const cleared = clearHidingFilters(filter, row);
      if (cleared !== filter) {
        applyFilter(cleared);
        return;
      }
      if (isFoldedOnce(row) && !foldExpanded) {
        setFoldOpen(true);
        return;
      }
    }
    const el = document.getElementById(scheduleNameId(pendingReveal.id));
    if (!el) {
      // The row is gone from a list loaded after the mutation (removed meanwhile): drop
      // the request so it cannot fire on some later render, and keep focus off <body>.
      // A list that predates the mutation proves nothing, so keep waiting on it.
      // A FAILED load at or after it settles `loadedSeq` too: the row cannot be shown, so
      // the request is dropped instead of lingering to steal focus later.
      if (!row && schedules && loadedSeq >= pendingReveal.afterSeq) {
        setPendingReveal(null);
        if (pendingReveal.focus) tabRefs.current.schedules?.focus();
      }
      return;
    }
    el.scrollIntoView?.({ block: "nearest" });
    if (pendingReveal.focus) el.focus();
    setPendingReveal(null);
  }, [pendingReveal, tab, schedules, loadedSeq, filter, foldExpanded]);

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
        <div className="space-y-4" role="tabpanel" id={panelId("catalog")} aria-labelledby={tabId("catalog")}>
          {(error || loadError) && <Alert message={error || loadError} />}
          {notice && <PageNotice text={notice} jobSlug={noticeState.jobSlug} onShowJob={showJobInSchedules} />}
          <JobCatalog
            catalog={catalog}
            schedules={schedules}
            repos={repos}
            busy={busyId !== ""}
            openSlug={enableDialogSlug}
            onOpenSlug={setEnableDialogSlug}
            onEnable={enableDefault}
            onShowJob={showJobInSchedules}
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
          {notice && <PageNotice text={notice} jobSlug={noticeState.jobSlug} onShowJob={showJobInSchedules} />}

          {all.length === 0 ? (
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
              <ScheduleFilters
                source={filter.source}
                counts={counts}
                onSource={(source) => applyFilter({ ...filter, source })}
                repos={repoChoices}
                repoId={filter.repoId}
                onRepo={(repoId) => applyFilter({ ...filter, repoId })}
                jobName={jobName}
                onClearJob={() => applyFilter({ ...filter, jobSlug: null })}
              />
              {shown.length === 0 && folded.length === 0 ? (
                <div className="rounded-xl border border-dashed border-edge p-8 text-center">
                  <p className="text-sm text-muted">No schedules match</p>
                  <div className="mt-3">
                    <Button
                      variant="secondary"
                      size="sm"
                      onClick={() => {
                        applyFilter(NO_FILTER);
                        // The button unmounts with the empty state: hand focus to the
                        // now-pressed "All" chip rather than letting it fall to <body>.
                        document.getElementById(FILTER_ALL_ID)?.focus();
                      }}
                    >
                      Clear filters
                    </Button>
                  </div>
                </div>
              ) : (
                /* Below md the table restyles into stacked cards (D11): no horizontal scroll. */
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
                      {shown.map(renderRow)}
                    </tbody>
                    {folded.length > 0 && (
                      <>
                        <tbody className="block md:table-row-group">
                          <FoldRow
                            count={folded.length}
                            refs={foldRefs(folded)}
                            expanded={foldExpanded}
                            onToggle={() => setFoldOpen(!foldExpanded)}
                          />
                        </tbody>
                        {/* The disclosure's aria-controls target; rows render only while open. */}
                        <tbody id={FOLD_ID} className="block md:table-row-group">
                          {foldExpanded && folded.map(renderRow)}
                        </tbody>
                      </>
                    )}
                  </table>
                </div>
              )}
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

// The D6 fold's rows (the disclosure's aria-controls target).
const FOLD_ID = "schedules-fired-fold";

// FoldRow is the D6 disclosure: one row at the bottom of the table standing for the
// fired one-time schedules that match the filters, with their issue refs. A real button
// carries the expanded state; it works on the narrow-card layout (D11) as a full-width row.
function FoldRow({
  count,
  refs,
  expanded,
  onToggle,
}: {
  count: number;
  refs: string[];
  expanded: boolean;
  onToggle: () => void;
}) {
  const shownRefs = refs.slice(0, 8);
  return (
    <tr className="block border-t border-edge md:table-row">
      <td colSpan={6} className="block px-4 py-2.5 md:table-cell">
        <button
          type="button"
          aria-expanded={expanded}
          aria-controls={FOLD_ID}
          onClick={onToggle}
          className="flex w-full flex-wrap items-center gap-x-2 gap-y-1 rounded text-left text-[12.5px] text-muted hover:text-fg"
        >
          <ChevronRightIcon
            aria-hidden="true"
            className={cx("shrink-0 transition-transform motion-reduce:transition-none", expanded && "rotate-90")}
          />
          <span className="font-medium">
            {count} one-time schedule{count === 1 ? "" : "s"} already fired
          </span>
          {shownRefs.length > 0 && (
            <span className="font-mono text-faint">
              {shownRefs.join(" ")}
              {refs.length > shownRefs.length && ` +${refs.length - shownRefs.length} more`}
            </span>
          )}
        </button>
      </td>
    </tr>
  );
}

// PageNotice is the page's info notice (role=status, like Alert's info tone). A catalog
// enable's notice also offers the Schedules tab filtered to that job (D7); on the
// Schedules tab the link would only re-apply the filter it just set, so it is harmless.
function PageNotice({
  text,
  jobSlug,
  onShowJob,
}: {
  text: string;
  jobSlug: string | null;
  onShowJob: (slug: string) => void;
}) {
  return (
    <div role="status" className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border border-info/40 bg-info/10 px-3 py-2 text-sm text-info">
      <span>{text}</span>
      {jobSlug && (
        <button
          type="button"
          onClick={() => onShowJob(jobSlug)}
          className="rounded font-medium underline underline-offset-2 hover:text-fg"
        >
          Show it on the Schedules tab
        </button>
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
