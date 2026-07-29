import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent } from "react";
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

/**
 * UnreadableQuestion is what a parked run shows when its `question` payload cannot be
 * used — no `question_id`, or no renderable questions (see parseQuestionPayload).
 *
 * Without it the run sits at "needs your answer" with NO panel and NO explanation until
 * the 24h deadline fails it. Not a dead end — Stop run is in the steer card below — but
 * silence, and the user has no way to learn that answering is impossible rather than
 * merely unavailable. That is the worse half: an absent affordance reads as "not loaded
 * yet", so the reasonable response is to wait, which is exactly what cannot help.
 *
 * It states the ONE thing the user can act on. It deliberately does NOT offer a composer:
 * the api rejects an answer that names no question, so a Send here would 400 every time
 * — the same reasoning that makes parseQuestionPayload return null rather than an id-less
 * payload.
 */
export function UnreadableQuestion({ busy, onCancel }: { busy: boolean; onCancel?: () => void }) {
  return (
    <div className="overflow-hidden rounded-xl border border-warn/50 bg-warn/5">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-warn/30 bg-warn/10 px-4 py-3">
        <div>
          <h2 className="text-sm font-semibold text-warn">The agent's question could not be read</h2>
          <p className="text-xs text-muted">
            This run is parked waiting for an answer, but the question it sent cannot be displayed, so
            there is nothing here to answer.
          </p>
        </div>
        {onCancel && (
          <Button variant="danger" disabled={busy} onClick={onCancel}>
            Cancel run
          </Button>
        )}
      </div>
      <div className="p-4 text-sm text-muted">
        <p>
          The run will keep waiting until its answer deadline expires, then fail. Cancelling it now is
          usually better than waiting. The raw question is in the activity log below.
        </p>
      </div>
    </div>
  );
}

