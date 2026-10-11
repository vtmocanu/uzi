package apiclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestPollCoverageFlagsRequired(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "internal", "hostedsvc", "testdata", "controller_poll_wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"registered", "ordinary_coverage", "checker_coverage"} {
		for _, mode := range []string{"missing", "null", "string"} {
			t.Run(key+"/"+mode, func(t *testing.T) {
				var body map[string]any
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Fatal(err)
				}
				evidence := body["workers"].([]any)[0].(map[string]any)["roll_evidence"].(map[string]any)
				switch mode {
				case "missing":
					delete(evidence, key)
				case "null":
					evidence[key] = nil
				case "string":
					evidence[key] = "false"
				}
				mutated, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				got, err := decodePoll(&http.Response{Body: io.NopCloser(bytes.NewReader(mutated)), Header: make(http.Header)})
				if err == nil || got.Workers != nil {
					t.Fatal("incomplete coverage evidence admitted")
				}
			})
		}
	}
}
