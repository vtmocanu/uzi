// WeeklyLimitBanner renders the server's weekly-limit block (PRD #4123) beside the run's
// credential chip: which credential the run is parked on at its weekly limit (from M2, also one near it), why uzi keeps the run
// on it, what the run will do (wait or fail), and the commands that would change that.
//
// Render-only by design. The server computes reason, wait and fixes once for every
// surface (weeklylimit.ForRun); nothing here re-derives them, so the web cannot promise a
// resume the run will not get. The wait sentence only says "resumes" for `waits`.
//
// Labels are OWNER text: rendered as React text (escaped) after sanitizeLabel strips Cf
// format characters, never as markup. Commands are rendered byte-exact (they are what the
// owner pastes into a shell; the server already shell-quotes them and withholds any
// command whose label is not command-safe, flagging command_unavailable "label").
//
// The block rides a run parked on its weekly limit (M1) and, from M2, any non-terminal run whose
// credential is near or at it. `parked` tells the two apart so the wait line is a fact for a
// parked run ("It waits for the reset") and a conditional for one still working ("If it reaches
// the limit, it waits"). The headline reads the same either way.
//
// The one visual device is the meter under the headline: a thin track filled to the weekly use, with a tick
// at the effective limit (the reserve when set, else 100). It is aria-hidden; the sentence
// beside it carries the same numbers.

import { useState } from "react";
import { Link } from "react-router-dom";

import type { WeeklyLimit, WeeklyLimitFix } from "../lib/api";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Button, cx } from "./ui";

function quoted(label: string): string {
  return `“${sanitizeLabel(label)}”`;
}

function resetText(resetsAt: string | null): string | null {
  if (!resetsAt) return null;
  const ms = Date.parse(resetsAt);
  if (Number.isNaN(ms)) return null;
  return new Date(ms).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function pctText(n: number): string {
  return `${Math.round(n)}%`;
}

// Mirrors apitypes.WeeklyLimitDTO.ReasonText so web, CLI and Slack say the same thing.
function reasonSentence(block: WeeklyLimit): string {
  const alt = block.alternative_label ? quoted(block.alternative_label) : null;
  switch (block.reason) {
    case "switch_pending":
      return "A credential switch is already in progress for this run.";
    case "can_switch":
      return alt
        ? `This run can move to ${alt} on its next claim.`
        : "This run can move to another token on its next claim.";
    case "selection_unchanged":
      return "Auto-select still prefers this credential, even though another pooled one is usable.";
    case "headroom_outside_pool":
      return alt
        ? `${alt} has weekly headroom but is not in the auto-select pool.`
        : "Another token has weekly headroom but is not in the auto-select pool.";
    case "pool_exhausted":
      return "Every other pooled token is at its weekly limit or held by the reserve.";
    case "no_alternative":
      return "There is no other usable token to switch to.";
    case "account_bound":
      return "This Codex run stays on the account it was created with.";
    case "not_auto":
      return "This run does not auto-select tokens.";
    case "pinned":
      return `This run is pinned to ${quoted(block.credential_label)}.`;
    default:
      return "It is not known whether this run can switch.";
  }
}

// Whether the run is stopped on its weekly limit right now: a seven_day limit_wait or a
// weekly-reserve pause (the server's M1 park cases). Anything else carrying the block is near
// or at the limit while still running, queued or waiting on something else.
export function weeklyLimitParked(run: { status: string; rate_limit_type?: string | null; hold_reason?: string | null }): boolean {
  return (
    (run.status === "limit_wait" && run.rate_limit_type === "seven_day") ||
    (run.status === "paused" && run.hold_reason === "weekly_reserve")
  );
}

// The headline: the credential, its weekly use against the effective limit, and the reset.
// With a reserve the limit is the owner's threshold, so it is named as theirs.
function headline(block: WeeklyLimit, reset: string | null): string {
  const label = quoted(block.credential_label);
  const when = reset ? `(resets ${reset})` : "(reset time unknown)";
  if (block.pct === null) return `Weekly usage of ${label} is unknown ${when}.`;
  const pct = pctText(block.pct);
  const limit = pctText(block.effective_limit);
  const reserve = block.effective_limit < 100;
  if (block.pct >= block.effective_limit) {
    return reserve
      ? `${label} is at ${pct} of its weekly allowance, past your ${limit} reserve ${when}.`
      : `${label} has reached its weekly limit ${when}.`;
  }
  return reserve
    ? `${label} is at ${pct} of its weekly allowance; your reserve holds runs at ${limit} ${when}.`
    : `${label} is at ${pct} of its weekly limit ${when}.`;
}

// Only `waits` mentions a resume: `fails` and `unknown` must never read as a promise. A run
// that is not parked has not hit the limit yet, so its line is conditional.
function waitSentence(block: WeeklyLimit, reset: string | null, parked: boolean): string {
  if (!parked) {
    switch (block.wait) {
      case "waits":
        return reset
          ? `If it reaches the limit, it waits for the reset on ${reset}, then resumes.`
          : "If it reaches the limit, it waits for the weekly reset, then resumes.";
      case "fails":
        return "If it reaches the limit, it fails instead of waiting for the reset.";
      case "reserve_pause":
        return "At your reserve it pauses after its current milestone, until a fresh reading shows usage below the reserve or you resume it.";
      default:
        return "Whether it waits for the reset at the limit is not known.";
    }
  }
  switch (block.wait) {
    case "waits":
      return reset
        ? `It waits for the reset on ${reset}, then resumes.`
        : "It waits for the weekly reset, then resumes.";
    case "fails":
      return "It fails at the limit instead of waiting for the reset.";
    case "reserve_pause":
      return "It stays paused until a fresh reading shows usage below your reserve, or until you resume it.";
    default:
      return "Whether it waits for the reset is not known.";
  }
}

function fixDescription(fix: WeeklyLimitFix): string {
  const target = fix.target_label ? quoted(fix.target_label) : null;
  switch (fix.kind) {
    case "set_token":
      return target ? `Switch this run to ${target}` : "Switch this run to another token";
    case "auto_select":
      return "Turn on auto-select";
    case "pool_on":
      return target ? `Add ${target} to the auto-select pool` : "Add the token to the auto-select pool";
    case "resume_reserve":
      return "Resume this run";
    case "edit_reserve":
      return "Change your weekly reserve";
    default:
      return "Change the token setup";
  }
}

function CopyableCommand({ command }: { command: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command);
      setCopied(true);
    } catch {
      // Clipboard can be unavailable (insecure context); the command stays visible to copy by hand.
    }
  };
  return (
    <div className="mt-1 flex flex-wrap items-center gap-2">
      <code
        data-weekly-command=""
        className="console min-w-0 flex-1 whitespace-pre-wrap rounded-md [overflow-wrap:anywhere] border border-edge bg-ink px-2 py-1 font-mono text-xs text-fg"
      >
        {command}
      </code>
      <Button variant="secondary" size="sm" onClick={() => void copy()}>
        {copied ? "Copied" : "Copy command"}
      </Button>
      <span role="status" aria-live="polite" className="sr-only">
        {copied ? "Command copied" : ""}
      </span>
    </div>
  );
}

