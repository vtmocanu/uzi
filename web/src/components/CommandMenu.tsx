// The command menu (PRD #3875): Cmd/Ctrl K opens a modal palette that lists the viewer's
// pending decisions ("Needs you"), destinations ("Go to") and a few actions, filtered by
// typing. Choosing an item only
// NAVIGATES; nothing is executed in place. Built on Modal for focus move, Tab trap, Escape
// and focus restore. The input is focused from this component's own effect (not autoFocus
// or a ref callback) so Modal's child effect runs first and records the opener.

import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { api, type Repo, type RecoveryCustodyHolds, type RunSummaryItem } from "../lib/api";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";
import { mrRefSymbol } from "../lib/forgeNoun";
import { overviewDecisions, type DecisionItem } from "../lib/overview";
import { stripUnsafeChars } from "../lib/safeText";
import { HELD_WORK_HREF } from "./HeldWorkCard";
import { Modal } from "./Modal";
import { BoardIcon, ChatIcon, SearchIcon } from "./icons";
import { visibleNavItems } from "./navItems";
import { KindIcon } from "./NeedsDecisionCard";

export type CommandGroup = "needs" | "actions" | "goto";

export type CommandItem = {
  id: string;
  group: CommandGroup;
  label: string;
  hint: string;
  // An in-app path; choosing the item navigates here.
  to: string;
  icon: ReactNode;
  title?: string;
};

const GROUP_TITLES: Record<CommandGroup, string> = {
  needs: "Needs you",
  actions: "Actions",
  goto: "Go to",
};

// Without a query the menu reads in the mock's order; with one, ties keep the order the
// sidebar-first reader expects (destinations before actions before decisions).
const DISPLAY_ORDER: CommandGroup[] = ["needs", "actions", "goto"];
const QUERY_ORDER: CommandGroup[] = ["goto", "actions", "needs"];

const ALNUM = /[\p{L}\p{N}]/u;

// 0: label starts with the query; 1: the query starts a word; 2: any other substring;
// null: no match.
function matchRank(label: string, query: string): 0 | 1 | 2 | null {
  const l = label.toLowerCase();
  const first = l.indexOf(query);
  if (first < 0) return null;
  if (first === 0) return 0;
  for (let i = first; i >= 0; i = l.indexOf(query, i + 1)) {
    if (i === 0 || !ALNUM.test(l[i - 1])) return 1;
  }
  return 2;
}

// The visible items for a query, in display order. Pure so the ranking is unit-testable.
export function filterCommandItems(items: CommandItem[], query: string): CommandItem[] {
  const q = query.trim().toLowerCase();
  if (!q) {
    return DISPLAY_ORDER.flatMap((g) => items.filter((it) => it.group === g));
  }
  return items
    .map((item, index) => ({ item, index, rank: matchRank(item.label, q) }))
    .filter((r): r is { item: CommandItem; index: number; rank: 0 | 1 | 2 } => r.rank !== null)
    .sort(
      (a, b) =>
        a.rank - b.rank ||
        QUERY_ORDER.indexOf(a.item.group) - QUERY_ORDER.indexOf(b.item.group) ||
        a.index - b.index,
    )
    .map((r) => r.item);
}

const RUN_DECISION_VIEW = {
  plan: { hint: "Plan review", icon: <KindIcon kind="plan" /> },
  question: { hint: "Question", icon: <KindIcon kind="question" /> },
  park: { hint: "Parked", icon: <KindIcon kind="park" /> },
  pr: { hint: "Pull request", icon: <KindIcon kind="pr" /> },
} as const;

// The Needs you items for a set of decisions (overviewDecisions' output). Every target
// comes from the loaded run id or the fixed held-work link, never from label text.
export function decisionCommandItems(decisions: ReadonlyArray<DecisionItem>): CommandItem[] {
  return decisions.map((d): CommandItem => {
    if (d.kind === "held") {
      return {
        id: "needs:held",
        group: "needs",
        label: d.verb,
        hint: d.count > 1 ? `Custody · ${d.count}` : "Custody",
        to: HELD_WORK_HREF,
        icon: <KindIcon kind="held" />,
        title: d.label === null ? undefined : stripUnsafeChars(d.label),
      };
    }
    const run = d.run;
    const view = RUN_DECISION_VIEW[d.kind];
    let label: string;
    if (d.kind === "pr" && run.mr_iid != null) {
      // The card's reference: the MR number with the forge's symbol, not the issue's.
      label = `${d.verb} ${mrRefSymbol(run.forge_type)}${run.mr_iid}`;
    } else if (run.issue_iid != null) {
      label =
        d.kind === "plan" || d.kind === "pr" ? `${d.verb} for #${run.issue_iid}` : `${d.verb} #${run.issue_iid}`;
    } else {
      const name = stripUnsafeChars(run.title || run.issue_title || "");
      label = name ? `${d.verb}: ${name}` : d.verb;
    }
    return {
      id: `needs:${d.kind}:${run.id}`,
      group: "needs",
      label,
      hint: view.hint,
      to: `/runs/${encodeURIComponent(run.id)}`,
      icon: view.icon,
    };
  });
}

