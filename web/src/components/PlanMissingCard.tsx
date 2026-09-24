import { stripUnsafeChars } from "../lib/safeText";

// PlanMissingCard renders the worker's `plan_missing` status (issue #1593): a gated
// planning turn ended with prose only, even after one corrective nudge, so there is no
// plan to review and no structured question. On an attended run the worker follows this
// with its own "Plan missing" question (the QuestionPanel above the feed); on an
// unattended run the run fails with fail_origin plan_missing. Either way this card is
// the place the reader sees WHAT the lead said instead of planning.
//
// TRUST: `text` is a fixed worker notice, but `lead_final_message` is UNTRUSTED model
// output (it can carry prompt-injection prose, markdown, HTML, links). Both render as
// escaped, INERT JSX text children, never through <Markdown> (so a link is not
// clickable and `**x**` is not bold), and each passes through stripUnsafeChars first,
// because React's escaping does not touch a bidi override or a zero-width character
// (issue #124). No attribute (title/href/style/aria-label) carries either string.
//
// The worker already bounds the message and scrubs secrets from it; the client-side
// clamp below is defence in depth for a row written by an older or misbehaving worker.
export const LEAD_MESSAGE_DISPLAY_MAX = 4000;

const FALLBACK_NOTICE = "Plan missing: the planning turn ended without a plan.";

// clampCodePoints cuts on code points, not UTF-16 units, so the clamp never leaves half
// of a surrogate pair (an astral emoji) dangling at the cut.
function clampCodePoints(s: string, max: number): string {
  const points = Array.from(s);
  return points.length > max ? `${points.slice(0, max).join("")}…` : s;
}

export function PlanMissingCard({ text, leadFinalMessage }: { text?: string; leadFinalMessage?: string }) {
  const notice = stripUnsafeChars(text ?? "").trim() || FALLBACK_NOTICE;
  const lead = leadFinalMessage ? clampCodePoints(stripUnsafeChars(leadFinalMessage), LEAD_MESSAGE_DISPLAY_MAX) : "";

  return (
    <div data-testid="plan-missing-card" className="overflow-hidden rounded-xl border border-warn/40 bg-warn/[0.06]">
      <div className="border-b border-warn/20 bg-warn/10 px-3 py-2 text-xs font-semibold text-warn">
        Plan missing
      </div>
      <div className="space-y-3 px-3 py-3">
        <p className="text-sm text-fg">{notice}</p>
        {lead !== "" && (
          <div className="space-y-1">
            <p className="text-xs font-medium text-muted">Lead's last message (untrusted model output)</p>
            <div
              data-testid="plan-missing-lead-message"
              // A capped-height scroll box must be keyboard-reachable to be scrollable
              // without a pointer. The label is a fixed string, never the model text.
              tabIndex={0}
              role="region"
              aria-label="Lead's last message"
              className="max-h-80 overflow-y-auto whitespace-pre-wrap break-words rounded-md border border-edge bg-ink/60 px-2.5 py-2 text-sm text-fg"
            >
              {lead}
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