function FixItem({ fix }: { fix: WeeklyLimitFix }) {
  return (
    <li className="min-w-0">
      <span className="text-xs text-fg">{fixDescription(fix)}</span>
      {fix.command ? (
        <CopyableCommand command={fix.command} />
      ) : fix.command_unavailable === "label" ? (
        <p className="mt-0.5 text-xs text-muted">
          <Link to="/settings" className="font-medium text-brand hover:text-brand-hover">
            Rename this token in Settings
          </Link>{" "}
          to get a copyable command.
        </p>
      ) : fix.kind === "edit_reserve" ? (
        <p className="mt-0.5 text-xs text-muted">
          <Link to="/settings/run-defaults" className="font-medium text-brand hover:text-brand-hover">
            Open Run defaults
          </Link>{" "}
          to change the threshold.
        </p>
      ) : null}
    </li>
  );
}

function Meter({ pct, limit, warning }: { pct: number | null; limit: number; warning: boolean }) {
  const clamp = (n: number) => Math.max(0, Math.min(100, n));
  return (
    <span
      aria-hidden="true"
      data-weekly-meter=""
      className="relative block h-1.5 w-full max-w-[16rem] rounded-full bg-edge"
    >
      {pct !== null && (
        <span
          className={cx("absolute inset-y-0 left-0 rounded-full", warning ? "bg-warn" : "bg-muted")}
          style={{ width: `${clamp(pct)}%` }}
        />
      )}
      <span
        className="absolute -inset-y-1 w-0.5 rounded-full bg-fg"
        style={{ left: `calc(${clamp(limit)}% - 1px)` }}
      />
    </span>
  );
}

export function WeeklyLimitBanner({
  block,
  parked,
}: {
  block: WeeklyLimit | undefined | null;
  /** The run is stopped on its weekly limit now (weeklyLimitParked). */
  parked: boolean;
}) {
  if (!block) return null;
  const warning = block.tier === "warning";
  const reset = resetText(block.resets_at);

  return (
    <section
      aria-label="Weekly usage limit"
      data-weekly-tier={block.tier}
      className={cx(
        "mt-3 max-w-3xl min-w-0 space-y-1.5 rounded-lg border px-3 py-2.5",
        warning ? "border-warn/40 bg-warn/10" : "border-edge bg-surface",
      )}
    >
      <p className={cx("text-sm font-medium", warning ? "text-warn" : "text-fg")}>
        {headline(block, reset)}
      </p>
      <Meter pct={block.pct} limit={block.effective_limit} warning={warning} />
      <p className="text-xs text-muted">
        {reasonSentence(block)} {waitSentence(block, reset, parked)}
      </p>
      {block.fix.length > 0 && (
        <div className="pt-1">
          <p className="text-xs font-medium text-fg">To change this run:</p>
          <ul className="mt-1 space-y-2">
            {block.fix.map((f, i) => <FixItem key={`fix-${i}`} fix={f} />)}
          </ul>
        </div>
      )}
      {block.future_runs.length > 0 && (
        <div className="pt-1">
          <p className="text-xs font-medium text-fg">For runs you start later:</p>
          <ul className="mt-1 space-y-2">
            {block.future_runs.map((f, i) => <FixItem key={`future-${i}`} fix={f} />)}
          </ul>
        </div>
      )}
    </section>
  );
}
