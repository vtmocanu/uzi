// pr-description-eval-sanitize is an offline JSON adapter for the TS evaluator.
// stdin contains exactly {"fields": <raw PrDescriptionFields>}; stdout contains
// {"fields": <sanitized PrDescriptionFields>, "diagram_rejected": <bool>}.
// Successful fields are internal pipeline data, not terminal-safe diagnostics.
// Failure emits only {"error": <fixed class>} and exits 1. No environment,
// credentials, files, editor, or network clients are used by this command.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

const (
	// 4 MiB accommodates all raw field caps, including six-byte JSON escapes.
	// Both reading and decoding are bounded by this cap, including whitespace.
	maxInputBytes = 4 << 20
	// Layout caps keep sanitized fields below 128 KiB, even JSON-escaped.
	maxOutputBytes = 128 << 10
)

type request struct {
	Fields *apitypes.PrDescriptionFields `json:"fields"`
}

type response struct {
	Fields          apitypes.PrDescriptionFields `json:"fields"`
	DiagramRejected bool                         `json:"diagram_rejected"`
}

func main() {
	os.Exit(run(os.Stdin, os.Stdout))
}

// run processes one request with no retries. A failure never publishes partial
// sanitized fields. A failed stdout write terminates without a second write.
func run(input io.Reader, output io.Writer) int {
	data, class := sanitize(input)
	if class != "" {
		data, _ = json.Marshal(struct {
			Error string `json:"error"`
		}{Error: class})
	}
	data = append(data, '\n')
	n, err := output.Write(data)
	if err != nil || n != len(data) || class != "" {
		return 1
	}
	return 0
}

func sanitize(input io.Reader) ([]byte, string) {
	raw, err := io.ReadAll(io.LimitReader(input, maxInputBytes+1))
	if err != nil {
		return nil, "input_error"
	}
	if len(raw) > maxInputBytes {
		return nil, "input_too_large"
	}
	if !utf8.Valid(raw) {
		return nil, "invalid_json"
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var in request
	if err := decoder.Decode(&in); err != nil || in.Fields == nil {
		return nil, "invalid_json"
	}
	// Only JSON whitespace may follow the single request, never another value.
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, "invalid_json"
	}
	fields, err := workersvc.SanitizePrDescriptionFields(context.Background(), *in.Fields)
	if err != nil {
		return nil, "api_rejected"
	}
	data, err := json.Marshal(response{
		Fields:          fields,
		DiagramRejected: in.Fields.Diagram != nil && fields.Diagram == nil,
	})
	if err != nil {
		return nil, "output_error"
	}
	if len(data)+1 > maxOutputBytes {
		return nil, "output_too_large"
	}
	return data, ""
}
