import { Select } from "./ui";

// The closed set of Agent SDK reasoning-effort levels (PRD #617). Kept UNEXPORTED
// (web/knip.jsonc exports:error would flag an exported-but-not-cross-module const).
// Mirrors the SDK's EffortLevel union, the Go agenttmpl.EffortLevels list, and the
// protocol.ts default_effort type — no shared source, so keep the four in lockstep.
const EFFORT_LEVELS = ["low", "medium", "high", "xhigh", "max"] as const;

// EffortSelect is the per-user reasoning-effort picker: an "Inherit" (empty) option
// plus the five closed levels. The effective value is surfaced through onChange
// ("" = inherit). Unlike ModelSelect there is NO custom free-text mode and NO
// warning surface — the enum is closed, so the control can only ever emit a valid
// value or "".
export function EffortSelect({
  value,
  onChange,
  id,
}: {
  value: string;
  onChange: (effort: string) => void;
  id?: string;
}) {
  return (
    <Select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">Inherit (SDK default: high)</option>
      {EFFORT_LEVELS.map((lvl) => (
        <option key={lvl} value={lvl}>
          {lvl}
        </option>
      ))}
    </Select>
  );
}
