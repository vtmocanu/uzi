package runenv

// The form budget (PRD #4416). The worker registers, for every secret value a claim
// carries, each encoded form of it for masking and scanning, and fails the claim once the
// distinct counted strings pass FormCharsMax UTF-16 units (RUN_ENV_FORM_CHARS_MAX in
// agent/src/run-env-forms.ts). The store bounds a repo's secret value bytes by
// MaxClaimSecretBytes at write time, so a store filled to that bound never produces a
// claim that fails every run.
//
// The counted strings are every registered form and, for each form registered for
// redaction, its JSON-escaped copy (the copy the worker's logger indexes beside the form)
// when that differs from the form. Diff-shaped forms are scan needles only and have no
// copy. The bound covers counted units only. The redactor's invisible-stripped copies
// (made of every redaction form once any form holds a control or format character) are
// not counted; they are an extra memory cost of at most the summed length of the
// redaction forms, which the agent's cost test measures.
//
// FormCharsPerSecretByte is an analytic upper bound of the counted units one UTF-8 byte of
// a secret value can add. Worst case is a one-byte control character, which JSON and
// the HTML-safe encoders write as a six-unit \uXXXX escape and whose escaped copy is seven
// units (the backslash doubles). For a value of N bytes (its UTF-16 length is at most N):
//
//	per value variant (up to 4: as given, trailing breaks stripped, LF-normalized, both):
//	  raw                                  N
//	  JSON, JSON with \/, HTML-safe x2    4 x 6N = 24N
//	  percent-encoded, lower-case, `+`    3 x 3N =  9N
//	  base64: 3 standard + 3 URL-safe     6 x 4N/3 = 8N   (each interior is at most 4N/3 units)
//	  subtotal                            42N, so 4 variants 168N
//	  escaped copies: raw 6N, the 4 JSON-family forms 4 x 7N = 28N; a percent or base64
//	  form holds no character JSON escapes, so its copy is itself and adds nothing
//	                                      34N, so 4 variants 136N
//	line forms of a multi-line value (every line except the standard PEM markers on the agent's list and ASCII space/tab-only lines
//	  registers, raw and JSON-escaped; the lines total at most N)
//	                                      N + 6N = 7N
//	  escaped copies of the two line forms 6N + 7N = 13N
//	diff-shaped forms (6: LF and CRLF joins for each of the prefixes +, -, space): a line
//	  break byte adds at most 3 units (CRLF plus prefix), so each form is at most 3N
//	                                      6 x 3N = 18N
//	total                                 168N + 136N + 7N + 13N + 18N = 342N
//
// The strings are counted distinct, so duplicates only lower the count, and a form shorter
// than the 8-unit secret floor is dropped. No rounding term is positive (each base64
// interior is at most 4N/3), so the bound needs no per-value constant. The worker's check
// is "count > FormCharsMax", so a claim of at most MaxClaimSecretBytes secret bytes counts
// at most FormCharsPerSecretByte*MaxClaimSecretBytes <= FormCharsMax and never fails it.
// fixtures/run-env/form-budget.json carries the same numbers. The agent test
// run-env-forms-budget.test.ts samples adversarial values against the worker's own form
// builder and pins the form counts and per-family lengths this derivation assumes (8
// provider forms, 6 base64, 4 variants, 2 line forms per line, 6 diff-shaped forms, and
// the escaped copy of a raw or JSON-family form at most 6 or 7 units per byte), so a
// form the worker gains fails that test; it does not prove the bound for every value.
const (
	FormCharsMax           = 40_000_000
	FormCharsPerSecretByte = 342

	// MaxClaimSecretBytes is the most secret value bytes (every live, disabled, pending
	// and retired secret row of the repo and the user scope) a repo may hold.
	MaxClaimSecretBytes = FormCharsMax / FormCharsPerSecretByte
)
