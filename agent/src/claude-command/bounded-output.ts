// Bounded inline output for a guarded model command (issue #2471, M2).
//
// The command's stdout and stderr are consumed continuously, so the transport never blocks the
// command, but only a head and a tail are ever retained: the worker never buffers the full
// stream. The inline text is head + marker + tail, at most INLINE_CAP_CHARS characters in
// total (UTF-16 code units, the unit `String.length` counts). A best-effort copy of the output
// may be on the spill file the wrapper writes, which this module never touches.
//
// Redaction. A secret can straddle a cut, so the head and tail windows are kept
// (longest secret length - 1) characters LONGER than the final budget, redacted whole, and only
// then trimmed. The margin is capped at MAX_MARGIN (65536) characters; a secret longer than
// MAX_MARGIN + 1 would need more than the cap, so when the output has to be cut the inline
// preview is omitted entirely (counts and the truncation note remain) rather than risk a partial
// secret. When the redacted window turns out shorter than its budget plus the margin (the
// redactor shrank it), its buffer edge would end up in the inline text, so the margin is dropped
// from that edge instead: a partial secret at a buffer edge never reaches the model. The
// redactor's invisible-separator matching can span more than the secret's own length; that is the
// same best-effort net `makeTextRedactor` documents, not a guarantee beyond it.
//
// Control characters. Redaction runs first (it sees the raw text, including invisible-separator
// splits); the result is then made safe to display: CRLF and lone CR become LF, and every other
// C0/C1 control character (ESC, BEL, NUL, DEL, ...), U+2028/U+2029 and the bidi controls become
// U+FFFD. Tab and LF are kept. Lengths only ever shrink in this pass (CRLF), so the cap holds.

import { StringDecoder } from "node:string_decoder";
import { makeTextRedactor } from "../redact.js";

/** The inline text cap, head + marker + tail, in characters. */
export const INLINE_CAP_CHARS = 30_000;
const HALF = INLINE_CAP_CHARS / 2;
/** Upper bound on the redaction margin, so a pathological secret cannot grow retained memory. */
const MAX_MARGIN = 65_536;

// oxlint-disable-next-line no-control-regex
const UNSAFE_RE = /[\u0000-\u0008\u000b\u000c\u000e-\u001f\u007f-\u009f\u061c\u200e\u200f\u2028\u2029\u202a-\u202e\u2066-\u2069]/g;

function displaySafe(text: string): string {
  return text.replace(/\r\n?/g, "\n").replace(UNSAFE_RE, "\uFFFD");
}

/** Drop a lone surrogate at either edge so a cut never emits a broken pair. */
function wellFormedEdges(text: string): string {
  let out = text;
  if (out.length > 0) {
    const first = out.charCodeAt(0);
    if (first >= 0xdc00 && first <= 0xdfff) out = out.slice(1);
  }
  if (out.length > 0) {
    const last = out.charCodeAt(out.length - 1);
    if (last >= 0xd800 && last <= 0xdbff) out = out.slice(0, -1);
  }
  return out;
}

export interface RenderedOutput {
  /** The inline text, at most INLINE_CAP_CHARS characters. */
  readonly text: string;
  /** True when the middle of the output was omitted (the inline text is a preview). */
  readonly truncated: boolean;
  /** True when no inline preview is shown at all (a secret too long to redact across the cut). */
  readonly previewOmitted?: boolean;
}

export class BoundedOutput {
  private readonly redact: (text: string) => string;
  private readonly margin: number;
  /** A secret is longer than the margin can cover: a cut output gets no inline preview. */
  private readonly previewUnsafe: boolean;
  private readonly headKeep: number;
  private readonly tailKeep: number;
  private readonly decoders = [new StringDecoder("utf8"), new StringDecoder("utf8")];
  private head = "";
  private tail = "";
  private chars = 0;
  private bytes = 0;

  constructor(secrets: readonly string[] = []) {
    this.redact = makeTextRedactor([...secrets]);
    const longest = secrets.reduce((max, s) => (typeof s === "string" && s.length > max ? s.length : max), 0);
    this.margin = Math.min(MAX_MARGIN, Math.max(0, longest - 1));
    this.previewUnsafe = longest - 1 > MAX_MARGIN;
    this.headKeep = HALF + this.margin;
    this.tailKeep = HALF + this.margin;
  }

  /** Total bytes consumed from both streams. */
  get totalBytes(): number { return this.bytes; }

  /** Characters currently retained (head + tail buffers); bounded regardless of input size. */
  get retainedChars(): number { return this.head.length + this.tail.length; }

  /** Feed one chunk of stream `source` (0 = stdout, 1 = stderr). Never throws on content. */
  write(source: 0 | 1, chunk: Buffer | string): void {
    const buffer = typeof chunk === "string" ? Buffer.from(chunk, "utf8") : chunk;
    this.bytes += buffer.length;
    this.append(this.decoders[source]!.write(buffer));
  }

  /** Flush the multibyte decoders; call once when both streams have ended. */
  end(): void {
    for (const decoder of this.decoders) this.append(decoder.end());
  }

  private append(text: string): void {
    if (text.length === 0) return;
    this.chars += text.length;
    if (this.head.length < this.headKeep) this.head += text.slice(0, this.headKeep - this.head.length);
    // The tail buffer holds at least the last tailKeep characters and at most twice that.
    this.tail = text.length >= this.tailKeep ? text.slice(-this.tailKeep) : this.tail + text;
    if (this.tail.length > 2 * this.tailKeep) this.tail = this.tail.slice(-this.tailKeep);
  }

  render(): RenderedOutput {
    // Everything seen is still held: head and tail overlap or abut.
    const missing = this.chars - this.head.length;
    if (missing <= 0 || (missing <= this.tail.length)) {
      const full = missing <= 0 ? this.head : this.head + this.tail.slice(this.tail.length - missing);
      const clean = displaySafe(this.redact(full));
      if (clean.length <= INLINE_CAP_CHARS) return { text: clean, truncated: false };
      return this.cut(clean, clean, false);
    }
    return this.cut(this.redact(this.head), this.redact(this.tail), true);
  }

  /** Assemble head + marker + tail from redacted windows. `windowsAreEdges` is true when each
   *  window ends or starts at a retention edge (so the margin must be dropped when it is short). */
  private cut(headWindow: string, tailWindow: string, windowsAreEdges: boolean): RenderedOutput {
    if (this.previewUnsafe) {
      return { text: `\n[... output preview omitted: ${String(this.bytes)} bytes in total ...]\n`, truncated: true, previewOmitted: true };
    }
    const marker = `\n[... output truncated: ${String(this.bytes)} bytes in total; the middle is omitted ...]\n`;
    const available = INLINE_CAP_CHARS - marker.length;
    const h = Math.ceil(available / 2);
    const t = available - h;
    let headPart: string;
    let tailPart: string;
    if (!windowsAreEdges) {
      headPart = headWindow.slice(0, h);
      tailPart = tailWindow.slice(tailWindow.length - t);
    } else {
      headPart = headWindow.length >= h + this.margin ? headWindow.slice(0, h)
        : headWindow.slice(0, Math.max(0, headWindow.length - this.margin));
      headPart = headPart.slice(0, h);
      tailPart = tailWindow.length >= t + this.margin ? tailWindow.slice(tailWindow.length - t)
        : tailWindow.slice(Math.min(this.margin, tailWindow.length));
      tailPart = tailPart.slice(Math.max(0, tailPart.length - t));
    }
    const text = displaySafe(wellFormedEdges(headPart)) + marker + displaySafe(wellFormedEdges(tailPart));
    return { text, truncated: true };
  }
}
