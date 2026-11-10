// How a page opens the app-level command menu (PRD #3875). The AppShell owns the single
// menu instance and publishes `open`; outside the shell (tests that mount a page alone, the
// signed-out shell) the context is null and callers render no trigger.

import { createContext, useContext } from "react";

export type CommandMenuApi = { open: () => void };

export const CommandMenuContext = createContext<CommandMenuApi | null>(null);

export function useCommandMenu(): CommandMenuApi | null {
  return useContext(CommandMenuContext);
}
