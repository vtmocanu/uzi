// HarnessBadge — provider logo for runs (PRD #1429 M4a, #1653 D-W5, issue #1681).
//
// Claude and Codex both have a logo. The default is a small round chip; the bare
// variant is a 28px square logo beside a run-list title, including vault-waiting rows.
//
// The SVG is aria-hidden; its wrapper is role="img" named "Claude" / "Codex", and
// its title ("Runs on Claude" / "Runs on Codex") explains it on hover. A null/undefined
// harness renders nothing: Run.harness is NOT NULL server-side, so a missing value is a
// malformed or pre-#1429 payload, and guessing a provider would be a false claim.
//
// No link: the harness fact needs no further drill-in.

import type { Harness } from "../lib/api";
import { ClaudeIcon, OpenAIIcon } from "./icons";

export function HarnessBadge({ harness, variant = "chip" }: { harness: Harness | null | undefined; variant?: "chip" | "bare" }) {
  // An unrecognised value (a newer server's harness) also renders nothing, for the same
  // no-false-claim reason as a missing one.
  const name = harness === "claude" ? "Claude" : harness === "codex" ? "Codex" : null;
  if (name == null) return null;
  return (
    <span
      role="img"
      aria-label={name}
      title={`Runs on ${name}`}
      className={variant === "bare"
        ? "inline-flex h-7 w-7 flex-none items-center justify-center text-fg"
        : "inline-flex h-5 w-5 flex-none items-center justify-center rounded-full border border-edge bg-ink text-fg"}
    >
      {name === "Claude" ? (
        <ClaudeIcon className={variant === "bare" ? "h-[22px] w-[22px]" : "h-[13px] w-[13px]"} />
      ) : (
        <OpenAIIcon className={variant === "bare" ? "h-[22px] w-[22px]" : "h-[13px] w-[13px]"} />
      )}
    </span>
  );
}
