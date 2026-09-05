// Kanban board of a repo's issues. Columns are GitLab labels; moves are
// forge-first (the server writes the label, then returns the authoritative
// card — a failed move snaps back because nothing moved optimistically).
// Column identity follows a status-color language:
// every column gets a stable accent dot, Backlog is neutral, Closed is muted. Closed
// is now a real drop target (PRD #1034 M4): dragging an open card into it closes the
// issue, and dragging a closed card onto any open lane reopens it. Cards are content-first: title,
// meta, badges. Live behavior (latest_run badges, 10s visibility-gated polling,
// auto-move toasts, the attention strip, in-app issue links) is PRD #12 M2/M3.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams, Link, useNavigate } from "react-router-dom";
import {
  api,
  ApiError,
  isOpenMRConflict,
  openMRConflictMRIID,
  type Board as BoardData,
  type Card as CardData,
  type RunListItem,
} from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { hasAnthropicToken } from "../lib/hasToken";
import { startRunGate } from "../lib/runStream";
import {
  effectiveRunStatus,
  hasActiveRun,
  isAwaitingApproval,
  isAwaitingFollowup,
  isAwaitingInput,
} from "../lib/runBadge";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { lanePaging, visibleColumns } from "../lib/boardColumns";
import { chipLabels, hoistLabels } from "../lib/labelChips";
import {
  canPromote,
  isUziCard,
  isSelfImproveTracker,
  matchesQuery,
  visibleCards,
} from "../lib/boardCards";
import {
  DEFAULT_SORT_DIR,
  dropIntent,
  insertionEdgeFor,
  isSortDir,
  isSortMode,
  neighbourAnchor,
  sortCards,
  SORT_MODES,
  type DropAnchor,
  type SortDir,
  type SortMode,
} from "../lib/boardOrder";
import { prefs } from "../lib/prefs";
import { Alert, Button, cx, Input, PageHeader, Select, Skeleton } from "../components/ui";
import { FixCiButton, PipelineBadge } from "../components/PipelineBadge";
import { RunIssueRef } from "../components/RunIssueRef";
import { forgePlatform } from "../lib/forgeNoun";
import { PlusIcon, XIcon } from "../components/icons";
import { useAuth } from "../auth/AuthContext";
import { stripUnsafeChars } from "../lib/safeText";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";
import { COLUMN_ACCENTS } from "./board/shared";
import { IssueCard } from "./board/IssueCard";
import { CreateIssueForm } from "./board/CreateIssueForm";
import { IssuesFilter } from "./board/IssuesFilter";
import { ColumnSettings } from "./board/ColumnSettings";

// Re-exported so `import { Board, IssueCard } from "./Board"` (Board.test.tsx) stays
// byte-identical after IssueCard moved to ./board/IssueCard (PRD #1007 M3).
export { IssueCard } from "./board/IssueCard";

const OPEN_KEY = "";
const CLOSED_KEY = "__closed__";

// PRD #304. A lane renders its per-lane cap (default 10) cards, then reveals one PAGE
// (up to 50) at a time. The cap is a RENDER-only slice — cardsByColumn stays full, so
// the reorder anchors still see the whole lane (Decision 2). PER_LANE_OPTIONS is the
// density control's choice set; a hand-edited or missing pref falls back to the default.
const PAGE = 50;
const DEFAULT_PER_LANE = 10;
const PER_LANE_OPTIONS = [5, 10, 20] as const;

// columnKeyForCard maps a card to the key of the column it renders in.
function columnKeyForCard(card: CardData): string {
  if (card.closed) return CLOSED_KEY;
  return card.column;
}

// columnLabel is the human name of the column a card sits in (for toasts).
// "Backlog" is the DISPLAY name of the implicit column (PRD #102 M1); its key is
// still OPEN_KEY ("") and move() still sends the wire string "open" — see move().
function columnLabel(card: CardData): string {
  if (card.closed) return "Closed";
  if (card.column === "") return "Backlog";
  return card.column;
}

