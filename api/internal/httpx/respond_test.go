package httpx

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// body wraps a string in a request whose Body is that string. httptest.NewRequest
// gives the *http.Request a real Body, which is what both decoders consume.
func requestWithBody(s string) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(s))
}

// payload is the decode target used across the decoder tests. A single string field
// keeps a valid body small and lets an oversize body be built by padding S.
type payload struct {
	S string `json:"s"`
}

// oversizeJSON returns a well-formed JSON object strictly larger than n bytes, so it
// crosses the cap on its own SIZE rather than on any malformation — a truncated or
// malformed body would exercise the 400 path and prove nothing about the size cap.
func oversizeJSON(t *testing.T, n int) string {
	t.Helper()
	s := `{"s":"` + strings.Repeat("a", n) + `"}`
	if len(s) <= n {
		t.Fatalf("oversize fixture is %d bytes, not over the %d-byte target", len(s), n)
	}
	return s
}

// exactSizeJSON returns a well-formed JSON object of EXACTLY n bytes: {"s":"<pad>"}
// has an 8-byte frame (`{"s":""}`), so the padding is n-8. Used to pin the cap
// boundary against the real maxBodyBytes value.
func exactSizeJSON(t *testing.T, n int) string {
	t.Helper()
	const frame = 8 // len(`{"s":""}`)
	if n < frame {
		t.Fatalf("cannot build a %d-byte JSON object; frame alone is %d bytes", n, frame)
	}
	s := `{"s":"` + strings.Repeat("a", n-frame) + `"}`
	if len(s) != n {
		t.Fatalf("exact fixture is %d bytes, want %d", len(s), n)
	}
	return s
}

func TestDecodeJSONHappyPath(t *testing.T) {
	var dst payload
	if err := DecodeJSON(requestWithBody(`{"s":"hello"}`), &dst); err != nil {
		t.Fatalf("DecodeJSON on a valid body errored: %v", err)
	}
	if dst.S != "hello" {
		t.Fatalf("decoded S = %q, want %q", dst.S, "hello")
	}
}

func TestDecodeJSONLimitedHappyPath(t *testing.T) {
	var dst payload
	rec := httptest.NewRecorder()
	if err := DecodeJSONLimited(rec, requestWithBody(`{"s":"hello"}`), &dst); err != nil {
		t.Fatalf("DecodeJSONLimited on a valid body errored: %v", err)
	}
	if dst.S != "hello" {
		t.Fatalf("decoded S = %q, want %q", dst.S, "hello")
	}
}

func TestDecodeJSONRejectsUnknownFields(t *testing.T) {
	// The decoder is configured with DisallowUnknownFields, so an extra key is a
	// decode error rather than a silently-ignored field.
	var dst payload
	err := DecodeJSON(requestWithBody(`{"s":"hi","extra":1}`), &dst)
	if err == nil {
		t.Fatal("DecodeJSON accepted an unknown field, want an error")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error %q does not name the unknown field", err.Error())
	}
}

func TestDecodeJSONLimitedRejectsUnknownFields(t *testing.T) {
	var dst payload
	rec := httptest.NewRecorder()
	err := DecodeJSONLimited(rec, requestWithBody(`{"s":"hi","extra":1}`), &dst)
	if err == nil {
		t.Fatal("DecodeJSONLimited accepted an unknown field, want an error")
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error %q does not name the unknown field", err.Error())
	}
}

// TestOversizeBodyIsTypedOnLimitedAndTruncatedOnPlain is the flagship: on the SAME
// over-cap body, DecodeJSONLimited returns a matchable *http.MaxBytesError (so a
// handler can answer 413), while DecodeJSON silently truncates into an ordinary
// decode error that is NOT a *http.MaxBytesError (the documented behaviour that is
// the entire reason the two functions exist separately).
func TestOversizeBodyIsTypedOnLimitedAndTruncatedOnPlain(t *testing.T) {
	over := oversizeJSON(t, maxBodyBytes+1024)

	var limitedDst payload
	rec := httptest.NewRecorder()
	limitedErr := DecodeJSONLimited(rec, requestWithBody(over), &limitedDst)
	if limitedErr == nil {
		t.Fatal("DecodeJSONLimited accepted an over-cap body, want an error")
	}
	var maxErr *http.MaxBytesError
	if !errors.As(limitedErr, &maxErr) {
		t.Fatalf("DecodeJSONLimited error is %T (%v), want a *http.MaxBytesError so the caller can answer 413", limitedErr, limitedErr)
	}

	var plainDst payload
	plainErr := DecodeJSON(requestWithBody(over), &plainDst)
	if plainErr == nil {
		t.Fatal("DecodeJSON accepted an over-cap body, want a (truncation) error")
	}
	var plainMax *http.MaxBytesError
	if errors.As(plainErr, &plainMax) {
		t.Fatalf("DecodeJSON returned a *http.MaxBytesError (%v); it must silently truncate into a plain decode error, not a typed size error", plainErr)
	}
}

// TestDecodeJSONLimitedCapBoundary pins the enforced cap to the real maxBodyBytes
// constant: a body of exactly maxBodyBytes bytes is permitted (no size error),
// maxBodyBytes+1 is not.
func TestDecodeJSONLimitedCapBoundary(t *testing.T) {
	// Exactly at the cap: MaxBytesReader permits maxBodyBytes bytes, so a valid body
	// of that size decodes without a size error.
	atCap := exactSizeJSON(t, maxBodyBytes)
	var atDst payload
	atRec := httptest.NewRecorder()
	if err := DecodeJSONLimited(atRec, requestWithBody(atCap), &atDst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			t.Fatalf("a body of exactly maxBodyBytes (%d) tripped the cap: %v", maxBodyBytes, err)
		}
		t.Fatalf("unexpected error decoding an at-cap body: %v", err)
	}

	// One byte over: the cap trips with a typed size error.
	overCap := exactSizeJSON(t, maxBodyBytes+1)
	var overDst payload
	overRec := httptest.NewRecorder()
	err := DecodeJSONLimited(overRec, requestWithBody(overCap), &overDst)
	if err == nil {
		t.Fatalf("a body of maxBodyBytes+1 (%d) did not trip the cap", maxBodyBytes+1)
	}
	var maxErr *http.MaxBytesError
	if !errors.As(err, &maxErr) {
		t.Fatalf("maxBodyBytes+1 error is %T (%v), want *http.MaxBytesError", err, err)
	}
}

func TestJSONWritesStatusBodyAndContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusCreated, payload{S: "ok"})

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"s":"ok"}` {
		t.Fatalf("body = %q, want %q", got, `{"s":"ok"}`)
	}
}

func TestJSONNilWritesStatusWithNoBody(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusNoContent, nil)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", rec.Body.String())
	}
}

func TestErrorWritesErrorEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	Error(rec, http.StatusBadRequest, "bad request")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	// Pin the wire contract: the error envelope is JSON, so the header must say so even
	// if Error ever stops delegating to JSON (CodeRabbit).
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":"bad request"}` {
		t.Fatalf("body = %q, want %q", got, `{"error":"bad request"}`)
	}
}
