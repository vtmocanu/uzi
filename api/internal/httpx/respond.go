// Package httpx contains small helpers for writing JSON HTTP responses.
package httpx

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// maxBodyBytes caps request bodies decoded via DecodeJSON (1 MiB).
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
func DecodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}
