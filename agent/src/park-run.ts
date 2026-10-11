import type { AnswerVerdict } from "./steering.js";

/** Accept only owner answers already validated and consumed by the steering channel. */
export function isParkAnswer(verdict: AnswerVerdict): boolean {
  if (verdict.kind !== "answer") return false;
  const answers = verdict.answers.map((answer) => answer.trim()).filter(Boolean);
  return answers.length > 0 && answers.every((answer) => answer === "/park");
}
