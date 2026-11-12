package issuetracker

import (
	"io"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

const (
	// listPageBytes caps one page of a list response.
	listPageBytes int64 = 8 << 20
	// objectBytes caps a single-object response.
	objectBytes int64 = 2 << 20
	// errorBodyBytes caps the body read from an error response.
	errorBodyBytes int64 = 4 << 10
)

// readBounded reads at most limit bytes of r. A longer body returns a
// *forge.ResponseTooLargeError carrying prov, an Outcome classified from the
// HTTP status of the response, and no remote content. Overflow is checked
// before the read error so an oversize stays identifiable. The caller closes
// the body; readBounded does not. The limit counts bytes after transparent
// decompression, which is what r yields, deliberately (forge instead forces
// identity encoding).
func readBounded(r io.Reader, limit int64, prov forge.ResponseProvenance, status int) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(body)) > limit {
		return nil, &forge.ResponseTooLargeError{Provenance: prov, Outcome: classifyStatus(status)}
	}
	if err != nil {
		return nil, err
	}
	return body, nil
}
