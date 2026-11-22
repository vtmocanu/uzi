package runenv

// The form budget (PRD #4416). The worker registers, for every secret value a claim
// carries, each encoded form of it for masking and scanning, and fails the claim once the
// distinct forms pass FormCharsMax UTF-16 units (RUN_ENV_FORM_CHARS_MAX in
// agent/src/run-env-forms.ts). Sizing that cap for the per-scope and history limits would
// need about 450M units, so the store enforces a smaller bound at write time instead: a
// repo's secret value bytes may not pass MaxClaimSecretBytes, so a store filled to its
// own limits never produces a claim that fails every run.
//
// FormCharsPerSecretByte is an analytic upper bound of the form units one UTF-8 byte of
// a secret value can add. Worst case is a one-byte control character, which JSON and
// the HTML-safe encoders write as a six-unit \uXXXX escape. For a value of N bytes
// (its UTF-16 length is at most N):
//
//	per value variant (up to 4: as given, trailing breaks stripped, LF-normalized, both):
//	  raw                                  N
//	  JSON, JSON with \/, HTML-safe x2    4 x 6N = 24N
//	  percent-encoded, lower-case, `+`    3 x 3N =  9N
//	  base64: 3 standard + 3 URL-safe     6 x 4N/3 = 8N   (each interior is at most 4N/3 units)
//	  subtotal                            42N, so 4 variants 168N
//	line forms of a multi-line value (raw and JSON-escaped line, lines total at most N)
//	                                      N + 6N = 7N
//	diff-shaped forms (6: LF and CRLF joins for each of the prefixes +, -, space): a line
//	  break byte adds at most 3 units (CRLF plus prefix), so each form is at most 3N
//	                                      6 x 3N = 18N
//	total                                 168N + 7N + 18N = 193N
//
// The forms are counted distinct, so duplicates only lower the count, and a form shorter
// than the 8-unit secret floor is dropped. No rounding term is positive (each base64
// interior is at most 4N/3), so the bound needs no per-value constant. The worker's check
// is "count > FormCharsMax", so a claim of at most MaxClaimSecretBytes secret bytes counts
// at most FormCharsPerSecretByte*MaxClaimSecretBytes <= FormCharsMax and never fails it.
// fixtures/run-env/form-budget.json carries the same numbers, and the agent test
// run-env-forms-budget.test.ts proves the bound against the worker's own form builder.
const (
	FormCharsMax           = 40_000_000
	FormCharsPerSecretByte = 193

	// MaxClaimSecretBytes is the most secret value bytes (every live, disabled, pending
	// and retired secret row of the repo and the user scope) a repo may hold.
	MaxClaimSecretBytes = FormCharsMax / FormCharsPerSecretByte
)
