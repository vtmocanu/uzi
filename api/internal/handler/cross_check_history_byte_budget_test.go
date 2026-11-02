package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCrossCheckHistoryWriterByteBudget(t *testing.T) {
	// {"value":""} plus the writer's newline consumes 13 encoded bytes.
	const envelopeBytes = 13
	for _, encoding := range []struct {
		name  string
		value string
		width int
	}{
		{name: "ASCII", value: "x", width: 1},
		{name: "HTML escaped", value: "<", width: 6},
	} {
		t.Run(encoding.name, func(t *testing.T) {
			for _, boundary := range []struct {
				name   string
				offset int
			}{
				{name: "below", offset: -1},
				{name: "at", offset: 0},
				{name: "above", offset: 1},
			} {
				t.Run(boundary.name, func(t *testing.T) {
					target := crossCheckHistoryEnvelopeLimit + boundary.offset
					payloadBytes := target - envelopeBytes
					// ASCII padding fills the remainder after six-byte HTML escapes,
					// so every case reaches its exact target rather than rounding down.
					value := strings.Repeat(encoding.value, payloadBytes/encoding.width) +
						strings.Repeat("x", payloadBytes%encoding.width)
					payload := map[string]string{"value": value}
					encoded, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					if len(encoded)+1 != target {
						t.Fatalf("fixture encoded bytes including newline=%d want %d", len(encoded)+1, target)
					}
					rec := httptest.NewRecorder()
					writeCrossCheckHistory(rec, payload)
					raw := rec.Body.Bytes()
					if boundary.offset > 0 {
						if rec.Code != http.StatusInternalServerError || len(raw) == 0 || len(raw) > 1024 {
							t.Fatalf("oversize response: status=%d encoded bytes=%d, want 500 and small error", rec.Code, len(raw))
						}
						if !json.Valid(raw) {
							t.Fatal("oversize error is not JSON")
						}
						return
					}
					if rec.Code != http.StatusOK || len(raw) != target || len(raw) > crossCheckHistoryEnvelopeLimit {
						t.Fatalf("boundary response: status=%d encoded bytes=%d want 200 and %d bytes", rec.Code, len(raw), target)
					}
					if raw[len(raw)-1] != '\n' || !json.Valid(raw) {
						t.Fatal("success envelope must be JSON followed by a newline")
					}
					if encoding.width == 6 && (!bytes.Contains(raw, []byte(`\u003c`)) || bytes.Contains(raw, []byte("<"))) {
						t.Fatal("success envelope did not use six-byte HTML escapes")
					}
					var decoded map[string]string
					if err := json.Unmarshal(raw, &decoded); err != nil {
						t.Fatal(err)
					}
					if len(decoded) != 1 || decoded["value"] != value {
						t.Fatal("writer changed the bounded payload")
					}
				})
			}
		})
	}
}
