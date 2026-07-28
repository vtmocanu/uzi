// PRD #88 M2: the run feed's clarification-question derivation, kept out of the
// components so the parse and the open/closed rule are unit-tested in isolation —
// the same split runBadge.ts and runStream.ts use.
//
// THERE IS NO DTO FIELD AND NO COLUMN FOR THE OPEN QUESTION (PRD #88 D-L). The state
// is derived from the feed by `seq`, exactly as RunView's derivePlanRevision derives
// the gate state from the latest of {plan, plan_revising}. Web, Slack and the CLI
// therefore share one derivation rule instead of three, and adding a surface costs
// nothing on the wire.
//
// EVERY STRING REACHING THIS FILE IS UNTRUSTED. The lead composes a question from repo
// and issue content it read, so `question`, `header`, and each option's `label` /
// `description` are attacker-influenceable model output. Nothing here sanitizes —
// parsing only establishes SHAPE. The render sites own the sink discipline: question
// and answer prose go through the hardened <Markdown>, option labels render as React
// text children, and neither may ever reach href / title / style /
// dangerouslySetInnerHTML (D-K).

import type { RunMessage } from "./api";

/** One suggested answer on a question. Both fields are model-authored. */
export interface QuestionOption {
  label: string;
  description: string;
}

/** One question in an `ask_user` call — the payload shape frozen by M1
 *  (`AskUserQuestion` in agent/src/protocol.ts). Free text is ALWAYS allowed, options
 *  or not: they are a convenience, never a closed set. */
export interface AskQuestion {
  question: string;
  header: string;
  options: QuestionOption[];
  multiSelect: boolean;
}

/** Payload of a `question` run-message: `{ question_id, questions }` as of `39cffd4c`
 *  (D-R). It carried a third field, `generation`, which the design doc labelled "the
 *  stale-answer discriminator" — a label that was false, since nothing ever read it
 *  back. It has been removed from the wire; see questionOrdinal below for the display
 *  ordinal that replaces it. */
export interface QuestionPayload {
  /** The question's stable identity, minted at the first park and RE-USED verbatim
   *  when a resumed worker re-parks on the same question. The answer names it, and the
   *  api rejects an answer naming any other id — so it is not decorative, it is the
   *  whole stale-answer guard and must round-trip byte-exact.
   *
   *  It is the ONLY staleness key. Nothing in this file may add a second one keyed on a
   *  clock or an arrival ordinal: a requeue re-parks the run, so every such key is
   *  re-stamped by an event the user neither caused nor can see, and a conjunction is
   *  only as strong as its weaker clause. */
  questionId: string;
  questions: AskQuestion[];
}

/** Payload of an `answer` run-message — the echo the worker emits once an answer is
 *  applied, so the round-trip is auditable in the feed (mirrors `plan_feedback`). */