export function QuestionPanel({
  open,
  busy,
  canSteer = true,
  onAnswer,
  onCancel,
}: {
  /** The derived open question AND its feed-counted ordinal, taken as ONE object
   *  because they are one derivation — passing them separately is how a caller ends up
   *  showing question 2's round marker over question 3's text. */
  open: OpenQuestion;
  busy: boolean;
  /** False for a NON-OWNER viewer (a non-owner admin can open the owner-or-admin run
   *  view, but POST /inputs is user-scoped and 404s for them). useRunStream states the
   *  rule outright — "never a broken Send that 404s" — and SteerQueueCard already obeys
   *  it. Defaults true so an owner is never gated by an absent prop.
   *
   *  It hides the CONTROLS, not the content: the run view is deliberately visible to a
   *  non-owner admin, so hiding the question itself would remove information they are
   *  entitled to and turn a permissions boundary into a blank page. */
  canSteer?: boolean;
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
  const hintId = useId();
  // Located by querySelector off a stable container ref rather than by a direct ref: the
  // `ui` Textarea is a plain (non-forwardRef) component. RunView's DispositionControls
  // solves the same problem the same way for the same reason — changing a shared
  // primitive to buy one focus call would be the larger edit.
  const bodyRef = useRef<HTMLDivElement>(null);

  // MEASURED (web-ux, browser): when the panel appears, focus is left on <body> and the
  // first answer box is tab stop 25 of 40, behind the whole sidebar, with no skip link.
  // The run is parked on this control, so a keyboard user is made to Tab ~26 times to
  // reach the one thing the app says is blocking.
  //
  // Moving focus on mount is normally the wrong instinct, and RunEvent's ToolResultBody
  // deliberately does NOT do it ("never on the error auto-expand, which would steal focus
  // as the feed renders"). The distinction is that a feed row appearing is incidental to
  // what the user is doing, whereas this panel appearing IS the app stopping and asking.
  // Keyed on questionId so a second park re-focuses: the panel unmounts and remounts
  // between questions, but keying on the id survives a future change that reuses it.
  useEffect(() => {
    bodyRef.current?.querySelector("textarea")?.focus();
  }, [question.questionId]);

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

  // Non-owner: the question is readable, nothing is answerable. Options render as inert
  // text rather than disabled buttons — a greyed-out control still invites a click and
  // then refuses it, which is the affordance-that-lies the canSteer rule exists to avoid.
  if (!canSteer) {
    return (
      <div className="overflow-hidden rounded-xl border border-warn/40 bg-warn/5">
        <div className="border-b border-warn/30 bg-warn/10 px-4 py-3">
          <h2 className="text-sm font-semibold text-warn">
            Waiting on an answer{ordinal > 1 ? ` · q${ordinal}` : ""}
          </h2>
          <p className="text-xs text-muted">
            The agent asked the run's owner a question. Only they can answer it.
          </p>
        </div>
        <div className="space-y-3 p-4">
          {question.questions.map((q, qi) => (
            <div key={qi} className="space-y-1.5">
              {q.header.trim() !== "" && (
                <div className="text-xs font-semibold uppercase tracking-wider text-faint">{q.header}</div>
              )}
              <Markdown content={q.question} />
              {q.options.length > 0 && (
                <div className="flex flex-wrap gap-1.5">
                  {q.options.map((o, oi) => (
                    <span
                      key={oi}
                      className="rounded-full border border-edge-strong bg-raised px-2 py-[2px] text-[11px] text-muted"
                    >
                      {o.label}
                    </span>
                  ))}
                </div>
              )}
            </div>
          ))}
        </div>
      </div>
    );
  }

  const submit = () => {
    if (busy || !ready) return;
    onAnswer(encodeAnswerBody(question.questionId, answers));
  };

  // Cmd/Ctrl+Enter submits from any answer box, pairing with the focus-on-mount above so
  // the whole keyboard path is: park → type → chord. Without it the last step is Tabbing
  // forward past every remaining chip and textarea to reach Send.
  //
  // NOT bare Enter, which ChatComposer uses. That convention holds for a single-field
  // composer; this panel can hold several questions, so Enter must stay a newline or a
  // multi-question answer becomes unwritable. The chord is also why Shift+Enter needs no
  // special case here — it is already just a newline.
  //
  // Guarded on `ready` through submit() rather than firing a rejected request: the button
  // is disabled in the same state, and a chord that silently does nothing is better than
  // one that 400s.
  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      submit();
    }
  };

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
            {/* `multiple` used to gate the whole phrase, so a SINGLE-question park — the
                modal case in production — read "The agent stopped to ask — the run is
                parked". The count is the variable part; the noun is not. */}
            The agent stopped to ask{multiple ? ` ${question.questions.length} questions` : " a question"} —
            the run is parked until you answer.
          </p>
        </div>
        {onCancel && (
          <Button variant="danger" disabled={busy} onClick={onCancel}>
            Cancel run
          </Button>
        )}
      </div>

      <div ref={bodyRef} className="space-y-4 p-4">
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
                {/* MEASURED (web-ux): with the chips in a bare <div>, question 2's chips
                    sit between "Answer to question 1" and "Answer to question 2" in tab
                    order and read as belonging to question 1 — the visual grouping was
                    carried entirely by CSS. The group names itself after its question, and
                    aria-describedby carries the single-vs-multi rule programmatically
                    rather than leaving it in a <p> nothing points at. */}
                <div
                  role="group"
                  aria-label={q.header.trim() !== "" ? q.header : `Options for question ${qi + 1}`}
                  aria-describedby={`${hintId}-${qi}`}
                  className="flex flex-wrap gap-1.5"
                >
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
                          // The leading space is INSIDE the string: the visual gap is a
                          // CSS margin, which contributes nothing to the accessible name,
                          // so without it the chip announces as "Postgres table— Simplest".
                          <span className="ml-1.5 text-faint">{` — ${o.description}`}</span>
                        )}
                      </button>
                    );
                  })}
                </div>
                <p id={`${hintId}-${qi}`} className="text-[11px] text-faint">
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
              onKeyDown={onKeyDown}
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
            Your answer resumes the agent's session where it stopped.{" "}
            <span className="whitespace-nowrap">⌘/Ctrl + Enter</span> sends. Never paste a credential, token
            or password — the agent must not ask for one, and the answer is stored with the run.
          </span>
          <Button disabled={busy || !ready} onClick={submit}>
            Send answer
          </Button>
        </div>
      </div>
    </div>
  );
}
