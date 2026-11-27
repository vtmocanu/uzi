import { stripUnsafeChars } from "./safeText";

/**
 * How an issue is named to a reader: its issue tracker key ("ABC-12") on a repo linked
 * to an issue tracker (PRD 4173 M6), else "#iid". The key is untrusted issue tracker
 * text, so it goes through stripUnsafeChars before it reaches a label, title or toast.
 */
export function issueRef(iid: number, issueKey?: string | null): string {
  const key = issueKey ? stripUnsafeChars(issueKey) : "";
  return key !== "" ? key : `#${iid}`;
}
