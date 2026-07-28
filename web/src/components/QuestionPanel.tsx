import { useMemo, useState } from "react";
import { Button, Textarea, cx } from "./ui";
import { Markdown } from "./Markdown";
import { answersReady, composeAnswer, encodeAnswerBody, type OpenQuestion } from "../lib/runQuestion";

// PRD #88 M2: the "Answer required" affordance — the run's THIRD human-in-the-loop
// channel, beside the plan gate and user-initiated steering. It renders only while the
// run is parked at `awaiting_input` with an open question derived from the feed
// (deriveOpenQuestion); there is no DTO field behind it (D-L).
//
// Modelled on the plan gate's Request-changes composer (PRD #41, RunView's PlanPanel):
// the same warn-toned bordered card, the same "the run is parked until you decide"
// framing, the same Textarea + submit pair. It differs in one deliberate way — the
// composer is disclosed IMMEDIATELY rather than behind an action button. The gate has
// three verdicts to choose between first, so disclosure earns its keep there; a
// question has exactly one thing to do, and hiding the only control behind a click
// would put a step between the user and the run they are blocking.
//
// 🔴 EVERY STRING IN THE PAYLOAD IS ATTACKER-INFLUENCEABLE (D-K). The lead composes a
// question from repo/issue content it read, so `question`, `header`, `label` and
// `description` are untrusted:
//   - question text → the hardened <Markdown> sink (no rehype-raw, so raw HTML is inert
//     text; react-markdown v10's default urlTransform plus our own schemeIsDangerous
//     kill javascript:/data: hrefs).
//   - option label/description → React text CHILDREN. Never an href, title, style, or
//     dangerouslySetInnerHTML. A `title` would be the worst of these: a whole-subtree
//     textContent assertion cannot see an attribute, so the escape would be untested by
//     construction (see runBadge.ts's badgeTitle note for the same trap).
// Chips are the residual precisely because they are the one place a label wants to
// become an attribute.

// selectionKey identifies one option within one question — a chip's checked state.
function selectionKey(qIndex: number, oIndex: number): string {
  return `${qIndex}:${oIndex}`;
}

