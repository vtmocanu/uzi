package main

// Bounds on every untrusted-input and evidence-output surface. Each is a hard
// cap the control loop enforces: an oversized control line is abnormal, evidence
// is both length- and count-bounded, and the /proc walk is depth/breadth bounded
// so a hostile descendant tree cannot make the supervisor allocate without limit.
const (
	// maxControlLine is the largest accepted control frame (fd 3). A longer
	// line is treated as abnormal, matching the M0 8192-byte ceiling.
	maxControlLine = 8192

	// maxEvidenceBytes bounds a single evidence line (fd 4).
	maxEvidenceBytes = 65536

	// maxEvidenceLines caps total evidence lines emitted over the run.
	maxEvidenceLines = 256

	// maxSnapshotDescendants bounds the recursive /proc descendant walk so a
	// snapshot cannot be turned into an unbounded traversal.
	maxSnapshotDescendants = 200

	// defaultDisposeTimeoutMs is used when a dispose omits timeoutMs and for the
	// best-effort drain that accompanies an abnormal outcome.
	defaultDisposeTimeoutMs = 2000

	// maxDisposeTimeoutMs is the bounded ceiling a requested dispose timeout is
	// clamped to (no fixed lifetime; only bounded per-op deadlines).
	maxDisposeTimeoutMs = 10000
)

// Control op names carried on fd 3.
const (
	opSnapshot = "snapshot"
	opDispose  = "dispose"
)
