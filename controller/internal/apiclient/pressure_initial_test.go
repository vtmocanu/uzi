package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPressureReasonRequests(t *testing.T) {
	for _, status := range []int{204, 400, 404, 500} {
		for _, method := range []string{http.MethodPost, http.MethodDelete} {
			t.Run(method+http.StatusText(status), func(t *testing.T) {
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					body, _ := io.ReadAll(r.Body)
					if r.Method != method || string(body) != `{"reason":"pressure_recycle"}` || r.Header.Get("Authorization") != "Bearer fixture" {
						t.Errorf("bad request: %s %s", r.Method, body)
					}
					w.WriteHeader(status)
				}))
				defer server.Close()
				client := New(server.URL, "fixture", time.Second, nil, testLogger())
				var err error
				if method == http.MethodPost {
					err = client.RequestDrainWithReason(context.Background(), "w1", "pressure_recycle")
				} else {
					err = client.ClearDrainWithReason(context.Background(), "w1", "pressure_recycle")
				}
				if (err == nil) != (status == 204) || calls != 1 {
					t.Fatalf("status=%d calls=%d err=%v", status, calls, err)
				}
				for _, reasons := range [][]string{{"invalid"}, {"pressure_recycle", "image_roll"}, {"", "pressure_recycle"}} {
					if client.RequestDrainWithReason(context.Background(), "w1", reasons...) == nil || client.ClearDrainWithReason(context.Background(), "w1", reasons...) == nil {
						t.Fatal("invalid reason accepted")
					}
				}
			})
		}
	}
}

func TestClearDrainNotFoundCompatibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	client := New(server.URL, "fixture", time.Second, nil, testLogger())
	for _, reason := range []string{"", "image_roll", "pressure_recycle"} {
		t.Run(reason, func(t *testing.T) {
			err := client.ClearDrainWithReason(context.Background(), "w1", reason)
			if (err != nil) != (reason == "pressure_recycle") {
				t.Fatalf("reason=%q err=%v", reason, err)
			}
		})
	}
	if err := client.ClearDrain(context.Background(), "w1"); err != nil {
		t.Fatalf("legacy clear: %v", err)
	}
}

func TestNeverRegisteredPollDecode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "internal", "hostedsvc", "testdata", "controller_poll_wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"missing", "null", "true", "false", "string", "number", "object"} {
		t.Run(mode, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			evidence := body["workers"].([]any)[0].(map[string]any)["roll_evidence"].(map[string]any)
			delete(evidence, "never_registered")
			switch mode {
			case "null":
				evidence["never_registered"] = nil
			case "true":
				evidence["never_registered"] = true
			case "false":
				evidence["never_registered"] = false
			case "string":
				evidence["never_registered"] = "true"
			case "number":
				evidence["never_registered"] = 1
			case "object":
				evidence["never_registered"] = map[string]any{}
			}
			mutated, _ := json.Marshal(body)
			got, err := decodePoll(&http.Response{Body: io.NopCloser(bytes.NewReader(mutated)), Header: make(http.Header)})
			if mode == "string" || mode == "number" || mode == "object" {
				if err == nil || got.Workers != nil {
					t.Fatal("bad type accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			p := got.Workers[0].RollEvidence.NeverRegistered
			if mode == "missing" || mode == "null" {
				if p != nil {
					t.Fatal("unknown defaulted")
				}
			} else if p == nil || *p != (mode == "true") {
				t.Fatal("positive evidence lost")
			}
		})
	}
}

func TestPressureDrainPollDecode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "api", "internal", "hostedsvc", "testdata", "controller_poll_wire.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, reason := range []string{"pressure_recycle", "image_roll", "unknown"} {
		t.Run(reason, func(t *testing.T) {
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			body["workers"].([]any)[0].(map[string]any)["draining_reason"] = reason
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			got, err := decodePoll(&http.Response{Body: io.NopCloser(bytes.NewReader(encoded)), Header: make(http.Header)})
			if reason == "unknown" {
				if err == nil || got.Workers != nil {
					t.Fatal("unknown drain reason accepted")
				}
				return
			}
			if err != nil || got.Workers[0].DrainingReason == nil || *got.Workers[0].DrainingReason != reason {
				t.Fatalf("known drain reason lost: %v", err)
			}
		})
	}
}