export function Board() {
  const { id: repoId = "" } = useParams();
  const navigate = useNavigate();
  const { uziLabel, autopilotLabel } = useAuth();
  const demo = useDemoMode();
  const [board, setBoard] = useState<BoardData | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [syncing, setSyncing] = useState(false);
  const [dragIid, setDragIid] = useState<number | null>(null);
  const [dropTarget, setDropTarget] = useState<string | null>(null);
  const [editingColumns, setEditingColumns] = useState(false);
  const [creatingIssue, setCreatingIssue] = useState(false);
  const [starting, setStarting] = useState<number | null>(null);
  const [promoting, setPromoting] = useState<number | null>(null);

  // Hide-empty-columns toggle, persisted per repo. Initialised lazily from prefs;
  // re-read on repoId change because the route swaps :id without remounting the
  // component (the lazy init only ran for the first repo). See boardColumns.ts —
  // the actual hide decision is derived at render, never stored per column.
  const hideEmptyKey = `uzi.board.${repoId}.hideEmpty`;
  const [hideEmpty, setHideEmpty] = useState(() => prefs.get(hideEmptyKey, false));
  useEffect(() => {
    setHideEmpty(prefs.get(`uzi.board.${repoId}.hideEmpty`, false));
  }, [repoId]);
  const toggleHideEmpty = () => {
    setHideEmpty((v) => {
      const next = !v;
      prefs.set(hideEmptyKey, next);
      return next;
    });
  };

  // "Show all other issues" toggle, SERVER-BACKED per account, per repo via the
  // GET/PUT /board/prefs row. Under PRD #764 membership is the single `uzi` label, so
  // the row's `extra_labels` half is no longer consumed on the client — but it is still
  // round-tripped through `storedExtras` (never mutated here) so the server row stays
  // intact for any future consumer, and `show_all` continues to drive the toggle.
  const [storedExtras, setStoredExtras] = useState<string[] | null>(null);
  const [showAll, setShowAll] = useState(false);

  // Fetch the server prefs on load and re-fetch whenever the route swaps :id without
  // remounting (the same trap hideEmpty/sortMode document above). Errors are
  // non-fatal: the board still renders and the extras fall back to the admin default.
  //
  // MIGRATION (open question 4): the pre-M3 per-browser key `showNonPRD` must not
  // lapse — a user who had deliberately widened their board would otherwise be
  // silently narrowed. On first load, if the server row is PRISTINE (never customised
  // AND show-all off) and the legacy key is set, seed the server once with show-all
  // on, then RETIRE the legacy key so it can never re-trigger (a user who later turns
  // show-all off must stay off). The prefs helper has no removal method, so the key is
  // set false per its get(key, false) convention.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const serverPrefs = await api.getBoardPrefs(repoId);
        if (cancelled) return;
        const legacyKey = `uzi.board.${repoId}.showNonPRD`;
        const pristine = serverPrefs.extra_labels === null && serverPrefs.show_all === false;
        if (pristine && prefs.get<boolean>(legacyKey, false) === true) {
          setStoredExtras(null);
          setShowAll(true);
          // Retire the legacy key first, so a failed seed cannot leave it armed to
          // migrate again on the next load.
          prefs.set(legacyKey, false);
          try {
            await api.setBoardPrefs(repoId, { extra_labels: null, show_all: true });
          } catch {
            // Best-effort seed: the local showAll still holds for this session, and
            // the retired key means the migration will not re-run regardless.
          }
          return;
        }
        setStoredExtras(serverPrefs.extra_labels);
        setShowAll(serverPrefs.show_all);
      } catch {
        // Non-fatal: keep the admin-default fallback (storedExtras stays null).
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [repoId]);

  // The "Issues" popover's open state, its container ref for outside-click, and the
  // trigger button ref so Escape returns focus to it (a keyboard user dismissing the
  // popover must land back on the control, not fall to <body>).
  const [issuesOpen, setIssuesOpen] = useState(false);
  const issuesPopRef = useRef<HTMLDivElement>(null);
  const issuesTriggerRef = useRef<HTMLButtonElement>(null);
  // Dismiss on Escape or an outside click, the BuildInfoPopover pattern. Listeners
  // are attached only while open, so a closed popover costs nothing.
  useEffect(() => {
    if (!issuesOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setIssuesOpen(false);
        // Return focus to the trigger (WCAG 2.4.3): an outside click leaves focus
        // where the pointer landed, but a keyboard Escape must restore it.
        issuesTriggerRef.current?.focus();
      }
    };
    const onDown = (e: MouseEvent) => {
      if (issuesPopRef.current && !issuesPopRef.current.contains(e.target as Node)) {
        setIssuesOpen(false);
      }
    };
    document.addEventListener("keydown", onKey);
    document.addEventListener("mousedown", onDown);
    return () => {
      document.removeEventListener("keydown", onKey);
      document.removeEventListener("mousedown", onDown);
    };
  }, [issuesOpen]);

  // Board-wide sort mode (PRD #102 M5, Decision 7a/8). Per-browser, per repo, exactly
  // like hideEmpty — the ORDER is durable in the DB, but the choice of whether to
  // honour it is a view preference.
  //
  // The default is "manual", which on a board nobody has dragged IS forge_issue_iid
  // ASC (every board_position is NULL and the SQL falls back to iid), so the feature
  // is invisible until someone wants it rather than something they have to go and pick.
  //
  // Read through isSortMode so a stale or hand-edited localStorage value degrades to
  // Manual instead of selecting no comparator. The useEffect re-read mirrors
  // hideEmpty's and is load-bearing for the same reason: the route swaps :id without
  // remounting, so a lazy useState initialiser only ever runs for the first repo. That
  // bug has been fixed once in this file already.
  const sortModeKey = `uzi.board.${repoId}.sortMode`;
  const [sortMode, setSortModeState] = useState<SortMode>(() => {
    const v = prefs.get<string>(sortModeKey, "manual");
    return isSortMode(v) ? v : "manual";
  });
  useEffect(() => {
    const v = prefs.get<string>(`uzi.board.${repoId}.sortMode`, "manual");
    setSortModeState(isSortMode(v) ? v : "manual");
  }, [repoId]);

  // sortDir mirrors sortMode: a per-browser, per-repo view preference (Decision 3).
  // Read through isSortDir so a stale or hand-edited localStorage value degrades to the
  // mode's natural default instead of an invalid direction. The useEffect re-read is
  // load-bearing for the same reason sortMode's is: the route swaps :id without
  // remounting, so a lazy useState initialiser only ever runs for the first repo, and
  // omitting the re-read would keep the previous repo's direction after a route swap.
  const sortDirKey = `uzi.board.${repoId}.sortDir`;
  const [sortDir, setSortDirState] = useState<SortDir>(() => {
    const v = prefs.get<string>(sortDirKey, DEFAULT_SORT_DIR[sortMode]);
    return isSortDir(v) ? v : DEFAULT_SORT_DIR[sortMode];
  });
  useEffect(() => {
    // Read the persisted MODE from storage rather than closing over the sortMode state:
    // on a route swap both [repoId] effects run in one commit pass and setSortModeState
    // has not flushed yet, so the closed-over sortMode is the PREVIOUS repo's. Reading
    // storage makes the DEFAULT_SORT_DIR fallback match the repo we are switching TO,
    // which matters only for a pre-#412 prefs state (a persisted non-manual mode with no
    // persisted sortDir); after #412 setSortMode/setSortDir always persist dir alongside.
    const persistedMode = prefs.get<string>(`uzi.board.${repoId}.sortMode`, "manual");
    const modeDefault = DEFAULT_SORT_DIR[isSortMode(persistedMode) ? persistedMode : "manual"];
    const v = prefs.get<string>(`uzi.board.${repoId}.sortDir`, modeDefault);
    setSortDirState(isSortDir(v) ? v : modeDefault);
  }, [repoId]);
  const setSortDir = useCallback(
    (next: SortDir) => {
      setSortDirState(next);
      prefs.set(`uzi.board.${repoId}.sortDir`, next);
    },
    [repoId],
  );

  // Switching mode resets direction to that mode's natural default (Decision 4): a user
  // picking Title expects A->Z, not a leftover direction from the previous mode; the
  // toggle is then their explicit opt-out. Because applyDrop calls setSortMode("manual"),
  // a drop resets direction too, which is harmless — manual ignores direction.
  const setSortMode = useCallback(
    (next: SortMode) => {
      setSortModeState(next);
      prefs.set(`uzi.board.${repoId}.sortMode`, next);
      const nextDir = DEFAULT_SORT_DIR[next];
      setSortDirState(nextDir);
      prefs.set(`uzi.board.${repoId}.sortDir`, nextDir);
    },
    [repoId],
  );

  // Board search (PRD #304 M2). A live, client-side filter over the already-loaded
  // cards. `q` is the trimmed query; `searchActive` gates every "while searching"
  // behaviour (caps lift, empty lanes drop, the result count). It narrows renderCards
  // AFTER visibleCards and BEFORE the cardsByColumn memo (Decision 6) — payloadCards is
  // never touched, so the reorder freeze stays correct (Decision 2).
  const [query, setQuery] = useState("");
  const q = query.trim();
  const searchActive = q.length > 0;
  // The search input is focused (M5, `/`) via its stable id rather than a ref: the
  // shared <Input> is a plain function component, not forwardRef, so a ref on it would
  // be dropped in React 18 — and ui.tsx is outside this change's scope. Escape blurs via
  // the keydown event's currentTarget, so it needs no handle at all.
  const SEARCH_INPUT_ID = "board-search";

  // Per-lane default (PRD #304 M4). A per-browser, per-repo VIEW preference (density,
  // not policy), so it lives in prefs beside hideEmpty/sortMode — including the
  // useEffect re-read on repoId, because the route swaps :id without remounting and a
  // lazy initialiser only ever runs for the first repo (the trap those two document,
  // fixed in this file once already). A hand-edited or missing value degrades to the
  // default rather than selecting an unsupported cap.
  const perLaneKey = `uzi.board.${repoId}.perLane`;
  const readPerLane = (v: unknown): number =>
    typeof v === "number" && (PER_LANE_OPTIONS as readonly number[]).includes(v) ? v : DEFAULT_PER_LANE;
  const [perLane, setPerLaneState] = useState(() => readPerLane(prefs.get(perLaneKey, DEFAULT_PER_LANE)));
  useEffect(() => {
    setPerLaneState(readPerLane(prefs.get(`uzi.board.${repoId}.perLane`, DEFAULT_PER_LANE)));
  }, [repoId]);
  const setPerLane = (next: number) => {
    setPerLaneState(next);
    prefs.set(perLaneKey, next);
  };

  // Per-lane reveal state (PRD #304 M1/M2): how many cards each lane has been asked to
  // show beyond its baseline. lanePaging() clamps a missing/low entry up to the
  // baseline, so `{}` means "every lane at its starting size". Re-baselined (reset to
  // {}) whenever the search toggles, the per-lane default changes, or the repo changes —
  // each of those moves what the baseline IS, so a stale reveal count would be wrong.
  const [shownCounts, setShownCounts] = useState<Record<string, number>>({});
  useEffect(() => {
    setShownCounts({});
  }, [searchActive, perLane, repoId]);

  // The live insertion point during a drag: which card the pointer is over and which
  // edge. Driven by dragIid + this state, never by reading e.dataTransfer in
  // onDragOver — the HTML drag-and-drop model puts the drag data store in protected
  // mode for every event except dragstart and drop, so getData() there returns "".
  const [insertAt, setInsertAt] = useState<DropAnchor | null>(null);
  // reorderRef is what REFUSES a re-entrant drop; `reordering` only tells the focus
  // effect below when the operation settled. Two of them because a state read inside an
  // event handler sees the value from that render, so two drops in one batch would both
  // see `false` — a ref is updated synchronously and is the only thing that can reject
  // the second. `reordering` no longer disables anything (see B1 below).
  const [reordering, setReordering] = useState(false);
  const reorderRef = useRef(false);
  // Mutation-generation counter (#1060): bumped whenever a board mutation
  // (reorder, column move, close/reopen) acquires OR releases the reorderRef lock
  // below. poll() captures it before its await and discards a response if it
  // changed since — so a background poll whose getBoard was in flight when a
  // mutation applied its authoritative card cannot overwrite it with stale state.
  const mutationGenRef = useRef(0);
  // B1. `reordering` drives the buttons' `disabled`, and DISABLING A FOCUSED BUTTON
  // BLURS IT — the browser moves focus to <body> and never brings it back. Measured by
  // the browser pass every 100ms from 101ms to 901ms: `disabled=true active=BODY`,
  // still BODY after the button was re-enabled, on a card that was mid-column
  // throughout (so not the legitimate end-of-column case).
  //
  // The cost is what makes it blocking rather than cosmetic: it is 38 Tab presses from
  // page start to the first card's move button, so "move this card up three places"
  // becomes 38 tabs, one press, 38 tabs, one press. The buttons exist to satisfy
  // WCAG 2.1.1; this made them single-use, and fails 2.4.3 (Focus Order) as well.
  //
  // TWO SEPARATE FOCUS-LOSS PATHS, and the fix needs both halves:
  //
  //   1. DISABLING the focused button blurred it. Closed by not disabling at all —
  //      reorderRef refuses re-entry synchronously, so `disabled` was only ever an
  //      affordance here. Removing the cause beats compensating for it.
  //   2. The board RE-RENDERS on setBoard and React replaces the button element, which
  //      drops focus even with nothing disabled. That path is real and independent:
  //      measured by the fallback test below, which reddens when this restore is removed
  //      while no button is ever disabled.
  //
  // So the restore stays. It matches on the accessible NAME, which is stable across a
  // within-column move (same card, same direction, same lane).
  const refocusName = useRef<string | null>(null);
  useEffect(() => {
    if (reordering || refocusName.current == null) return;
    const want = refocusName.current;
    refocusName.current = null;
    const buttons = Array.from(document.querySelectorAll<HTMLButtonElement>("button[aria-label]"));
    const exact = buttons.find((b) => b.getAttribute("aria-label") === want);
    // The card may have reached the end of its column, which legitimately disables the
    // button it was moved with. Fall back to the card's OTHER direction so focus stays
    // on the card the user was working with instead of falling to <body> anyway.
    if (exact && !exact.disabled) {
      exact.focus();
      return;
    }
    const sibling = want.includes(" up in ")
      ? want.replace(" up in ", " down in ")
      : want.replace(" down in ", " up in ");
    buttons.find((b) => b.getAttribute("aria-label") === sibling && !b.disabled)?.focus();
  }, [reordering]);

  // Start-run preconditions, refreshed alongside the board: whether the user has a
  // worker and an Anthropic token. Whether an issue already has an active run now
  // comes from the card's own latest_run (no separate listRuns fan-in).
  const [hasWorker, setHasWorker] = useState(false);
  const [hasToken, setHasToken] = useState(false);
  // The viewer's runs on this repo blocked on their approval — drives the
  // attention strip above the columns.
  const [awaitingRuns, setAwaitingRuns] = useState<RunListItem[]>([]);
  // The viewer's runs on this repo parked on a clarification question (PRD #88). A
  // SEPARATE bucket from awaitingRuns, not a widening of it: the strip below names what
  // the run needs, and "needs approval" pointed at a parked question would send the user
  // looking for a plan gate that is not there. Same visual treatment, different words —
  // which is the whole reason awaiting_input is its own status.
  const [questionRuns, setQuestionRuns] = useState<RunListItem[]>([]);
  // PRD #517: the viewer's runs on this repo parked awaiting their next follow-up
  // (awaiting_followup). Its OWN bucket, a sibling to questionRuns rather than folded into
  // it: a follow-up park is not an unanswered question, so the strip names a distinct
  // action ("awaiting follow-up"). Same needs-you classification and loud ring as the two
  // parks above (needsHumanAttention), so it belongs in the "how many need me" tally.
  const [followupRuns, setFollowupRuns] = useState<RunListItem[]>([]);
  // The viewer's runs on this repo the health detector flagged as looking stuck
  // (PRD #47): any non-ok flag on a run the two buckets above do not already carry.
  // Issue runs only (a ci_fix run has no board card to link to).
  //
  // 🔴 THE EXCLUSION IS BY STATUS, NOT BY HEALTH ENUM, and that is the correction issue
  // #182 forced. It used to read `health !== "approval_idle"`, which was a true
  // statement of the same intent only while approval_idle was the ONLY flag an
  // awaiting_approval run could carry. #182 made waiting_worker reachable there, so the
  // enum form started admitting runs the awaiting bucket also holds: two chips, a
  // DUPLICATE React key in the strip below (both arrays are spread into one .map keyed
  // on r.id), and a summary reading "1 run needs approval · 1 run looks stuck" for one
  // run. Keying on the status makes the exclusion mean what it always said it meant, and
  // survives any future flag on these statuses.
  const [stuckRuns, setStuckRuns] = useState<RunListItem[]>([]);

  // Toasts announce auto-moves the poll observes ("#42 → Human Review").
  const [toasts, setToasts] = useState<{ id: number; text: string }[]>([]);
  const toastSeq = useRef(0);
  // Pending toast-removal timers, cleared on unmount so a dismissal never fires
  // setState on an unmounted component.
  const toastTimers = useRef<ReturnType<typeof setTimeout>[]>([]);
  // S5: the announcement text lives in its OWN state, read by a live region that is
  // always mounted (rendered near the top of the tree below). The toast container
  // cannot be that region — it is conditionally rendered, so the region is created in
  // the same tick as its first content, and most assistive tech announces only
  // CHANGES to a region that already existed. A region born holding text is usually
  // silent, which is the worst kind of accessibility bug: the markup looks right.
  //
  // In-repo precedent for the always-mounted shape: RateLimitMeters.tsx renders a
  // permanently empty sr-only role=status whose CONTENT changes. (The M6 brief said
  // that region sits beside the stuck-runs banner — it does not; it is on Settings.
  // The pattern is what transfers, not the location.)
  const [announcement, setAnnouncement] = useState("");
  const pushToast = useCallback((text: string) => {
    const id = (toastSeq.current += 1);
    setToasts((t) => [...t, { id, text }]);
    // Same text to the live region. Kept in step here, in the one function every
    // toast goes through, so a caller cannot announce visually and not audibly.
    setAnnouncement(text);
    const timer = setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 6000);
    toastTimers.current.push(timer);
  }, []);

  // M5: `/` focuses the search input, the ubiquitous "search" shortcut. Only when the
  // user is NOT already typing somewhere (an input, textarea or contenteditable), so it
  // never steals a literal slash mid-word. Guarded/cleaned up like the Issues-popover
  // effect above; a document listener because the target is a specific input, not the
  // focused element.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "/") return;
      const el = document.activeElement as HTMLElement | null;
      const tag = el?.tagName;
      if (tag === "INPUT" || tag === "TEXTAREA" || el?.isContentEditable) return;
      e.preventDefault();
      document.getElementById(SEARCH_INPUT_ID)?.focus();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  // iids the operator moved by hand within the last poll window. The column change
  // is already applied locally, so the background poll must NOT re-announce it as
  // an auto-move — this closes the false-toast window between a drag's server
  // commit and move()'s local setBoard (reviewer SF2). Released after one poll
  // interval; a genuine later auto-move on the same card still toasts.
  const suppressToastIids = useRef<Set<number>>(new Set());
  const suppressTimers = useRef<ReturnType<typeof setTimeout>[]>([]);
  useEffect(
    () => () => {
      toastTimers.current.forEach(clearTimeout);
      suppressTimers.current.forEach(clearTimeout);
    },
    [],
  );

  // boardRef mirrors the rendered board so the background poll can diff a fresh
  // payload against what the user is looking at. Manual drags are already applied
  // here, so they never toast — only server-driven moves do.
  const boardRef = useRef<BoardData | null>(null);
  useEffect(() => {
    boardRef.current = board;
  }, [board]);

  // repoIdRef tracks the repo currently displayed, so an async prefs resync (see
  // persistPrefs) can tell whether it resolved after the route swapped :id — the same
  // stale-repo trap the prefs load effect guards with its `cancelled` flag.
  const repoIdRef = useRef(repoId);
  useEffect(() => {
    repoIdRef.current = repoId;
  }, [repoId]);

  const load = useCallback(async () => {
    setError("");
    try {
      const { board } = await api.getBoard(repoId);
      setBoard(board);
    } catch (err) {
      setError(errorMessage(err, "Failed to load board"));
    } finally {
      setLoading(false);
    }
  }, [repoId]);

  const loadPreconditions = useCallback(async () => {
    try {
      const [{ workers }, { secrets }, { runs }] = await Promise.all([
        api.listWorkers(),
        api.listSecrets(),
        api.listRuns({ repoId }),
      ]);
      setHasWorker(workers.length > 0);
      setHasToken(hasAnthropicToken(secrets));
      // issue #750: classify from the EFFECTIVE status, not the raw one. A run
      // re-planning after a revise keeps status === "awaiting_approval" server-side, but
      // effectiveRunStatus returns "revising" for it (is_revising true), so
      // isAwaitingApproval reads false and it drops out of the attention strip. The next
      // `plan` flips is_revising false server-side and it returns to this bucket.
      setAwaitingRuns(runs.filter((r) => isAwaitingApproval(effectiveRunStatus(r))));
      setQuestionRuns(runs.filter((r) => isAwaitingInput(r.status)));
      setFollowupRuns(runs.filter((r) => isAwaitingFollowup(r.status)));
      setStuckRuns(
        runs.filter(
          (r) =>
            r.health !== "ok" &&
            // Whatever the three buckets above already show, this one must not repeat.
            !isAwaitingApproval(r.status) &&
            !isAwaitingInput(r.status) &&
            // PRD #517: awaiting_followup has its OWN followupRuns bucket now, so exclude it
            // here — otherwise a stale health flag on a parked run would both count in that
            // bucket AND read "looks stuck", and the strip below spreads every bucket into
            // one .map keyed on r.id, so a run in two buckets is a duplicate React key.
            !isAwaitingFollowup(r.status) &&
            // Belt-and-braces for a STALE flag: the server's exit contract clears
            // health on every status transition, so an approval_idle on some other
            // status should not exist — but if one ever did, "looks stuck" would be
            // the wrong words for it.
            r.health !== "approval_idle" &&
            r.issue_iid != null,
        ),
      );
    } catch {
      // Non-fatal: the board still renders; Start-run stays gated conservatively.
    }
  }, [repoId]);

  // poll is the background refresh: re-read the board, toast on any column change
  // the user did not make, and refresh preconditions. Errors are swallowed (keep the
  // last good board; the next tick retries) — only the foreground load surfaces them.
  const poll = useCallback(async () => {
    const gen = mutationGenRef.current;
    try {
      const { board: fresh } = await api.getBoard(repoId);
      // #1060: a mutation started or settled while this poll's getBoard was in
      // flight — its payload predates the mutation's authoritative card, so drop
      // it (toasts included) rather than clobber the just-applied local state.
      if (mutationGenRef.current !== gen) return;
      const prev = boardRef.current;
      if (prev) {
        for (const card of fresh.cards) {
          const before = prev.cards.find((c) => c.iid === card.iid);
          if (
            before &&
            columnKeyForCard(before) !== columnKeyForCard(card) &&
            !suppressToastIids.current.has(card.iid)
          ) {
            pushToast(`#${card.iid} → ${columnLabel(card)}`);
          }
        }
      }
      setBoard(fresh);
    } catch {
      // keep the last good board
    }
  }, [repoId, pushToast]);

  useEffect(() => {
    load();
    loadPreconditions();
  }, [load, loadPreconditions]);

  // Liveness: poll every ~10s while visible, paused when the tab is hidden, so
  // auto-moves and badge changes appear without a manual Refresh. Becoming visible
  // again triggers an immediate catch-up. No WebSocket (out of scope). The shared
  // hook stashes this callback in a ref, so the inline arrow is safe.
  usePollWhileVisible(() => {
    poll();
    loadPreconditions();
  }, 10000);

  const startRun = async (card: CardData) => {
    setError("");
    setStarting(card.iid);
    // createAndOpen runs the create then navigates; the force path reuses it so
    // the retry does not duplicate the navigate.
    const createAndOpen = async (force?: boolean) => {
      const { run } = await api.createRun(repoId, card.iid, force);
      // encodeURIComponent the id: per-call-site open-redirect hardening (see
      // safeNextPath in Login.tsx). A no-op for today's UUID ids.
      navigate(`/runs/${encodeURIComponent(run.id)}`);
    };
    try {
      await createAndOpen();
    } catch (err) {
      // issue_has_open_mr (issue #856): a completed prior run still owns an open
      // MR. Compose a web-specific confirm naming the MR (no --force jargon);
      // confirm, then retry with force.
      if (isOpenMRConflict(err) && err instanceof ApiError) {
        const mr = openMRConflictMRIID(err);
        const detail =
          mr != null ? `an open merge request (!${mr})` : "an open merge request";
        const proceed = window.confirm(
          `This issue already has ${detail} from a completed run. Starting a new run will plan and review it again from scratch. Start a new run anyway?`,
        );
        if (proceed) {
          try {
            await createAndOpen(true);
            return;
          } catch (retryErr) {
            setError(errorMessage(retryErr, "Could not start run"));
          }
        }
        // Declined (or forced retry failed): clear starting, no toast on decline.
        setStarting(null);
        loadPreconditions();
        return;
      }
      setError(errorMessage(err, "Could not start run"));
      setStarting(null);
      loadPreconditions();
    }
  };

  // Fix CI (PRD #6): queue a plan-gated ci_fix run for a failed pipeline's ref.
  // The server enforces the "no active fix / branch free" preconditions and returns
  // a 409 the ApiError surfaces; on success we open the new run.
  const [fixingRef, setFixingRef] = useState<string | null>(null);
  const fixCi = async (ref: string) => {
    setError("");
    setFixingRef(ref);
    try {
      const { run } = await api.createCIFixRun(repoId, ref);
      // encodeURIComponent the id (see safeNextPath in Login.tsx).
      navigate(`/runs/${encodeURIComponent(run.id)}`);
    } catch (err) {
      setError(errorMessage(err, "Could not start CI fix"));
      setFixingRef(null);
    }
  };

  const refresh = async () => {
    setSyncing(true);
    setError("");
    try {
      const { board } = await api.syncRepo(repoId);
      setBoard(board);
    } catch (err) {
      setError(errorMessage(err, "Refresh failed"));
    } finally {
      setSyncing(false);
    }
  };

  // move returns whether the forge write succeeded, so a cross-column drop can stop
  // before it freezes the order (PRD #102 M5). Nothing else about it changed.
  const move = async (toKey: string, iid: number): Promise<boolean> => {
    setError("");
    // Suppress the auto-move toast for this card: a poll landing between the
    // server commit below and the local setBoard would otherwise diff the new
    // column against the stale baseline and false-toast a move the user just made.
    suppressToastIids.current.add(iid);
    try {
      // "open" is the WIRE name of the implicit column, matched server-side with
      // EqualFold (handler/board.go). The M1 Open→Backlog rename is display-only:
      // renaming this literal (or OPEN_KEY) breaks drag-to-Backlog. "closed" is the
      // wire signal the server reads as a close (PRD #1034 M4); dropping a closed card
      // onto an open lane sends that lane's key, which the server reads as a reopen.
      const to = toKey === OPEN_KEY ? "open" : toKey === CLOSED_KEY ? "closed" : toKey;
      const { card } = await api.moveIssue(repoId, iid, to);
      // Forge-first: the server applied the label change and returned the
      // authoritative card. Replace it in place (no optimistic move, so a
      // failure leaves the card where it was — a natural snap-back).
      setBoard((prev) =>
        prev ? { ...prev, cards: prev.cards.map((c) => (c.iid === card.iid ? card : c)) } : prev,
      );
      // Hold the suppression for one poll interval so an already-in-flight poll
      // still sees the moved column as its baseline, then release it.
      const timer = setTimeout(() => suppressToastIids.current.delete(iid), 11000);
      suppressTimers.current.push(timer);
      return true;
    } catch (err) {
      suppressToastIids.current.delete(iid); // the move failed — nothing to suppress
      setError(errorMessage(err, "Move failed"));
      return false;
    }
  };

  // Promote (PRD #102 Decision 15; PRD #764): add the `uzi` label forge-first, then
  // replace the one card with the authoritative response — a no-optimistic-update shape
  // like move(). The promoted card becomes runnable, so it renders Start run afterwards.
  const promote = async (card: CardData) => {
    if (promoting != null) return;
    setPromoting(card.iid);
    try {
      const { card: updated } = await api.promoteIssue(repoId, card.iid);
      setBoard((b) => (b ? { ...b, cards: b.cards.map((c) => (c.iid === updated.iid ? updated : c)) } : b));
    } catch (err) {
      setError(errorMessage(err, "Could not promote the issue"));
    } finally {
      setPromoting(null);
    }
  };

  const columns = useMemo(() => {
    const cols: { key: string; label: string; droppable: boolean; accent: string }[] = [
      { key: OPEN_KEY, label: "Backlog", droppable: true, accent: "bg-faint" },
    ];
    (board?.columns ?? []).forEach((c, i) => {
      cols.push({
        key: c.label_name,
        label: c.label_name,
        droppable: true,
        accent: COLUMN_ACCENTS[i % COLUMN_ACCENTS.length],
      });
    });
    // PRD #1034 M4: Closed is a whole-lane drop target (close on drop) — droppable:true —
    // but it has NO per-card insertion/reorder semantics; the render gates those on
    // `!closedCol` so a drop anywhere in the lane bubbles to the lane onDrop as a close.
    cols.push({ key: CLOSED_KEY, label: "Closed", droppable: true, accent: "bg-edge-strong" });
    return cols;
  }, [board]);

  // Label-chip exclusions (PRD #102 M4, Decision 6). The column set is the SAVED
  // board columns; ColumnSettings' suggester builds its own from unsaved edit state.
  const chipExclusions = useMemo(
    () => ({
      autopilotLabel,
      columnLabels: (board?.columns ?? []).map((c) => c.label_name),
    }),
    [board, autopilotLabel],
  );

  // TWO NAMED CARD SETS, kept distinct even though the filter is the identity today
  // (PRD #102 M5, Decision 7b). This is structure, not decoration: the payload-set rule
  // is the difference between a freeze that is correct and one that silently NULLs
  // every card the viewer had hidden, and the way that rule gets broken is somebody
  // reaching for whichever array is nearest to hand. Naming them makes it structural
  // rather than remembered.
  //
  //   payloadCards — drives the FREEZE. Never filtered.
  //   renderCards  — drives the LANES. M6's non-PRD toggle filters HERE.
  //
  // M6 is the change that makes the split do work: until now the filter was the
  // identity function and any implementation of the freeze passed. With a real
  // card-level filter, using renderCards for the freeze would NULL the position of
  // every card the viewer had hidden — which is what freeze-test 3 discriminates.
  const payloadCards = useMemo<CardData[]>(() => board?.cards ?? [], [board]);

  // The board's single connection's bot forge user id (PRD #767 M5). A card is uzi's to
  // run when it carries the `uzi` label OR this id is one of its assignees. It is
  // per-connection (unlike uziLabel, which is a session setting), so it comes off the
  // board payload, not useAuth(); 0 (unresolved) never marks a card runnable.
  const botForgeUserID = board?.bot_forge_user_id ?? 0;

  // Membership is now a single label (PRD #764, D3): a card is a board member when it
  // carries the `uzi` run-eligibility label. The old per-user `extra_labels` dimension
  // is retired on the client — the board_prefs row is still round-tripped (its
  // `show_all` half drives the toggle below), but its `extra_labels` goes unused here.
  const renderCards = useMemo(
    () => visibleCards(payloadCards, uziLabel, botForgeUserID, showAll),
    [payloadCards, uziLabel, botForgeUserID, showAll],
  );

  // Board search filter (PRD #304 M2, Decision 6): membership → SEARCH → sort → cap.
  // Applied to renderCards (post-membership) and consumed by cardsByColumn, which then
  // sorts and the lane loop slices. An empty query is the identity, so the board reads
  // exactly as it did before search when nothing is typed. payloadCards is untouched —
  // the reorder freeze must never see a filtered set (Decision 2).
  const searchedCards = useMemo(
    () => (q ? renderCards.filter((c) => matchesQuery(c, q)) : renderCards),
    [renderCards, q],
  );

  // M5: announce the result count through the EXISTING sr-only live region (no second
  // region), DEBOUNCED and last-writer-wins. That region is the single-string channel
  // pushToast also feeds, so a per-keystroke announcement would spam assistive tech and
  // race the auto-move toasts — the 300ms timer collapses a burst of typing to one
  // announcement. Only while a search is active; clearing the query stops announcing
  // (it does not announce "0 results" for an emptied box). Declared here, after
  // searchedCards, because it reads its length.
  useEffect(() => {
    if (!searchActive) return;
    const n = searchedCards.length;
    const timer = setTimeout(() => {
      setAnnouncement(`${n} result${n === 1 ? "" : "s"}`);
    }, 300);
    return () => clearTimeout(timer);
  }, [searchActive, searchedCards.length]);

  // persistPrefs writes the server row OPTIMISTICALLY (PRD #196 M3): the caller has
  // already updated local state so the toggle feels instant. On a failed PUT surface
  // the error and reload the row to resync, so the client never silently diverges from
  // the server.
  const persistPrefs = useCallback(
    async (extra_labels: string[] | null, show_all: boolean) => {
      try {
        await api.setBoardPrefs(repoId, { extra_labels, show_all });
      } catch (err) {
        setError(errorMessage(err, "Could not save your board view"));
        try {
          const fresh = await api.getBoardPrefs(repoId);
          // Guard the same stale-repo trap the load effect guards: the route swaps
          // :id without remounting, so a resync resolving after the user navigated to
          // another repo would clobber that repo's prefs with this one's. The load
          // effect owns the fresh repo's state; a late resync for the old repo must not.
          if (repoIdRef.current !== repoId) return;
          setStoredExtras(fresh.extra_labels);
          setShowAll(fresh.show_all);
        } catch {
          // Leave the optimistic state; the next foreground load reconciles it.
        }
      }
    },
    [repoId],
  );
  const toggleShowAll = useCallback(() => {
    const next = !showAll;
    setShowAll(next);
    persistPrefs(storedExtras, next);
  }, [showAll, storedExtras, persistPrefs]);

  // The "Show all other issues" population: cards that are not `uzi` members, not the
  // self-improve tracker, and not closed — the non-runnable OPEN issues the toggle
  // reveals. The `!closed` guard matches visibleCards so the count and the rendered set
  // agree on which "other issues" exist.
  const showAllCount = useMemo<number>(
    () => payloadCards.filter((c) => !isUziCard(c, uziLabel, botForgeUserID) && !isSelfImproveTracker(c) && !c.closed).length,
    [payloadCards, uziLabel, botForgeUserID],
  );

  // columnKeys is the board's lane order for the freeze: the implicit Backlog column
  // first, then the configured ones. Closed is excluded — closed cards never enter the
  // freeze at all.
  const columnKeys = useMemo(
    () => [OPEN_KEY, ...(board?.columns ?? []).map((c) => c.label_name)],
    [board],
  );

  const cardsByColumn = useMemo(() => {
    const map = new Map<string, CardData[]>();
    for (const c of sortCards(searchedCards, sortMode, sortDir)) {
      const key = columnKeyForCard(c);
      const arr = map.get(key) ?? [];
      arr.push(c);
      map.set(key, arr);
    }
    // S4. THE CLOSED LANE NOW HONOURS mode+direction like every other lane (#412).
    // Previously an INTENTIONAL pin here re-sorted Closed to iid order in ALL modes:
    //   const closed = map.get(CLOSED_KEY);
    //   if (closed) map.set(CLOSED_KEY, [...closed].sort((a, b) => a.iid - b.iid));
    // #412 removed that pin so Closed flows through the same sortCards() bucketing as
    // every other lane. In `manual` mode sortCards is identity, so Closed still renders
    // issue-number ascending exactly as before — only the non-manual modes now reach it.
    //
    // ACCEPTED, KNOWN cost of removing the pin: a drop calls setSortMode("manual"), and
    // because the drop-freeze excludes closed cards (Decision 7b), a drop taken from a
    // non-manual mode leaves the OPEN lanes holding their just-frozen (unchanged) order
    // while the CLOSED lane alone reverts from mode-order to iid-order — the exact
    // isolated reshuffle the old pin used to hide. This is accepted because it fires only
    // on an actual drop, manual-mode iid IS the correct Closed identity ordering, and the
    // alternative (a permanent iid pin) is precisely the cross-lane inconsistency #412
    // removes. An M3 test asserts this post-drop Closed state — DO NOT re-add the pin to
    // "fix" it, as that silently reverts the feature.
    return map;
  }, [searchedCards, sortMode, sortDir]);

  // applyDrop is THE single order-computing path. A pointer drop and a keyboard ↑/↓
  // both land here with the same three arguments, so the two gestures cannot drift:
  // if a reviewer finds a second one, that is the defect, not an implementation detail.
  //
  // Sequence for a cross-column drop is move() THEN reorder, which refines Decision
  // 7b's "freeze, then move" rather than contradicting it — 7b's worked example is a
  // WITHIN-column drag, so its "move" is the reposition, and that is folded into the
  // freeze here (one write, no insert-between mechanism). The order of the column
  // LABEL write against the freeze is a case 7b never reaches. Move-first, because
  // reorder returns the authoritative board and the client adopts it: freeze-first
  // would render the new ORDER while the card is still in its OLD column (the forge
  // call has not returned), so the card visibly snaps back to its old lane and jumps
  // again about a second later. A failed forge write also then leaves NOTHING written,
  // which is the existing snap-back contract; freeze-first would renumber the board
  // around a move that never happened.
  //
  // THE FREEZE IS UNCONDITIONAL, including a drop while already in Manual mode.
  // Gating it on "mode != manual" reintroduces the exact bug Decision 7b exists to
  // prevent: on an untouched board every position is NULL, so one written position
  // sorts ahead of every NULL under NULLS LAST and a card dragged to the BOTTOM of its
  // column renders at the TOP.
  //
  // RETURNS whether the board actually moved. Callers that ANNOUNCE the outcome must
  // gate on it: swallowing the rejection into setError and resolving normally made
  // moveCard's toast fire on failure, so the board rendered "Could not save the new
  // order" and "#1 moved down in Backlog" at the same time. That is worst on the
  // keyboard path, where the toast IS the feedback and the user cannot see that the
  // card did not move. Same reason move() already returns Promise<boolean>.
  //
  // Re-entrant calls are REFUSED, not queued. reorderRef (not the `reordering` state)
  // is the guard, because two drops inside one React batch would both read the same
  // pre-update state value. Without it a second drop computes its order from the same
  // pre-first-drop payloadCards and silently discards the first — Decision 7b's
  // accepted last-writer-wins is about CROSS-TAB concurrency resolved by the 10s
  // refetch, not two drags in one tab in one second.
  const applyDrop = useCallback(
    async (dragIid: number, destKey: string, anchor: DropAnchor | null): Promise<boolean> => {
      // PRD #1034 M4: close/reopen are STATE changes, not reorders, so they route through
      // move() alone and never touch the lane freeze. This branch sits at the very TOP of
      // applyDrop, before the reorderRef guard, because there is no position to compute: a
      // close leaves the card in place, and a reopen lands at the bottom of its new lane
      // because the server nulls board_position (ORDER BY board_position ASC NULLS LAST).
      // dropIntent's own `dragged.closed` bail therefore stays as a safety net — a closed
      // card is short-circuited to move() here and never reaches dropIntent through this
      // path, but the bail guarantees it could never be ranked even if it did.
      const draggedCard = payloadCards.find((c) => c.iid === dragIid);
      if (!draggedCard) return false;
      const toClosed = destKey === CLOSED_KEY;
      if (toClosed && draggedCard.closed) return false; // Closed → Closed: no-op, no mutation

      // ONE synchronous lock across ALL board mutations — state moves (close/reopen) AND
      // reorders. reorderRef must be checked BEFORE the state-move branches below: a
      // close/reopen that ran while a reorder's reorderBoard() was still in flight could
      // have the older reorder response resolve last and setBoard(fresh) overwrite the
      // newer state-transition card with stale board data (and, symmetrically, a reorder
      // starting under a pending close/reopen). Refusing a re-entrant mutation beats
      // losing one, and silence is not the trade-off — every other rejection here speaks
      // (move() surfaces a forge error, a failed reorder sets "Could not save the new
      // order"), so on a slow link a user who drags twice is told, not left guessing.
      if (reorderRef.current) {
        setError("Still saving the previous move — try again in a moment.");
        return false;
      }

      // State changes (close/reopen) route through move() alone — there is no position to
      // compute (a close leaves the card in place; a reopen lands at the bottom of its new
      // lane because the server nulls board_position, ORDER BY board_position ASC NULLS
      // LAST) — but they still hold the SAME lock so a reorder cannot interleave with the
      // pending forge write. dropIntent's own `dragged.closed` bail stays a safety net.
      if (toClosed || draggedCard.closed) {
        reorderRef.current = true;
        mutationGenRef.current++;
        setReordering(true);
        try {
          // open → Closed = close (state only); closed → open lane = reopen + move (bottom).
          return await move(toClosed ? CLOSED_KEY : destKey, dragIid);
        } finally {
          reorderRef.current = false;
          mutationGenRef.current++;
          setReordering(false);
        }
      }
      const intent = dropIntent({
        payloadCards, // UNFILTERED — the payload-set rule (Decision 7b)
        columnKeys,
        sortMode,
        sortDir,
        dragIid,
        destColumnKey: destKey,
        anchor,
      });
      if (!intent) return false;
      reorderRef.current = true;
      mutationGenRef.current++;
      setReordering(true);
      try {
        if (intent.columnChanged && !(await move(destKey, dragIid))) {
          return false; // move() surfaced the error and left the card where it was
        }
        const { board: fresh } = await api.reorderBoard(repoId, intent.iids);
        // No optimistic reorder: the card does not visually move until the server's
        // authoritative board replaces this one, matching move()'s snap-back contract.
        setBoard(fresh);
        setSortMode("manual");
        // Auto-reveal (PRD #304 M1): a card moved or dropped PAST the per-lane cap must
        // not vanish into the hidden window. intent.iids is the destination column's
        // full new iid order, so the dragged card's index there is its rank in the lane;
        // bump the lane's shownCount to include it. Over-revealing slightly is safe.
        // moveCard routes through applyDrop, so keyboard moves are covered too.
        const idx = intent.iids.indexOf(dragIid);
        if (idx >= 0) {
          setShownCounts((m) => ({ ...m, [destKey]: Math.max(m[destKey] ?? 0, idx + 1) }));
        }
        return true;
      } catch (err) {
        setError(errorMessage(err, "Could not save the new order"));
        return false;
      } finally {
        reorderRef.current = false;
        mutationGenRef.current++;
        setReordering(false);
      }
    },
    // move/setBoard/setError are stable enough for this callback's purpose; the state
    // it genuinely depends on is listed.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [payloadCards, columnKeys, sortMode, sortDir, repoId, setSortMode],
  );

  // moveCard is the keyboard path (PRD #102 M5, lead ruling 1). It builds the anchor a
  // drop would have produced and hands it to the SAME applyDrop, so there is no second
  // reorder call and no second freeze. columnChanged is always false here, so the
  // forge is never touched by a keyboard move.
  const moveCard = useCallback(
    async (card: CardData, direction: "up" | "down") => {
      const lane = cardsByColumn.get(columnKeyForCard(card)) ?? [];
      const anchor = neighbourAnchor(lane, card.iid, direction);
      if (!anchor) return; // already at that end
      // Recorded BEFORE the await, because `reordering` flips to true inside applyDrop
      // and the button is blurred the moment it is disabled.
      refocusName.current = `Move issue #${card.iid} ${direction} in ${columnLabel(card)}`;
      // ONLY on success. This is the announcement channel for users who cannot see the
      // board, so a toast that fires regardless is not a cosmetic slip — it is the
      // feedback saying the opposite of what happened.
      if (await applyDrop(card.iid, columnKeyForCard(card), anchor)) {
        pushToast(`#${card.iid} moved ${direction} in ${columnLabel(card)}`);
      }
    },
    [cardsByColumn, applyDrop, pushToast],
  );

  if (loading) {
    return (
      <div className="space-y-5">
        <Skeleton className="h-8 w-64" />
        <div className="flex gap-4">
          {Array.from({ length: 4 }, (_, i) => (
            <Skeleton key={i} className="h-72 w-72 shrink-0" />
          ))}
        </div>
      </div>
    );
  }

  // Columns to render: hide-empty is derived here from the freshly-polled cards,
  // never stored, so an auto-move that populates a hidden column reveals it on the
  // next poll. A live drag reveals every lane so they stay drop targets, which
  // drives hiddenCount to 0 — the toolbar hint (gated on hiddenCount > 0) simply
  // disappears mid-drag rather than reading "0 hidden".
  //
  // PRD #304 M2: an active search ALSO drops empty lanes — a query with no match in a
  // lane should not leave that lane's header taking space — so it composes with
  // hideEmpty here (`hideEmpty || searchActive`). It still yields to a live drag, which
  // reveals every lane, because visibleColumns returns all columns when dragActive.
  const visible = visibleColumns(
    columns,
    (key) => cardsByColumn.get(key)?.length ?? 0,
    hideEmpty || searchActive,
    dragIid != null,
  );
  const hiddenCount = columns.length - visible.length;

  // The board's own description, hoisted out of the JSX so the reason it changed can
  // be written down. This is the one sentence a user actually reads about what this
  // board contains, rendered product copy invisible to any sweep of docs or code
  // comments. It names the CONFIGURED `uzi` label, since uzi_label is renameable
  // (PRD #764): runnable issues carry it, and "Issues" reveals the rest.
  const boardDescription =
    `Columns are ${forgePlatform(board?.forge_type)} labels. Cards move automatically as their runs progress; ` +
    `you can still drag a card to change its label on the forge. ${uziLabel}-labeled issues are uzi's to run; ` +
    `use "Issues" to show all other open issues alongside them.`;

  return (
    <div className="space-y-5">
      {/* M3: the toolbar pins so search stays reachable while a long lane scrolls. The
          app shell scrolls the WINDOW and neither <main> owns an overflow ancestor
          (AppShell.tsx), so plain `sticky top-0` is expected to hold; z-10 sits below
          the mobile shell's own sticky top bar (z-20). bg-ink is the page-background
          token (tailwind.config.js: `ink: token("bg")`) — an OPAQUE backing so cards
          scroll cleanly under it (plain `bg-bg` is not a real class and renders
          transparent). On mobile the shell's own bar is a 49px-tall sticky top-0 z-20
          strip, so offset below it; on lg the shell bar is hidden and we pin at 0.
          Verified in a browser pass. */}
      <div className="sticky top-[49px] z-10 bg-ink lg:top-0">
      <PageHeader
        backTo="/repos"
        backLabel="Boards"
        titleNode={
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="text-xl font-semibold tracking-tight">{board?.path_with_namespace ? maskRepoPath(board.path_with_namespace, demo) : "Board"}</h1>
            {board?.pipeline && <PipelineBadge pipeline={board.pipeline} />}
            {board?.pipeline && (
              <FixCiButton
                pipeline={board.pipeline}
                busy={fixingRef === board.pipeline.ref}
                onClick={() => fixCi(board.pipeline!.ref)}
              />
            )}
          </div>
        }
        description={boardDescription}
        actions={
          <>
            {/* M3: board search. First in the toolbar so it is the primary action; the
                label is sr-only (the placeholder carries the visible affordance) and the
                ref lets `/` focus it (M5). Escape clears the query and blurs. */}
            <label htmlFor={SEARCH_INPUT_ID} className="sr-only">
              Search issues
            </label>
            <Input
              id={SEARCH_INPUT_ID}
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Escape") {
                  setQuery("");
                  e.currentTarget.blur();
                }
              }}
              placeholder="Search issues…"
              className="w-44 py-1 text-xs"
            />
            {/* A real <label> around the repo's Select, so the control is named for a
                screen reader without any custom markup. */}
            <label className="flex select-none items-center gap-1.5 py-1.5 text-xs text-muted">
              Sort
              <Select
                value={sortMode}
                onChange={(e) => setSortMode(e.target.value as SortMode)}
                className="py-1 text-xs"
              >
                {SORT_MODES.map((m) => (
                  <option key={m.value} value={m.value}>
                    {m.label}
                  </option>
                ))}
              </Select>
            </label>
            {/* Direction toggle (Decision 6). DISABLED (not hidden) in manual mode so the
                toolbar layout stays stable and the control remains discoverable; manual
                ignores direction. Carries an accessible name and aria-pressed for the
                reversed state, plus a visible arrow and text label. When disabled in
                manual mode aria-pressed is omitted, so the inert control does not advertise
                a stale pressed state to a screen reader. */}
            <Button
              type="button"
              variant="secondary"
              size="sm"
              className="py-1 text-xs"
              disabled={sortMode === "manual"}
              aria-pressed={sortMode === "manual" ? undefined : sortDir === "desc"}
              aria-label={`Sort direction: ${sortDir === "asc" ? "ascending" : "descending"}`}
              onClick={() => setSortDir(sortDir === "asc" ? "desc" : "asc")}
            >
              {sortDir === "asc" ? "↑ Ascending" : "↓ Descending"}
            </Button>
            {/* Per-lane density (PRD #304 M4), mirroring the Sort control's markup.
                Changing it re-baselines every lane's shownCount (the effect on
                [searchActive, perLane, repoId]). */}
            <label className="flex select-none items-center gap-1.5 py-1.5 text-xs text-muted">
              Per lane
              <Select
                value={String(perLane)}
                onChange={(e) => setPerLane(Number(e.target.value))}
                className="py-1 text-xs"
              >
                {PER_LANE_OPTIONS.map((n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </Select>
            </label>
            {/* The "Issues" popover (PRD #764): a simple `uzi`-only / all toggle. The
                board renders only `uzi`-labelled (runnable) issues until "Show all other
                issues" is ticked. The button names the CONFIGURED `uzi` label (uzi_label
                is renameable) and borders in the brand tint when all issues are shown. */}
            <IssuesFilter
              open={issuesOpen}
              onToggleOpen={() => setIssuesOpen((o) => !o)}
              popRef={issuesPopRef}
              triggerRef={issuesTriggerRef}
              uziLabel={uziLabel}
              showAll={showAll}
              showAllCount={showAllCount}
              onToggleShowAll={toggleShowAll}
            />
            <label className="flex cursor-pointer select-none items-center gap-1.5 py-1.5 text-xs text-muted">
              <input
                type="checkbox"
                checked={hideEmpty}
                onChange={toggleHideEmpty}
                className="h-3.5 w-3.5 rounded border-edge accent-brand"
              />
              Hide empty
              {hiddenCount > 0 && <span className="text-muted">({hiddenCount} hidden)</span>}
            </label>
            <Button size="sm" onClick={() => setCreatingIssue((v) => !v)}>
              {creatingIssue ? (
                <>
                  <XIcon /> Close
                </>
              ) : (
                <>
                  <PlusIcon /> Create issue
                </>
              )}
            </Button>
            <Button variant="secondary" size="sm" onClick={() => setEditingColumns((v) => !v)}>
              {editingColumns ? "Close settings" : "Columns"}
            </Button>
            <Button variant="secondary" size="sm" disabled={syncing} onClick={refresh}>
              {syncing ? "Refreshing…" : "Refresh"}
            </Button>
          </>
        }
      />
      {/* Board-level result count (PRD #304 M2): only while a search is active, so a
          user knows the board is filtered and by how much. Pinned with the toolbar.
          Purely VISUAL — deliberately NOT a live region: the count is announced through
          the existing sr-only role=status region, debounced (M5), so a second announcing
          region here would spam assistive tech on every keystroke. */}
      {searchActive && (
        <p className="px-1 pb-2 text-xs text-muted">
          {searchedCards.length} result{searchedCards.length === 1 ? "" : "s"}
        </p>
      )}
      </div>

      {/* S5: ALWAYS MOUNTED, and that is the entire fix. A live region must exist
          before its content changes for assistive tech to announce the change; a
          region rendered into existence together with its first message is typically
          silent. Empty until something is announced, and sr-only because the visible
          toast stack already carries the message for everyone else. */}
      <div className="sr-only" role="status" aria-live="polite">
        {announcement}
      </div>

      {error && <Alert message={error} />}

      {(awaitingRuns.length > 0 ||
        questionRuns.length > 0 ||
        followupRuns.length > 0 ||
        stuckRuns.length > 0) && (
        <div
          role="status"
          aria-live="polite"
          className="flex flex-wrap items-center gap-x-2 gap-y-1 rounded-lg border border-warn/40 bg-warn/10 px-4 py-2.5 text-sm text-warn"
        >
          <span className="font-medium">
            {[
              awaitingRuns.length > 0 &&
                `${awaitingRuns.length} run${awaitingRuns.length > 1 ? "s" : ""} ${awaitingRuns.length > 1 ? "need" : "needs"} approval`,
              // PRD #88: its own clause, so the strip says which action is owed.
              questionRuns.length > 0 &&
                `${questionRuns.length} run${questionRuns.length > 1 ? "s" : ""} ${questionRuns.length > 1 ? "need" : "needs"} an answer`,
              // PRD #517: its own clause too — a follow-up park owes a different action than
              // an unanswered question, so it names "awaiting follow-up" rather than folding
              // into the answer count.
              followupRuns.length > 0 &&
                `${followupRuns.length} run${followupRuns.length > 1 ? "s" : ""} awaiting follow-up`,
              stuckRuns.length > 0 &&
                `${stuckRuns.length} run${stuckRuns.length > 1 ? "s" : ""} ${stuckRuns.length > 1 ? "look" : "looks"} stuck`,
            ]
              .filter(Boolean)
              .join(" · ")}
          </span>
          {[...awaitingRuns, ...questionRuns, ...followupRuns, ...stuckRuns].map((r) => (
            // Issue #485 NB1: the forge issue anchor rendered by RunIssueRef is a real
            // <a>, so it can no longer nest inside the navigational <Link> (also an <a> —
            // invalid HTML). The pill span is a `relative` container; the run-details
            // <Link> is a stretched absolute overlay, and the interactive ref is raised
            // above it (relative z-10). The pill's hover-bg lives on the container, so
            // hovering the transparent overlay (a descendant) still tints it.
            <span
              key={r.id}
              className="relative inline-flex items-center gap-1 rounded-md border border-warn/40 px-1.5 py-0.5 text-warn transition-colors hover:bg-warn/20"
            >
              <Link
                to={`/runs/${r.id}`}
                // Issue #485 review FIX 2: name the link by the run's title so several
                // issue-less runs in the strip get distinct, meaningful accessible names
                // instead of a repeated bare "Open run". issue_title is untrusted forge
                // text, so it stays sanitized via stripUnsafeChars.
                aria-label={`Open run${r.issue_iid != null ? ` for issue #${r.issue_iid}` : ""}${
                  r.issue_title.trim() ? `: ${stripUnsafeChars(r.issue_title)}` : ""
                }`}
                className="absolute inset-0 rounded-md"
              />
              <RunIssueRef
                issueIid={r.issue_iid}
                issueWebUrl={r.issue_web_url}
                kind={r.kind}
                forgeType={r.forge_type}
                raised
                tone="inherit"
              />{" "}
              →
            </span>
          ))}
        </div>
      )}

      {creatingIssue && (
        <CreateIssueForm
          repoId={repoId}
          forgeType={board?.forge_type ?? ""}
          onCreated={() => {
            setCreatingIssue(false);
            load();
            loadPreconditions();
          }}
          onError={setError}
        />
      )}

      {editingColumns && board && (
        <ColumnSettings
          board={board}
          onSaved={(b) => {
            setBoard(b);
            setEditingColumns(false);
          }}
          onError={setError}
        />
      )}

      {/* Issue #367 → #373: columns scroll HORIZONTALLY in one row (overflow-x-auto)
          again; the page still scrolls vertically for tall columns. Reverts the
          flex-wrap of Decision 2 (Option A), which stacked columns onto new rows when
          they exceeded the width and killed horizontal card scroll. */}
      <div className="flex items-start gap-4 overflow-x-auto pb-4">
        {visible.map((col) => {
          const cards = cardsByColumn.get(col.key) ?? [];
          // Per-lane paging (PRD #304 M1/M2). `cards` stays FULL — it drives the count
          // badge, the reorder-anchor gating (canMoveDown reads cards.length) and the
          // append-to-end drop — while `shown` is the render-only slice. A search lifts
          // the cap to a page (searchActive → baseline = page inside lanePaging).
          const paging = lanePaging({
            total: cards.length,
            shownCount: shownCounts[col.key] ?? 0,
            cap: perLane,
            page: PAGE,
            searchActive,
          });
          const shown = cards.slice(0, paging.render);
          const isTarget = dropTarget === col.key && col.droppable;
          const closedCol = col.key === CLOSED_KEY;
          // An empty lane only visible because a drag is in progress: dim it so it
          // reads as a transient drop target, not a real column.
          const dragRevealed = hideEmpty && dragIid != null && cards.length === 0;
          return (
            <div
              key={col.key || "open"}
              onDragOver={(e) => {
                if (!col.droppable) return;
                e.preventDefault();
                setDropTarget(col.key);
              }}
              onDragLeave={() => setDropTarget((t) => (t === col.key ? null : t))}
              onDrop={(e) => {
                // The LANE drop now means "append to the end of this column", which is
                // what it already effectively did — a card-level drop stopPropagation()s
                // so this only fires on lane whitespace or an empty lane.
                e.preventDefault();
                setDropTarget(null);
                setInsertAt(null);
                const iid = Number(e.dataTransfer.getData("text/plain"));
                if (iid) applyDrop(iid, col.key, null);
              }}
              className={cx(
                // Fixed-width, non-shrinking columns in one row; the row's overflow-x-auto
                // gives back horizontal card scroll (#373, reverting Decision 2's grow-wrap).
                "flex w-72 shrink-0 flex-col rounded-xl border p-2.5 transition-colors",
                dragRevealed && "opacity-60",
                isTarget
                  ? "border-brand/70 bg-brand/5 ring-1 ring-brand/40"
                  : closedCol
                    ? "border-dashed border-edge bg-transparent"
                    : "border-edge bg-surface/60",
              )}
            >
              {/* Static header (#373): sticky pinning is incompatible with the row's
                  overflow-x-auto (a non-visible overflow-x forces overflow-y:auto, which
                  makes the row — not the viewport — the sticky scroll context). We chose
                  horizontal card scroll over pinned headers, so the header is plain again. */}
              <div className="mb-2.5 flex items-center gap-2 px-1">
                <span aria-hidden="true" className={cx("h-2 w-2 rounded-full", col.accent)} />
                <span className={cx("text-sm font-semibold", closedCol ? "text-faint" : "text-fg")}>
                  {col.label}
                </span>
                <span className="ml-auto rounded-md bg-raised px-1.5 py-0.5 text-[11px] tabular-nums text-faint">
                  {/* `render/total` when the lane is capped (PRD #304 M2's N/M), else the
                      plain total. */}
                  {paging.countLabel || String(cards.length)}
                </span>
              </div>
              {/* Issue #367 (Decision 1): the per-lane max-h/overflow box is gone —
                  the lane grows to fit its cards and the page scrolls as one plane, so
                  an expanded lane ("Show N more") grows the page rather than opening an
                  inner scrollbar. `paging.canCollapse` still drives the Show-more/Collapse
                  controls; it just no longer gates a scroll box. */}
              <div className="flex flex-col gap-2">
                {shown.map((card, i) => {
                  // Runnability is the single `uzi` label (PRD #764): a card carrying it
                  // is uzi's to run and renders solid with Start run; one without it is
                  // quiet and gets Promote. The matched `uzi` chip is hoisted ahead of
                  // MAX_CARD_CHIPS and highlighted so it reads as the runnable marker.
                  const isEligible = isUziCard(card, uziLabel, botForgeUserID);
                  const matchedUzi = isEligible ? [uziLabel] : [];
                  return (
                  <IssueCard
                    key={card.iid}
                    card={card}
                    repoId={repoId}
                    projectWebUrl={board?.web_url}
                    chips={hoistLabels(chipLabels(card.labels, chipExclusions), matchedUzi)}
                    highlightLabels={matchedUzi}
                    // The active query, so the card highlights the matched substring in
                    // its title and any matching chip (PRD #304 M2). "" when not searching.
                    query={q}
                    laneLabel={col.label}
                    // Reorder controls exist only in a droppable lane, and only where
                    // there is somewhere to go. Closed is not a drop target, so its
                    // cards get neither.
                    canMoveUp={col.droppable && !card.closed && i > 0}
                    canMoveDown={col.droppable && !card.closed && i < cards.length - 1}
                    onMoveUp={() => moveCard(card, "up")}
                    onMoveDown={() => moveCard(card, "down")}
                    // Never an edge on the dragged card itself.
                    insertionEdge={
                      col.droppable && !closedCol && insertAt?.iid === card.iid && dragIid !== card.iid
                        ? insertAt.before
                          ? "top"
                          : "bottom"
                        : null
                    }
                    onCardDragOver={
                      col.droppable && !closedCol
                        ? (e) => {
                            // preventDefault or the drop is refused. Deliberately NOT
                            // stopPropagation: the lane's own onDragOver still has to
                            // fire to keep the lane highlight.
                            e.preventDefault();
                            if (dragIid == null || dragIid === card.iid) return;
                            const r = e.currentTarget.getBoundingClientRect();
                            const edge = insertionEdgeFor(r.top, r.height, e.clientY);
                            setInsertAt((prev) =>
                              prev && prev.iid === card.iid && prev.before === (edge === "top")
                                ? prev
                                : { iid: card.iid, before: edge === "top" },
                            );
                          }
                        : undefined
                    }
                    onCardDragLeave={
                      col.droppable && !closedCol
                        ? () => setInsertAt((prev) => (prev?.iid === card.iid ? null : prev))
                        : undefined
                    }
                    onCardDrop={
                      col.droppable && !closedCol
                        ? (e) => {
                            // stopPropagation is REQUIRED: without it the lane's onDrop
                            // runs afterwards and issues a second, anchor-less drop.
                            e.preventDefault();
                            e.stopPropagation();
                            const iid = Number(e.dataTransfer.getData("text/plain"));
                            const r = e.currentTarget.getBoundingClientRect();
                            const edge = insertionEdgeFor(r.top, r.height, e.clientY);
                            setDropTarget(null);
                            setInsertAt(null);
                            if (iid) applyDrop(iid, col.key, { iid: card.iid, before: edge === "top" });
                          }
                        : undefined
                    }
                    gate={startRunGate({
                      closed: card.closed,
                      hasWorker,
                      hasToken,
                      activeRunExists: hasActiveRun(card.latest_run),
                    })}
                    starting={starting === card.iid}
                    onStart={() => startRun(card)}
                    fixCiBusy={card.pipeline != null && fixingRef === card.pipeline.ref}
                    onFixCi={() => card.pipeline && fixCi(card.pipeline.ref)}
                    uziLabel={uziLabel}
                    isEligible={isEligible}
                    canPromote={canPromote(card, uziLabel, botForgeUserID)}
                    promoting={promoting === card.iid}
                    onPromote={() => promote(card)}
                    onDragStart={(e) => {
                      e.dataTransfer.setData("text/plain", String(card.iid));
                      setDragIid(card.iid);
                    }}
                    onDragEnd={() => {
                      setDragIid(null);
                      setInsertAt(null);
                    }}
                    dimmed={dragIid === card.iid}
                  />
                  );
                })}
                {/* N2: the drop-at-END affordance. Hovering a lane's whitespace
                    highlighted the lane and showed NOTHING about where the card would
                    land — while hovering any card showed a precise insertion edge. So
                    the one drop with no feedback was the one whose result the user is
                    least able to predict, and the lane's own whitespace below the last
                    card is only its p-2.5 (measured: cards ended at y=703, lane bottom
                    714, an ~11px strip).

                    This renders exactly when the pointer is over THIS lane and over no
                    card — which is the state the lane's onDrop already treats as
                    "append to the end", so the indicator and the behaviour are the
                    same condition rather than two that can drift.

                    It also enlarges the target as a side effect: the bar is a real
                    element in the flow, so the ~11px strip becomes a full row. That
                    was the cosmetic half of the finding; the feedback is the real one.

                    Not rendered for an empty lane — the "Drop a card here" placeholder
                    below already says it, and two indicators would contradict. */}
                {/* Not in the Closed lane (PRD #1034 M4): a drop there is a CLOSE, not an
                    append-at-rank, so the "drop at the end" cue would misdescribe it. */}
                {dragIid != null && isTarget && insertAt == null && cards.length > 0 && !closedCol && (
                  <div
                    aria-hidden="true"
                    className="rounded-lg border-2 border-dashed border-brand/70 py-3 text-center text-[11px] text-brand"
                  >
                    Drop at the end
                  </div>
                )}
                {cards.length === 0 && (
                  <p className="rounded-lg border border-dashed border-edge py-6 text-center text-xs text-faint">
                    {closedCol ? "Nothing closed yet" : "Drop a card here"}
                  </p>
                )}
              </div>
              {/* Paging affordances (PRD #304 M1). Outside the bounded scroll region
                  above so they stay visible below a scrolling card list. Show more
                  reveals one page; Collapse returns the lane to its baseline; and past
                  two pages of remainder a nudge points at search rather than a "show
                  all" that would rebuild the wall. Accessible names carry the counts /
                  lane name (M5). */}
              {(paging.showMoreBy > 0 || paging.canCollapse || (paging.nudgeSearch && !searchActive)) && (
                <div className="mt-2 flex flex-col gap-1.5">
                  {paging.showMoreBy > 0 && (
                    <button
                      type="button"
                      onClick={() =>
                        setShownCounts((m) => ({
                          ...m,
                          [col.key]: (m[col.key] ?? (searchActive ? PAGE : perLane)) + PAGE,
                        }))
                      }
                      aria-label={`Show ${paging.showMoreBy} more in ${col.label}, ${paging.remaining} hidden`}
                      className="rounded-md border border-edge bg-raised px-2 py-1 text-xs text-muted transition-colors hover:text-fg"
                    >
                      Show {paging.showMoreBy} more · {paging.remaining} left
                    </button>
                  )}
                  {paging.canCollapse && (
                    <button
                      type="button"
                      onClick={() =>
                        setShownCounts((m) => ({ ...m, [col.key]: searchActive ? PAGE : perLane }))
                      }
                      aria-label={`Collapse ${col.label}`}
                      className="self-start text-xs text-faint transition-colors hover:text-muted"
                    >
                      Collapse
                    </button>
                  )}
                  {paging.nudgeSearch && !searchActive && (
                    <p className="text-[11px] text-faint">Too many to show — use search to narrow.</p>
                  )}
                </div>
              )}
            </div>
          );
        })}
      </div>

      {/* No role/aria-live here any more (S5): the always-mounted sr-only region above
          owns announcing, and leaving them on this one would announce every toast
          twice for anyone whose AT did pick this region up. This stack is now purely
          visual. */}
      {toasts.length > 0 && (
        <div className="pointer-events-none fixed bottom-4 right-4 z-50 flex flex-col gap-2">
          {toasts.map((t) => (
            <div
              key={t.id}
              className="rounded-lg border border-brand/40 bg-surface px-4 py-2 text-sm text-fg shadow-xl"
            >
              {t.text}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
