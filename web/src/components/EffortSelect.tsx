import { Select } from "./ui";

// The closed set of Agent SDK reasoning-effort levels (PRD #617). Kept UNEXPORTED
// (web/knip.jsonc exports:error would flag an exported-but-not-cross-module const).
// Mirrors the agent HARNESS_EFFORTS, the SDK's EffortLevel union, the Go
// agenttmpl.EffortLevels list, and the protocol.ts default_effort type — no shared
// source, so keep them in lockstep. "runtime" (Go EffortRuntime) is NOT a level: it is
// a preference-only token, offered only through runtimeLabel, never sent on a claim
// and never a cross-check pin.
const EFFORT_LEVELS = ["low", "medium", "high", "xhigh", "max"] as const;

// EffortSelect is the per-user reasoning-effort picker: an "Inherit" (empty) option,
// an optional "Runtime default" option (only when runtimeLabel is passed), plus the
// five closed levels. The effective value is surfaced through onChange ("" =
// inherit, "runtime" = the preference-only token). Unlike ModelSelect there is NO
// custom free-text mode and NO warning surface — the enum is closed, so the control
// can only ever emit a valid value, "" or (when offered) "runtime".
export function EffortSelect({
  value,
  onChange,
  id,
  defaultLabel,
  runtimeLabel,
}: {
  value: string;
  onChange: (effort: string) => void;
  id?: string;
  defaultLabel?: string;
  runtimeLabel?: string;
}) {
  return (
    <Select id={id} value={value} onChange={(e) => onChange(e.target.value)}>
      <option value="">{defaultLabel ?? "Inherit (uzi default: medium)"}</option>
      {runtimeLabel !== undefined && <option value="runtime">{runtimeLabel}</option>}
      {EFFORT_LEVELS.map((lvl) => (
        <option key={lvl} value={lvl}>
          {lvl === "medium" ? "medium (uzi default)" : lvl}
        </option>
      ))}
    </Select>
  );
}