export interface AnswerPayload {
  answers: string[];
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function asString(v: unknown): string {
  return typeof v === "string" ? v : "";
}

// parseOptions keeps only well-shaped entries and drops the rest, rather than failing
// the whole question: a malformed option is a chip the user does not get, whereas
// rejecting the payload would hide a question the run is genuinely parked on and the
// user would have no way to unpark it (the run then dies on the answer deadline).
// Every question is answerable by free text, so a dropped chip is never a dead end.
function parseOptions(v: unknown): QuestionOption[] {
  if (!Array.isArray(v)) return [];
  const out: QuestionOption[] = [];
  for (const raw of v) {
    const rec = asRecord(raw);
    if (!rec) continue;
    const label = asString(rec["label"]).trim();
    if (label === "") continue;
    out.push({ label, description: asString(rec["description"]) });
  }
  return out;
}

// parseQuestions is the opposite call: a question with no text is not renderable and
// not answerable, so it is dropped. If that empties the list, parseQuestionPayload
// treats the whole payload as absent (see below).
function parseQuestions(v: unknown): AskQuestion[] {
  if (!Array.isArray(v)) return [];
  const out: AskQuestion[] = [];
  for (const raw of v) {
    const rec = asRecord(raw);
    if (!rec) continue;
    const question = asString(rec["question"]);
    if (question.trim() === "") continue;
    out.push({
      question,
      header: asString(rec["header"]),
      options: parseOptions(rec["options"]),
      multiSelect: rec["multiSelect"] === true,
    });
  }
  return out;
}

/**
 * parseQuestionPayload reads a `question` message's payload, or returns null when it
 * is not usable. Defensive by construction: the payload crosses the JSON boundary, so
 * the RunMessage.payload type (`unknown`) is the only honest one and nothing here may
 * assume a field exists.
 *
 * A missing `question_id` returns null rather than an id-less payload. That is the one
 * field with no safe default: the answer body must name it and the api rejects an
 * empty one, so a panel rendered without it would offer a composer whose every submit
 * 400s. Showing no panel is worse than nothing only if the run could otherwise be
 * unparked — it could not.
 */
export function parseQuestionPayload(payload: unknown): QuestionPayload | null {
  const rec = asRecord(payload);
  if (!rec) return null;
  const questionId = asString(rec["question_id"]).trim();
  if (questionId === "") return null;
  const questions = parseQuestions(rec["questions"]);
  if (questions.length === 0) return null;
  return { questionId, questions };
}

/** parseAnswerPayload reads an `answer` message's payload for the feed row. A payload
 *  with no usable `answers` array yields an empty list, which the row renders as
 *  "(no answer text)" rather than vanishing — the round-trip happened either way. */
export function parseAnswerPayload(payload: unknown): AnswerPayload {
  const rec = asRecord(payload);
  const raw = rec?.["answers"];
  if (!Array.isArray(raw)) return { answers: [] };
  return { answers: raw.filter((a): a is string => typeof a === "string") };
}

/** The open question plus the facts about it that the feed — not the payload — knows.
 *  Kept as a wrapper rather than folded into QuestionPayload so the wire shape stays
 *  exactly the wire shape: `ordinal` is derived here and must never look like something
 *  the worker sent. */
export interface OpenQuestion {
  question: QuestionPayload;
  /** 1-based "question N of this run", counted from the feed. See questionOrdinal. */
  ordinal: number;
}

/**
 * deriveOpenQuestion folds the feed into "which question, if any, is the run waiting
 * on". The rule is the LATEST of {question, answer} BY SEQ, matching derivePlanRevision's
 * latest-of-{plan, plan_revising}: a newer `question` re-opens after an earlier answer,
 * and a newer `answer` closes the one before it.
 *
 * Reading only the newest `question` would be wrong in the window between the user
 * answering and the run reporting `running`: the worker emits the `answer` message
 * before that state report, so a question-only rule would keep offering a composer for
 * a question already resolved — and a second submit would 409 as stale.
 */
export function deriveOpenQuestion(messages: RunMessage[]): OpenQuestion | null {
  let latest: RunMessage | undefined;
  for (const m of messages) {
    if (m.kind !== "question" && m.kind !== "answer") continue;
    if (!latest || m.seq > latest.seq) latest = m;
  }
  if (!latest || latest.kind !== "question") return null;
  const question = parseQuestionPayload(latest.payload);
  if (!question) return null;
  return { question, ordinal: questionOrdinal(messages, latest.seq) };
}

/**
 * questionOrdinal answers "which question of this run is this" — 1-based — by COUNTING
 * `question` messages up to and including the given seq.
 *
 * This replaces the `generation` field the payload used to carry (D-R). The difference
 * is not cosmetic: a counter minted worker-side is a CLAIM, restated on every park, and
 * a worker-death requeue re-parks on the same question and would restate it wrongly. The
 * feed is a durable, gapless log, so the count of `question` messages before this one is
 * a FACT about the run and stays correct across a requeue for free.
 *
 * Returns 0 for a seq with no matching question, which callers render as no marker at
 * all rather than guessing "1".
 */
export function questionOrdinal(messages: RunMessage[], seq: number): number {
  let n = 0;
  let found = false;
  for (const m of messages) {
    if (m.kind !== "question" || m.seq > seq) continue;
    n += 1;
    if (m.seq === seq) found = true;
  }
  return found ? n : 0;
}

/** The `answer` steering body. JSON, unlike every other kind, because an answer must
 *  name the question it answers (PRD #88 D-P C2); `approve_plan` already establishes
 *  the JSON-body idiom. Slack's inbound is free text, so the replier resolves its
 *  thread anchor to a question id and constructs this same shape server-side — one
 *  wire contract, not two. */
export function encodeAnswerBody(questionId: string, answers: string[]): string {
  return JSON.stringify({ question_id: questionId, answers });
}

/**
 * composeAnswer folds one question's chip selection and free text into the single
 * string the agent reads. Chips first (they are the model's own vocabulary, so echoing
 * the labels verbatim is what makes them useful), then the user's prose.
 *
 * Free text stands alone when nothing is picked, and REFINES rather than replaces a
 * selection when both are present — the PRD's "free-text answers are always allowed
 * even when options are offered" means the two compose, not that one wins.
 */
export function composeAnswer(selected: string[], freeText: string): string {
  const picks = selected.map((s) => s.trim()).filter((s) => s !== "");
  const text = freeText.trim();
  if (picks.length === 0) return text;
  const joined = picks.join(", ");
  return text === "" ? joined : `${joined} — ${text}`;
}

/** answersReady reports whether every question has something to send. A question is
 *  answered by a chip, by free text, or by both; an empty answer to ANY question blocks
 *  the submit, because the answers array is index-aligned with the questions and a
 *  blank entry reaches the lead as "(no answer given)". */
export function answersReady(answers: string[], questionCount: number): boolean {
  if (answers.length !== questionCount) return false;
  return answers.every((a) => a.trim() !== "");
}