export function QuestionPanel({
  open,
  busy,
  onAnswer,
  onCancel,
}: {
  /** The derived open question AND its feed-counted ordinal, taken as ONE object
   *  because they are one derivation — passing them separately is how a caller ends up
   *  showing question 2's round marker over question 3's text. */
  open: OpenQuestion;
  busy: boolean;
  /** Submits the encoded `answer` steering body. The panel owns the encoding so the id
   *  and the answers can never be assembled apart from each other. */
  onAnswer: (body: string) => void;
  /** A parked run is still cancellable — the same escape hatch the revising plan gate
   *  offers, and the only alternative to waiting out the answer deadline. */
  onCancel?: () => void;
}) {
  const { question, ordinal } = open;
  // Chip selection is keyed by (question, option) rather than held per question, so a
  // multiSelect question and a single-select one share one state shape.
  const [picked, setPicked] = useState<Set<string>>(() => new Set());
  const [texts, setTexts] = useState<string[]>(() => question.questions.map(() => ""));

  const toggle = (qIndex: number, oIndex: number, multi: boolean) => {
    const key = selectionKey(qIndex, oIndex);
    setPicked((prev) => {
      const next = new Set(prev);
      if (next.has(key)) {
        next.delete(key);
        return next;
      }
      // Single-select: picking replaces this question's other pick rather than adding
      // to it. Scoped to THIS question's prefix so a second question keeps its own.
      if (!multi) {
        for (const k of prev) if (k.startsWith(`${qIndex}:`)) next.delete(k);
      }
      next.add(key);
      return next;
    });
  };

  // The answers array is INDEX-ALIGNED with question.questions — the worker zips the
  // two back together (answeredPrompt), so a shifted or short array silently attributes
  // an answer to the wrong question.
  const answers = useMemo(
    () =>
      question.questions.map((q, qi) =>
        composeAnswer(
          q.options.filter((_, oi) => picked.has(selectionKey(qi, oi))).map((o) => o.label),
          texts[qi] ?? "",
        ),
      ),
    [question.questions, picked, texts],
  );

  const ready = answersReady(answers, question.questions.length);
  const multiple = question.questions.length > 1;

  return (
    <div className="overflow-hidden rounded-xl border border-warn/50 bg-warn/5">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-warn/30 bg-warn/10 px-4 py-3">
        <div>
          <h2 className="flex items-center gap-2 text-sm font-semibold text-warn">
            Answer required
            {ordinal > 1 && (
              <span className="inline-flex items-center rounded-md border border-warn/40 bg-warn/[0.12] px-1.5 py-px font-mono text-[11px] font-semibold text-warn">
                q{ordinal}
              </span>
            )}
          </h2>
          <p className="text-xs text-muted">
            The agent stopped to ask{multiple ? ` ${question.questions.length} questions` : ""} — the run is
            parked until you answer.
          </p>
        </div>
        {onCancel && (
          <Button variant="danger" disabled={busy} onClick={onCancel}>
            Cancel run
          </Button>
        )}
      </div>

      <div className="space-y-4 p-4">
        {question.questions.map((q, qi) => (
          <div key={qi} className="space-y-2">
            {q.header.trim() !== "" && (
              // Plain text child: model-authored, so never a markdown/HTML sink and never
              // an attribute.
              <div className="text-xs font-semibold uppercase tracking-wider text-faint">{q.header}</div>
            )}
            <div className="rounded-lg border border-edge bg-surface p-3">
              <Markdown content={q.question} />
            </div>

            {q.options.length > 0 && (
              <div className="space-y-1.5">
                <div className="flex flex-wrap gap-1.5">
                  {q.options.map((o, oi) => {
                    const on = picked.has(selectionKey(qi, oi));
                    return (
                      <button
                        key={oi}
                        type="button"
                        // aria-pressed rather than a checkbox role: this is a toggle
                        // button, and the single/multi distinction is carried by the hint
                        // line below rather than by faking radio semantics on a control
                        // that can also be deselected.
                        aria-pressed={on}
                        disabled={busy}
                        onClick={() => toggle(qi, oi, q.multiSelect)}
                        className={cx(
                          "max-w-full rounded-full border px-2.5 py-1 text-left text-xs transition-colors disabled:opacity-50",
                          on
                            ? "border-brand/60 bg-brand/15 text-brand"
                            : "border-edge-strong bg-raised text-muted hover:border-brand/40 hover:text-fg",
                        )}
                      >
                        <span className="font-medium">{o.label}</span>
                        {o.description.trim() !== "" && (
                          <span className="ml-1.5 text-faint">— {o.description}</span>
                        )}
                      </button>
                    );
                  })}
                </div>
                <p className="text-[11px] text-faint">
                  {q.multiSelect ? "Pick any that apply" : "Pick one"} — or write your own answer below.
                  Suggestions come from the agent.
                </p>
              </div>
            )}

            <Textarea
              rows={q.options.length > 0 ? 2 : 3}
              aria-label={multiple ? `Answer to question ${qi + 1}` : "Your answer"}
              placeholder={
                q.options.length > 0
                  ? "Add detail, or answer in your own words…"
                  : "Your answer (sent back into the agent's session)…"
              }
              value={texts[qi] ?? ""}
              onChange={(e) =>
                setTexts((prev) => {
                  const next = [...prev];
                  next[qi] = e.target.value;
                  return next;
                })
              }
            />
          </div>
        ))}

        <div className="flex flex-wrap items-center justify-between gap-2">
          <span className="text-[11px] text-faint">
            Your answer resumes the agent's session where it stopped. Never paste a credential, token or
            password — the agent must not ask for one, and the answer is stored with the run.
          </span>
          <Button disabled={busy || !ready} onClick={() => onAnswer(encodeAnswerBody(question.questionId, answers))}>
            Send answer
          </Button>
        </div>
      </div>
    </div>
  );
}
