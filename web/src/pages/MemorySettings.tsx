// Settings → Memory: the agent-memory review + purge surface (PRD #90 M6). Inside
// SettingsShell so it sits beside Account & token / Forge / Workers / Access.

import { SettingsShell } from "../components/SettingsShell";
import { Memory } from "../components/Memory";

export function MemorySettings() {
  return (
    <SettingsShell description="Learnings your agents carry across runs, per repo — review the ones you trust and delete the ones you don't.">
      <Memory />
    </SettingsShell>
  );
}
