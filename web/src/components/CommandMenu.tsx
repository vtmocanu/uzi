// The command menu (PRD #3875): Cmd/Ctrl K opens a modal palette that lists the viewer's
// destinations ("Go to") and a few actions, filtered by typing. Choosing an item only
// NAVIGATES; nothing is executed in place. Built on Modal for focus move, Tab trap, Escape
// and focus restore. The input is focused from this component's own effect (not autoFocus
// or a ref callback) so Modal's child effect runs first and records the opener.

import { useEffect, useId, useMemo, useRef, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { api, type Repo } from "../lib/api";
import { useDemoMode } from "../lib/demoMode";
import { maskRepoPath } from "../lib/demoMask";
import { stripUnsafeChars } from "../lib/safeText";
import { Modal } from "./Modal";
import { BoardIcon, ChatIcon, SearchIcon } from "./icons";
import { visibleNavItems } from "./navItems";

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

type RepoState = { status: "loading" } | { status: "error" } | { status: "ok"; repos: Repo[] };

export function CommandMenu({
  onClose,
  isAdmin,
  extraItems,
}: {
  onClose: () => void;
  isAdmin: boolean;
  // Further items (the Needs you decisions) merged with the built-in groups.
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
  // The active option is tracked by item id, so rows inserted above it (the repos arriving)
  // do not shift the highlight to a different command. null: the first visible item.
  const [activeId, setActiveId] = useState<string | null>(null);
  const [repoState, setRepoState] = useState<RepoState>({ status: "loading" });

  useEffect(() => {
    inputRef.current?.focus();
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
    return [...(extraItems ?? []), ...actions, ...goto];
  }, [repoState, demo, isAdmin, extraItems]);

  const visible = useMemo(() => filterCommandItems(items, query), [items, query]);
  const activeIndex = visible.length === 0 ? -1 : Math.max(0, visible.findIndex((it) => it.id === activeId));

  useEffect(() => {
    if (activeIndex < 0) return;
    document.getElementById(`${uid}-opt-${activeIndex}`)?.scrollIntoView?.({ block: "nearest" });
  }, [activeIndex, uid]);

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

  const renderOption = (item: CommandItem, i: number) => (
    <div
      key={item.id}
      id={optionId(i)}
      role="option"
      aria-selected={i === activeIndex}
      title={item.title}
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
      <span className="min-w-0 truncate">{item.label}</span>
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
      const titleId = `${uid}-group-${g}`;
      return (
        <div key={g} role="group" aria-labelledby={titleId}>
          <div
            id={titleId}
            role="presentation"
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
  const announcement = !searching
    ? ""
    : visible.length === 0
      ? "No matches"
      : `${visible.length} ${visible.length === 1 ? "result" : "results"}`;

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
            aria-expanded="true"
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
        <div id={listId} role="listbox" aria-label="Commands" className="max-h-[360px] overflow-y-auto p-2">
          {body}
        </div>
        {visible.length === 0 && (
          <div aria-hidden="true" className="px-3 py-6 text-center text-sm text-muted">
            No matches
          </div>
        )}
        {boardsUnavailable && <div className="px-5 pb-3 text-xs text-faint">Boards unavailable</div>}
        <div role="status" aria-live="polite" className="sr-only">
          {announcement}
        </div>
      </div>
    </Modal>
  );
}
