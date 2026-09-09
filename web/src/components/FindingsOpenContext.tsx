import { createContext, useContext } from "react";

// The channel the Findings page uses to keep the nav badge honest (PRD #1183 M4, the
// BLK-BADGE pattern lifted from JudgeTodoContext).
//
// The Findings nav badge and the page's To-triage tab both read the ONE canonical number
// (`stats.todo`, from GET /findings/stats), and neither re-derives it from the rows on
// screen. What was missing before this file was PROPAGATION: AppShell polls the badge on
// `[user, location.pathname]`, and a dismiss changes neither the user nor the pathname (nor
// does a bucket tab, which rewrites the search) — so after a dismiss the nav would read the
// stale count while the page read the fresh one. This publishes the fresh `stats.todo` to the
// badge with no round-trip, exactly as the Judge page does through JudgeTodoContext.
//
// It publishes a SETTER rather than triggering a refetch, because the page already holds a
// fresh canonical `stats` after every load and every mutation reload: refetching would spend a
// round-trip to relearn a number it is holding, and would open a window where the two disagree.
//
// The default is a NO-OP so the Findings page renders correctly outside an AppShell (every
// Findings unit test mounts it standalone). That is a deliberate trade: a missing provider
// degrades to the pre-fix behaviour rather than throwing — which is also precisely why the
// regression test has to mount AppShell and Findings TOGETHER. Mounted alone, each is right.
export const FindingsOpenContext = createContext<(open: number) => void>(() => {});

// useSetFindingsOpen returns the publisher for the canonical open-findings count. Call it with a
// `stats.todo` that came FROM THE SERVER — never with a number tallied off the rows on screen,
// which is the very drift the single canonical source exists to prevent.
export function useSetFindingsOpen(): (open: number) => void {
  return useContext(FindingsOpenContext);
}

// The READ side of the same channel, mirroring JudgeTodoValueContext. NULL means "no provider"
// — rendered outside an AppShell. A displayed 0 would be the claim "you have nothing to
// triage", so a provider-less consumer must render nothing rather than substitute 0.
export const FindingsOpenValueContext = createContext<number | null>(null);

export function useFindingsOpen(): number | null {
  return useContext(FindingsOpenValueContext);
}
