// Package httpx contains small helpers for writing JSON HTTP responses.
package httpx

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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

// RespondDecodeError writes the correct HTTP error for a DecodeJSONLimited failure.
// An oversize body (*http.MaxBytesError, matchable via errors.As) gets a truthful
// 413 with uzi's OWN prose — never err.Error(), which is net/http's fixed literal
// "http: request body too large" (pinned by Hyrum's law; echoing it couples our
// wire contract to a stdlib constant). Any other decode failure (malformed JSON,
// unknown field, EOF handling stays the caller's) gets the caller's 400 fallbackMsg.
func RespondDecodeError(w http.ResponseWriter, err error, fallbackMsg string) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		Error(w, http.StatusRequestEntityTooLarge, "the request body is too large")
		return
	}
	Error(w, http.StatusBadRequest, fallbackMsg)
}

// PathUUID parses the named chi URL param as a UUID. On failure it writes a 400
// with body {"error":"invalid <label> id"} and returns ok=false; the caller just
// returns. On success it returns the parsed UUID and ok=true.
func PathUUID(w http.ResponseWriter, r *http.Request, param, label string) (uuid.UUID, bool) {
	return PathUUIDMsg(w, r, param, "invalid "+label+" id")
}

// PathUUIDMsg is PathUUID with a caller-supplied error message, for the few sites
// whose 400 message does not fit the "invalid <label> id" template. On failure it
// writes a 400 with body {"error":"<message>"} and returns ok=false.
func PathUUIDMsg(w http.ResponseWriter, r *http.Request, param, message string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		Error(w, http.StatusBadRequest, message)
		return uuid.Nil, false
	}
	return id, true
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
// DecodeJSON is the decoder for nearly every other JSON route in the tree; leaving
// it alone makes "no other route's behaviour changed" true by construction instead
// of true because someone re-checked every one of its call sites, and it keeps this
// diff inside the small-files premise Phase 1 rests on. (Deliberately no count here:
// a tally drifts exactly like a line number — cite the shape, not the number.)
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
