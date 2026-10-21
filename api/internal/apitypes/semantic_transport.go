package apitypes

// Semantic transport budgets are encoded JSON bytes, including HTML escaping.
// A criterion has at most 27,246 string bytes (64 milestone ID, 2x2048
// text, 8 citations, 8 evidence pairs, and 10,240 maintainer bytes).
// The presentation and approval each repeat the maintainer metadata.
// Conservatively allow 256*(6*27246+4096) criterion bytes plus
// 512*(6*10276+256) for the two extra maintainer copies,
// 1 MiB base64 snapshots (decoded total <=576 KiB), 64 KiB report overhead,
// and 1 MiB metadata: 76,760,064 bytes, below 80 MiB.
// The ordinary RunDTO remainder retains its existing 32 MiB ceiling.
// Approval repeats only maintainer metadata and 36-byte UUIDs. Allowing
// 512 bytes per row for UUID, keys and syntax, plus 64 KiB identity metadata,
// 256*(6*10240+512)+64 KiB =15,925,248 fits 16 MiB.
const (
	SemanticIdentityHeader   = "X-Uzi-Candidate-Identity-Version"
	SemanticIdentityVersion  = "semantic_v1"
	SemanticGateMaxBytes     = 80 << 20
	OwnerRunLegacyMaxBytes   = 32 << 20
	OwnerRunMaxBytes         = SemanticGateMaxBytes + OwnerRunLegacyMaxBytes
	SemanticApprovalMaxBytes = 16 << 20
	RunInputLegacyMaxBytes   = 1 << 20
	SemanticRunInputMaxBytes = SemanticApprovalMaxBytes + RunInputLegacyMaxBytes
)

// A worker state ACK carrying run.completion_pending sets this header. A maximally
// escaped projection stays below CompletionPendingMaxBytes
// (TestSemanticCompletionPendingWireBound), which the worker adds to the ACK's
// ordinary cap only when the header is present. The projection still decodes strictly.
const (
	CompletionPendingHeader   = "X-Uzi-Completion-Pending"
	CompletionPendingVersion  = "v1"
	CompletionPendingMaxBytes = 20 << 20
)
