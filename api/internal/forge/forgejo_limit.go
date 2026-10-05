package forge

import (
	"errors"
	"io"
	"net/http"
)

// forgejoMaxResponseBytes caps how many body bytes a successful Forgejo response
// may deliver through the driver's client. The gitea SDK buffers bodies with
// io.ReadAll (2xx and non-2xx alike), so without a cap an allowlisted hostile
// forge could stream an unbounded body into api memory. It must stay above the
// largest caller-side limit (maxTraceBytes+1 in rawGetLimited). A package var so
// tests can lower it, like maxForgePages.
var forgejoMaxResponseBytes int64 = 32 << 20

// forgejoMaxErrorResponseBytes bounds error bodies before redaction. Overflow
// discards partial content so a token split at the boundary cannot leak.
const forgejoMaxErrorResponseBytes int64 = 4 << 10

// errForgeResponseTooLarge is returned by a capped body reader once the cap is
// exceeded. The read fails closed; a truncated body is never handed on as if it
// were complete.
var errForgeResponseTooLarge = errors.New("forgejo: response body exceeds size limit")

// cappedTransport wraps a RoundTripper and bounds every response body.
type cappedTransport struct {
	base http.RoundTripper
}

func (t cappedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	limit := forgejoMaxResponseBytes
	if resp.StatusCode/100 != 2 {
		limit = min(limit, forgejoMaxErrorResponseBytes)
	}
	resp.Body = &cappedBody{rc: resp.Body, remaining: limit}
	return resp, nil
}

type cappedBody struct {
	rc        io.ReadCloser
	remaining int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, errForgeResponseTooLarge
	}
	// Read one byte past the allowance so an over-cap body is detected rather
	// than silently cut at exactly the cap.
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		// Report only the in-cap bytes alongside the failure.
		return n + int(b.remaining), errForgeResponseTooLarge
	}
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }
