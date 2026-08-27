// sanitizeLabel strips Unicode FORMAT characters (category Cf) from a user-authored
// token label before it is rendered (PRD #111, web-ux F12).
//
// 🔴 REACT ESCAPING DOES NOT COVER THIS. Escaping stops a label becoming markup; it
// does nothing to a bidi override, which survives intact and REORDERS the text around
// it. Measured in a browser against the mock build: `console-key` renamed to U+202E +
// `yek-elosnoc` renders a settings row that visually reads `console-key` while holding
// a different string, and `default` + U+200B produces two rows both drawing the word
// "default", separable only by a badge.
//
// This is the symmetric case to `cellText` in cmd/uzi, and it exists for the identical
// reason. The server's validateSecretLabel now rejects Cf, so no NEW label can carry
// one — but that is a statement about what the server accepts, and this is a statement
// about what the renderer does with what it is handed. Three routes reach a renderer
// without passing that validator: a label stored before the validator landed (existing
// rows are never re-validated, and nothing re-validates on read), a future write path
// that skips it, and a row written straight to the database. Depending on the far side
// of a trust boundary for local safety is what turns one regression into two.
//
// Deliberately NOT a broader cleanup. It mirrors the Go predicate — `unicode.Cf` — and
// nothing else, so the two stay comparable. Blank-rendering NON-Cf characters (U+3164
// HANGUL FILLER, U+2800 BRAILLE BLANK, NBSP) are a known, accepted residual on both
// sides: that set is unbounded, homoglyphs are the same class with no character rule
// reaching them, and a longer list here than in Go would make the two implementations
// disagree about what a label is.
export function sanitizeLabel(label: string): string {
  // \p{Cf} with the `u` flag is the same category the Go side tests with
  // unicode.In(r, unicode.Cf). Control characters are already rejected by the
  // validator AND by the database CHECK, and are not re-handled here: this function
  // has one job so its equivalence to the Go predicate stays checkable.
  return label.replace(/\p{Cf}/gu, "");
}
