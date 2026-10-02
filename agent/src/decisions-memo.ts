// Issue #2083: the private, claim-fenced "decisions memo" an mr_rework run reads and a
// completed memo-kind run writes. This module holds the two worker-side invariants every
// seam shares (the signal scan, the prompt builder, the runner read/write): the size cap
// and a UTF-8-safe clamp. The memo is never published and never shown in the PR.

/** Mirrors the API's `MaxDecisionsMemoBytes` (api/internal/workersvc/decision_memo.go).
 *  The server is authoritative (it rejects an oversize body with 400); the worker clamps so
 *  an over-long memo is truncated rather than lost to that rejection. */
export const DECISIONS_MEMO_MAX_BYTES = 8192;

/** Looser transport-hygiene bound for the memo as scanned off signal_done: a memo over it is
 *  dropped, never cut (a cut before redaction could leave an unmatchable secret prefix). It is
 *  NOT the storage cap: the runner redacts the scanned text first and only then clamps to
 *  DECISIONS_MEMO_MAX_BYTES, so a secret straddling the storage cut is redacted whole
 *  rather than cut first and leaked as a prefix. Same size as REPORT_MD_MAX_LEN. */
export const DECISIONS_MEMO_TRANSPORT_MAX_BYTES = 32 * 1024;

/** The run kinds that take part in the memo: every kind that opens a merge request the
 *  user may later have reworked (issue, prompt, self_improve) plus the mr_rework run that
 *  reads it. Only mr_rework consumes the memo; the others only write it. */
export function isDecisionsMemoKind(kind: string): boolean {
  return kind === "issue" || kind === "prompt" || kind === "self_improve" || kind === "mr_rework";
}

/** Truncate `text` to at most `maxBytes` UTF-8 bytes without ever splitting a multibyte
 *  character (a cut mid-sequence would leave a replacement char the API may reject). */
export function clampUtf8Bytes(text: string, maxBytes: number = DECISIONS_MEMO_MAX_BYTES): string {
  const buf = Buffer.from(text, "utf8");
  if (buf.length <= maxBytes) return text;
  let end = maxBytes;
  // Back up over continuation bytes (10xxxxxx) so the cut lands on a character boundary.
  while (end > 0 && (buf[end]! & 0xc0) === 0x80) end--;
  return buf.subarray(0, end).toString("utf8");
}

/** The only memo wire format this worker understands (the API's `format` field). */
const DECISIONS_MEMO_FORMAT = 1;

/** Validate a decisions-memo GET response. Everything in it is untrusted: `enabled` counts
 *  only when it is exactly `true`, and a memo is returned only for format 1 with a non-empty
 *  string body within the cap in UTF-8 bytes. Anything else yields no memo (never a clamp:
 *  an oversize body is a malformed record, not one to truncate into the prompt). */
export function parseDecisionsMemoResponse(res: unknown): { enabled: boolean; memo?: string } {
  if (typeof res !== "object" || res === null) return { enabled: false };
  const r = res as { enabled?: unknown; memo?: unknown };
  if (r.enabled !== true) return { enabled: false };
  const m = r.memo;
  if (typeof m !== "object" || m === null) return { enabled: true };
  const { format, body } = m as { format?: unknown; body?: unknown };
  if (format !== DECISIONS_MEMO_FORMAT) return { enabled: true };
  if (typeof body !== "string" || body.trim() === "") return { enabled: true };
  if (Buffer.byteLength(body, "utf8") > DECISIONS_MEMO_MAX_BYTES) return { enabled: true };
  return { enabled: true, memo: body };
}
