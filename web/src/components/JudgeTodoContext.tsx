import { createContext, useContext } from "react";

// The channel the Judge page uses to keep the nav badge honest (PRD #98 review BLK-BADGE).
//
// The badge and the page's To-triage tab both read the ONE canonical number
// (`triage.todo`, from the /me/judge/stats query) and neither re-derives it from the groups
// on screen — that part was right and is mutation-defended on both sides. What was missing
// was PROPAGATION: AppShell polls on `[user, location.pathname]`, and a disposition changes
// neither, so after a dispose the nav read 3 while the tab read 0. Switching tabs does not
// help either — setBucket/clearRunAnchor change the SEARCH, not the pathname.
//
// It publishes a SETTER rather than triggering a refetch, because the disposition response
// already carries a fresh canonical `triage`: refetching would spend a round-trip to learn a
// number the page is holding, and would open a window where the two disagree again.
//
// The default is a NO-OP so the Judge page renders correctly outside an AppShell (every
// Judge unit test mounts it standalone). That is a deliberate trade: a missing provider
// degrades to the pre-fix behaviour rather than throwing — which is also precisely why the
// regression test has to mount AppShell and Judge TOGETHER. Mounted alone, each is right.
export const JudgeTodoContext = createContext<(todo: number) => void>(() => {});

// useSetJudgeTodo returns the publisher for the canonical to-triage count. Call it with a
// `triage.todo` that came FROM THE SERVER — never with a number tallied off the rows on
// screen, which is the very drift the single canonical source exists to prevent.
export function useSetJudgeTodo(): (todo: number) => void {
  return useContext(JudgeTodoContext);
}