type RepoState = { status: "loading" } | { status: "error" } | { status: "ok"; repos: Repo[] };
type DecisionState = { status: "loading" } | { status: "error" } | { status: "ok"; items: CommandItem[] };

export function CommandMenu({
  onClose,
  isAdmin,
  extraItems,
}: {
  onClose: () => void;
  isAdmin: boolean;
  // A seam for additional items merged with the built-in groups. Its only caller today is
  // the test; the render-time strip covers whatever text it carries.
  extraItems?: CommandItem[];
}) {
  const navigate = useNavigate();
  const demo = useDemoMode();
  const uid = useId();
  const inputId = `${uid}-input`;
  const listId = `${uid}-list`;
  const optionId = (i: number) => `${uid}-opt-${i}`;
  const inputRef = useRef<HTMLInputElement>(null);
  const [query, setQuery] = useState("");
  // A null highlight follows the list's first item (so Enter runs the first match, even when
  // decisions arrive above it); an explicit highlight (arrows or hover) is kept by item id,
  // so rows inserted above it do not shift it to a different command.
  const [activeId, setActiveId] = useState<string | null>(null);
  const [repoState, setRepoState] = useState<RepoState>({ status: "loading" });
  const [decisionState, setDecisionState] = useState<DecisionState>({ status: "loading" });

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  // The viewer's decisions are fetched once when the menu opens, like the repos: no poll,
  // no subscription. Either request failing hides the whole group (never a partial list);
  // a response that lands after unmount is dropped.
  useEffect(() => {
    let alive = true;
    void Promise.allSettled([api.listRuns({ view: "summary" }), api.getRecoveryHolds()]).then(
      ([runsResult, holdsResult]) => {
        if (!alive) return; // defensive: setState after unmount is a no-op in React 19
        if (runsResult.status !== "fulfilled" || holdsResult.status !== "fulfilled") {
          setDecisionState({ status: "error" });
          return;
        }
        const runs: RunSummaryItem[] = runsResult.value.runs;
        const holds: RecoveryCustodyHolds = holdsResult.value;
        setDecisionState({ status: "ok", items: decisionCommandItems(overviewDecisions(runs, holds)) });
      },
    );
    return () => {
      alive = false;
    };
  }, []);

  // Repos are fetched once when the menu opens: no subscription, no poll. A response that
  // lands after unmount is dropped.
  useEffect(() => {
    let alive = true;
    api
      .listRepos()
      .then(({ repos }) => {
        if (alive) setRepoState({ status: "ok", repos });
      })
      .catch(() => {
        if (alive) setRepoState({ status: "error" });
      });
    return () => {
      alive = false;
    };
  }, []);

  const items = useMemo<CommandItem[]>(() => {
    const actions: CommandItem[] = [
      { id: "action:chat", group: "actions", label: "Start a chat", hint: "Chat", to: "/chat", icon: <ChatIcon /> },
    ];
    if (repoState.status === "ok") {
      for (const r of repoState.repos) {
        actions.push({
          id: `action:board:${r.id}`,
          group: "actions",
          // Repo paths are forge-supplied: masked in demo mode, then stripped of bidi and
          // control characters. The link comes from the loaded repo id, never the label.
          label: `Open board: ${stripUnsafeChars(maskRepoPath(r.path_with_namespace, demo))}`,
          hint: "Board",
          to: `/repos/${encodeURIComponent(r.id)}/board`,
          icon: <BoardIcon />,
        });
      }
    }
    const goto: CommandItem[] = visibleNavItems(isAdmin).map((n) => ({
      id: `goto:${n.key}`,
      group: "goto",
      label: n.label,
      hint: n.hint,
      to: n.to,
      icon: n.icon,
    }));
    const needs = decisionState.status === "ok" ? decisionState.items : [];
    return [...needs, ...(extraItems ?? []), ...actions, ...goto];
  }, [repoState, decisionState, demo, isAdmin, extraItems]);

  const visible = useMemo(() => filterCommandItems(items, query), [items, query]);
  const activeIndex = visible.length === 0 ? -1 : Math.max(0, visible.findIndex((it) => it.id === activeId));

  // Re-runs on a new result list too: filtering can keep index 0 active while the list keeps a
  // stale scroll offset, and Enter must never run an option the user cannot see.
  useEffect(() => {
    if (activeIndex < 0) return;
    document.getElementById(`${uid}-opt-${activeIndex}`)?.scrollIntoView?.({ block: "nearest" });
  }, [activeIndex, uid, visible]);

  const run = (item: CommandItem) => {
    navigate(item.to);
    onClose();
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLInputElement>) => {
    if (e.nativeEvent.isComposing) return;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      if (visible.length === 0) return;
      const step = e.key === "ArrowDown" ? 1 : -1;
      setActiveId(visible[(activeIndex + step + visible.length) % visible.length].id);
    } else if (e.key === "Enter") {
      e.preventDefault();
      const item = visible[activeIndex];
      if (item) run(item);
    }
  };

  const searching = query.trim() !== "";
  const boardsUnavailable = repoState.status === "error";
  const decisionsUnavailable = decisionState.status === "error";

  const renderOption = (item: CommandItem, i: number) => (
    <div
      key={item.id}
      id={optionId(i)}
      role="option"
      aria-selected={i === activeIndex}
      // Re-stripped here whatever the source, so an unsafe title or label handed in through
      // extraItems cannot reach the DOM.
      title={item.title === undefined ? undefined : stripUnsafeChars(item.title)}
      onClick={() => run(item)}
      onMouseMove={() => setActiveId(item.id)}
      className={
        "flex min-h-10 cursor-pointer items-center gap-2.5 rounded-lg px-3 py-2 text-sm " +
        // bg-raised on bg-surface is too faint alone (WCAG 1.4.11): the brand rail is the
        // second cue.
        (i === activeIndex ? "bg-raised text-fg shadow-[inset_2px_0_0_rgb(var(--brand))]" : "text-fg")
      }
    >
      <span aria-hidden="true" className="text-base text-faint">
        {item.icon}
      </span>
      <span className="min-w-0 truncate">{stripUnsafeChars(item.label)}</span>
      {item.hint && <span className="ml-auto shrink-0 text-xs text-faint">{item.hint}</span>}
    </div>
  );

  // Only options (inside role="group" while browsing) live in the listbox; the empty and
  // error notes sit beside it so the listbox holds nothing but options and groups.
  let body: ReactNode;
  if (searching) {
    body = visible.map(renderOption);
  } else {
    body = DISPLAY_ORDER.map((g) => {
      const rows = visible.map((item, i) => ({ item, i })).filter((r) => r.item.group === g);
      if (rows.length === 0) return null;
      return (
        // aria-label, not aria-labelledby: the visible heading is upper-cased by CSS and some
        // readers then spell it out ("ACTIONS").
        <div key={g} role="group" aria-label={GROUP_TITLES[g]}>
          <div
            aria-hidden="true"
            className="px-3 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wider text-faint"
          >
            {GROUP_TITLES[g]}
          </div>
          {rows.map((r) => renderOption(r.item, r.i))}
        </div>
      );
    });
  }
  // Announced politely as the query narrows the list; empty while browsing.
  const notes = [boardsUnavailable && "Boards unavailable", decisionsUnavailable && "Decisions unavailable"]
    .filter((n): n is string => !!n)
    .join(". ");
  const count =
    visible.length === 0 ? "No matches" : `${visible.length} ${visible.length === 1 ? "result" : "results"}`;
  const announcement = !searching ? "" : notes ? `${count}. ${notes}` : count;

  return (
    <Modal
      label="Command menu"
      onClose={onClose}
      className="fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-black/50 px-4 pb-4 pt-[12vh] outline-hidden"
    >
      <div className="frame w-full max-w-[560px] overflow-hidden rounded-xl border border-edge-strong bg-surface text-fg shadow-2xl">
        <div className="flex items-center gap-2.5 border-b border-edge px-4 text-faint">
          <span aria-hidden="true" className="text-lg">
            <SearchIcon />
          </span>
          <label htmlFor={inputId} className="sr-only">
            Command
          </label>
          <input
            ref={inputRef}
            id={inputId}
            type="text"
            role="combobox"
            aria-controls={listId}
            aria-expanded={visible.length > 0}
            aria-autocomplete="list"
            aria-activedescendant={activeIndex >= 0 ? optionId(activeIndex) : undefined}
            autoComplete="off"
            spellCheck={false}
            placeholder="Type a command or search"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setActiveId(null);
            }}
            onKeyDown={onKeyDown}
            // The global input:focus-visible ring would draw a clipped box here; the active
            // option and the modal already show where the keyboard is.
            className="h-12 min-w-0 flex-1 bg-transparent text-[15px] font-medium text-fg outline-hidden placeholder:text-faint focus-visible:ring-0 focus-visible:ring-offset-0"
          />
          <kbd
            aria-hidden="true"
            className="rounded border border-edge bg-surface px-1.5 font-mono text-xs text-muted"
          >
            Esc
          </kbd>
        </div>
        <div
          id={listId}
          role="listbox"
          aria-label="Commands"
          className={"max-h-[360px] overflow-y-auto " + (visible.length > 0 ? "p-2" : "")}
        >
          {body}
        </div>
        {visible.length === 0 && (
          <div aria-hidden="true" className="px-3 py-6 text-center text-sm text-muted">
            No matches
          </div>
        )}
        {boardsUnavailable && <div className="px-5 pb-3 text-xs text-faint">Boards unavailable</div>}
        {decisionsUnavailable && <div className="px-5 pb-3 text-xs text-faint">Decisions unavailable</div>}
        <div role="status" aria-live="polite" className="sr-only">
          {announcement}
        </div>
      </div>
    </Modal>
  );
}
