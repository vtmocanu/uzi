/**
 * A run on a repo linked to an external issue tracker (PRD 4173) carries the tracker's issue key
 * (`ABC-12`) in the claim's `issue_key`; the claim's `issue_iid` is then that issue's number in its
 * project, and `#<iid>` on the code host is an UNRELATED forge issue. Such a run never closes
 * anything by a forge closing keyword and names its issue by key.
 */

export interface LinkedIssue {
  /** True iff the claim carries `issue_key` at all, whatever its value (fail closed). */
  linked: boolean;
  /** The key when it is a well-formed issue key; null otherwise (render "the linked issue"). */
  label: string | null;
}

const ISSUE_KEY_RE = /^[A-Za-z][A-Za-z0-9_]*-[1-9][0-9]*$/u;
const ISSUE_KEY_MAX = 64;

/** What a linked run's text says when its key is absent or malformed. */
const LINKED_ISSUE_FALLBACK_LABEL = "the linked issue";

export function linkedIssueKey(claim: { issue_key?: unknown }): LinkedIssue {
  const raw = claim.issue_key;
  if (raw === undefined) return { linked: false, label: null };
  const label = typeof raw === "string" && raw.length <= ISSUE_KEY_MAX && ISSUE_KEY_RE.test(raw) ? raw : null;
  return { linked: true, label };
}

/** The label a linked run's prose uses for its issue: the key, else "the linked issue". */
export function linkedIssueLabel(linked: LinkedIssue): string {
  return linked.label ?? LINKED_ISSUE_FALLBACK_LABEL;
}
