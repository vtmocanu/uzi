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
}: {
  run: Pick<
    Run,
    | "anthropic_secret_id"
    | "anthropic_secret_label"
    | "anthropic_select_reason"
    | "anthropic_headroom_pct"
  >;
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
  const { mode, hint, fellBack, deleted } = describeCredential(run);

  // A fallback is the ONE state where the user's configuration and what happened
  // differ: the worker is set to auto and the run spent the default anyway. Amber so
  // it is noticed. Not an error — D7 is that auto never fails a run — but it does mean
  // the pool did no work, which is usually a settings or poller problem the user can
  // fix, and which is invisible if it wears the same neutral chip as an ordinary
  // default.
  const tone = fellBack ? "warning" : "neutral";

  const chip = (
    <Badge
      tone={tone}
      title={
        deleted
          ? `${hint} The credential has since been deleted; the name is the one recorded when the run was claimed.`
          : hint
      }
    >
      token {safe}
      {deleted && " (deleted)"}
      {mode && ` — ${mode}`}
    </Badge>
  );

  // Only a FALLBACK is a link, and the narrowness is the point. PRD #104 M5 already
  // ships the per-token meters and eligibility chips on Settings → Anthropic tokens,
  // so this points at them rather than rebuilding a meter in the run header — but only
  // where the user has something to DO there. On an ordinary `auto` or `pinned` run
  // nothing is wrong and a link would be a dead end dressed as an action; on a deleted
  // credential there is nothing left to look at.
  //
  // The USER'S OWN settings page, deliberately not /admin/rate-limits: that route is
  // admin-only and would 403 for exactly the person reading their own run.
  if (!fellBack) return chip;
  return (
    <Link to="/settings" className="no-underline" aria-label={`${hint} Open your Anthropic tokens.`}>
      {chip}
    </Link>
  );
}
