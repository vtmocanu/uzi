// HarnessBadge — the run harness chip (PRD #1429 M4a, reworked by PRD #1653 D-W5).
//
// EVERY run shows which agent it runs on, Claude included: a small round chip holding
// the provider logo (the Claude mark for "claude", the OpenAI mark for "codex"). PRD
// #1429 M4a left Claude runs unmarked and gave Codex a text badge; PRD #1653 reversed
// that, so a mixed run list says at a glance which agent ran what, and the run header
// matches the list.
//
// The logo is aria-hidden; the chip itself is role="img" named "Claude" / "Codex", and
// its title ("Runs on Claude" / "Runs on Codex") explains it on hover. A null/undefined
// harness renders nothing: Run.harness is NOT NULL server-side, so a missing value is a
// malformed or pre-#1429 payload, and guessing a provider would be a false claim.
//
// No link: the harness fact needs no further drill-in.

import type { Harness } from "../lib/api";
import { ClaudeIcon, OpenAIIcon } from "./icons";

export function HarnessBadge({ harness }: { harness: Harness | null | undefined }) {
  // An unrecognised value (a newer server's harness) also renders nothing, for the same
  // no-false-claim reason as a missing one.
  const name = harness === "claude" ? "Claude" : harness === "codex" ? "Codex" : null;
  if (name == null) return null;
  return (
    <span
      role="img"
      aria-label={name}
      title={`Runs on ${name}`}
      className="inline-flex h-5 w-5 flex-none items-center justify-center rounded-full border border-edge bg-ink text-fg"
    >
      {name === "Claude" ? (
        <ClaudeIcon className="h-[13px] w-[13px]" />
      ) : (
        <OpenAIIcon className="h-[13px] w-[13px]" />
      )}
    </span>
  );
}
