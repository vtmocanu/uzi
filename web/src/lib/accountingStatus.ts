import type { RunMessage } from "./api";

/** Accounting evidence is retained in the stream but carries no visible activity. */
export function isAccountingStatus(message: RunMessage): boolean {
  const payload = message.payload;
  return message.kind === "status" && payload !== null && typeof payload === "object"
    && "event" in payload && payload.event === "codex_response_usage";
}
