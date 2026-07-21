// Package httpx contains small helpers for writing JSON HTTP responses.
package httpx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// maxBodyBytes caps request bodies decoded via DecodeJSON / DecodeJSONLimited
// (1 MiB).
//
// On the worker message route it has a COUNTERPART the worker enforces on itself:
// agent/src/batcher.ts caps a batch's bytes conservatively BELOW this figure
// (PRD #108 M3), so the worker splits before it ever reaches the server's cap and
// the 413 below is a backstop rather than the primary mechanism. Two hardcoded
// constants in two languages will drift; the design makes drift harmless in the
// only direction it can go — the worker's cap being the smaller one means it
// trips first, and a worker that has NOT been updated still gets a truthful 413
// instead of an ambiguous 400.
const maxBodyBytes = 1 << 20

// JSON writes v as a JSON response with the given status code.
func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("encode json response", "error", err)
	}
}

// Error writes a JSON error body: {"error": "<message>"}.
func Error(w http.ResponseWriter, status int, message string) {
	JSON(w, status, map[string]string{"error": message})
}

// DecodeJSON reads and decodes a JSON request body into dst, rejecting unknown
// fields and trailing data.
//
// The io.LimitReader here TRUNCATES SILENTLY: an oversize body is not reported as
// oversize, it is cut short and then fails as malformed JSON. Every caller
// answers that with a generic 400, which is correct for a browser client and
// wrong for a machine client that must decide whether to retry — see
// DecodeJSONLimited.
func DecodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// DecodeJSONLimited is DecodeJSON for a route whose client needs to tell "your
// body was too large" apart from "your body was malformed" (PRD #108 M2). It
// returns a *http.MaxBytesError — matchable with errors.As — instead of
// truncating into a decode failure, so the handler can answer 413 rather than
// folding a third unrelated cause into the same 400.
//
// It exists as a SIBLING rather than as a change to DecodeJSON deliberately.
// DecodeJSON has 57 non-test call sites; leaving it alone makes "no other route's
// behaviour changed" true by construction instead of true because someone checked
// 57 places, and it keeps this diff inside the small-files premise Phase 1 rests
// on.
//
// w is REQUIRED and must be the real ResponseWriter, not nil. Passing nil does
// not panic — MaxBytesReader's exceed path type-asserts w and a nil interface
// simply fails the assertion — but it skips requestTooLarge(), which is what sets
// closeAfterReply and the Connection: close header (net/http server.go:564-570).
// Without it the server tries to keep the connection alive by draining the unread
// body, up to maxPostHandlerReadBytes+1 = 256 KiB + 1 read and discarded
// (server.go:1112,1418) — on every oversize attempt, from a worker that may be
// retrying. The whole point of this function is to make that case cheap.
func DecodeJSONLimited(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
