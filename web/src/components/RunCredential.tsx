// RunCredential renders WHICH Anthropic credential a run's claim spent and WHY
// (PRD #111 M1, extended by M5) — "which account paid for this run, and why that
// one?", which the usage totals alone could never give.
//
// It keys off the LABEL, not the id, and that is the whole reason it is a component
// rather than three lines inline. The fields go null INDEPENDENTLY: the label is a
// snapshot taken at claim time, while the id is a live foreign key that the server
// nulls when the token is deleted. So `id === null` with a label is the normal shape
// of a historical run whose credential has since been removed, and rendering it is
// the point — the run still names the account it billed. Gating on the id would make
// exactly that case disappear.
//
// Renders nothing when there is no label: a run claimed before this landed, and a
// run not yet claimed, have nothing truthful to say and must not show a placeholder.
//
// M5 adds the MODE (D20), and the reason it is not decoration: an auto pick and a
// default fallback can name the SAME token, so the label answers "which account" and
// leaves "why that account" open — and PRD #104's compatibility path creates a row
// labelled literally `default`, so the label is not even a reliable hint. The words
// come from lib/runCredential, which renders the server's own record and derives
// nothing from the other fields.
//
// The label is USER-AUTHORED text and is rendered as plain JSX (React escapes it),
// never through <Markdown> and never interpolated into a URL.

import { Link } from "react-router-dom";

import type { Run } from "../lib/api";
import { describeCredential } from "../lib/runCredential";
import { sanitizeLabel } from "../lib/sanitizeLabel";
import { Badge } from "./ui";

export function RunCredential({
  run,
  variant = "full",
  linkable = true,
}: {
  run: Pick<
    Run,
    | "anthropic_secret_id"
    | "anthropic_secret_label"
    | "anthropic_select_reason"
    | "anthropic_headroom_pct"
  >;
  // variant "compact" (PRD #295) is the Runs-list rendering: label + a tone dot
  // (non-neutral only) + `(deleted)`, with mode/hint moved into the title so the
  // row stays scannable. "full" is the run-detail sentence chip and stays the
  // default so RunView/AgentDetail/Agents are untouched.
  variant?: "full" | "compact";
  // linkable=false suppresses the /settings link even in a non-neutral state.
  // The admin factory list (PRD #295 Decision 2) shows ANOTHER user's credential,
  // whose fix lives on that user's settings, not the viewing admin's — so the
  // badge renders but never links. Default true preserves the personal behaviour.
  linkable?: boolean;
}) {
  const label = run.anthropic_secret_label;
  if (!label) return null;
  // The label is user-authored and reaches a renderer without necessarily having
  // passed the server validator — see lib/sanitizeLabel for the three routes. React
  // escaping does not touch a bidi override.
  const safe = sanitizeLabel(label);
  // web-ux F8 is `deleted`: a run whose credential was DELETED is otherwise
  // indistinguishable from one whose credential still exists. Saying so is the
  // difference between "go look at this token" and "this token is gone".
  const { mode, hint, tone, linked, deleted } = describeCredential(run);
  const hintId = `run-credential-hint-${run.anthropic_secret_id ?? "gone"}`;

  const chip =
    variant === "compact" ? (
      // PRD #295: the Runs-list rendering. The tone dot carries the "worth a look"
      // signal the full chip carries with a link, the label answers "which account",
      // and the mode + hint move into the title so the row does not grow a
      // sentence-length pill. The sr-only hint span and aria-describedby are the same
      // web-ux F21 pattern as the full chip — the explanation is DESCRIBED, never the
      // accessible NAME.
      <Badge
        tone={tone === "warning" ? "warning" : tone === "info" ? "info" : "neutral"}
        // dot only where the state is non-neutral, matching the full chip's
        // "link iff non-neutral" rule: a calm auto/pinned pick stays quiet.
        dot={tone !== "neutral"}
        aria-describedby={hintId}
        title={
          deleted
            ? `${mode ? mode + " — " : ""}${hint} The credential has since been deleted; the name is the one recorded when the run was claimed.`
            : mode
              ? `${mode} — ${hint}`
              : hint
        }
      >
        {safe}
        {deleted && " (deleted)"}
        <span id={hintId} className="sr-only">
          {hint}
        </span>
      </Badge>
    ) : (
      <Badge
        tone={tone === "warning" ? "warning" : tone === "info" ? "info" : "neutral"}
        // web-ux F20: this badge carries sentence-length reasons —
        // `default (auto: the chosen token would not open)` measured 389px against a
        // 375px viewport and overflowed the document. The em dash is not the cause; a
        // pill that cannot wrap is.
        wrap
        // web-ux F21: the explanation is DESCRIBED, never the accessible NAME. An
        // aria-label here would replace the visible text, so a voice-control user saying
        // "click token default" would match nothing — WCAG 2.5.3 Label in Name. The
        // sr-only span below is the same pattern this file's sibling uses for D6_HINT.
        aria-describedby={hintId}
        title={
          deleted
            ? `${hint} The credential has since been deleted; the name is the one recorded when the run was claimed.`
            : hint
        }
      >
        {/* web-ux F19, second attempt, and the first one is why this is QUOTES rather
          than weight. `token default — default` is the exact input D20 named as its
          motivating case: the same word twice, first a NAME and then a MODE, with
          nothing marking which is which. Bolding the label was measured at 3x zoom and
          the one weight step (600 vs 500) at 11px in muted grey is not perceptible at
          actual size — the markup changed and the meaning did not.

          Quotes do the work at any size and any contrast, and they are unambiguous
          about WHICH token is a name. The weight stays because it costs nothing and
          helps where it is visible. */}
        token “<span className="font-semibold">{safe}</span>”
        {deleted && " (deleted)"}
        {mode && ` — ${mode}`}
        <span id={hintId} className="sr-only">
          {hint}
        </span>
      </Badge>
    );

  // Only a non-neutral state is a link, and it is ONE rule so the two cannot drift:
  // link iff the tone is not neutral. PRD #104 M5 already ships the per-token meters
  // and eligibility chips on Settings → Anthropic tokens, so this points at them
  // rather than rebuilding a meter in the run header — but only where the user has
  // something to DO there. On an ordinary `auto` or `pinned` run nothing is wrong and
  // a link is a dead end dressed as an action; on a deleted credential there is
  // nothing left to look at.
  //
  // `linkable` additionally strips the link on the admin factory list (PRD #295 D2),
  // where the credential belongs to another user and this link would point the admin
  // at their OWN settings — a dead end. Default true keeps the personal behaviour.
  //
  // The USER'S OWN settings page, deliberately not /admin/rate-limits: that route is
  // admin-only and would 403 for exactly the person reading their own run.
  return linked && linkable ? (
    <Link to="/settings" className="no-underline">
      {chip}
    </Link>
  ) : (
    chip
  );
}
