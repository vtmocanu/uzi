package recovery

import (
	"strconv"

	"github.com/google/uuid"
)

// chunkPlaintextSize is the target plaintext size of one archive chunk (~1 MiB, D4).
// The server splits the streamed bundle into chunks of this many plaintext bytes; the
// last chunk may be shorter. Each chunk is sealed independently with SealWithAAD, so a
// download never buffers the whole bundle and one bounded read/seal step is the unit of
// work.
const chunkPlaintextSize = 1 << 20

// chunkAAD is the additional authenticated data bound into a chunk's AEAD tag (D4).
// It is domain-separated ("recovery_chunk|") and binds three facts that together defeat
// reorder, cross-capture substitution and splice:
//
//   - captureID: the PER-CAPTURE identity (NOT a content digest), so a sealed chunk
//     from one capture cannot be served under another capture's id.
//   - index: the chunk's position, so swapping two rows fails the tag check.
//   - length: the plaintext byte count, so a truncated/padded ciphertext fails.
//
// The same AAD must be supplied at OpenWithAAD; any mismatch is an integrity error, not
// a silently-served corrupt byte. The value is deterministic ASCII, safe to build from
// escapes at any time.
func chunkAAD(captureID uuid.UUID, index int, length int) []byte {
	return []byte("recovery_chunk|" + captureID.String() + "|" +
		strconv.Itoa(index) + "|" + strconv.Itoa(length))
}
