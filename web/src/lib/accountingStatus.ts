import type { RunMessage } from "./api";

/** Accounting evidence is retained in the stream but carries no visible activity. */
export function isAccountingStatus(message: RunMessage): boolean {
  const payload = message.payload;
  return message.kind === "status" && payload !== null && typeof payload === "object"
    && "event" in payload && payload.event === "codex_response_usage";
}

/** The model-written "Now" line (PRD #2603) lives on the run progress card; as a transcript
 *  event it would only render as an unrenderable-kind row. */
export function isProgressNote(message: RunMessage): boolean {
  return message.kind === "progress_note";
}
