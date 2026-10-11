package apiclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func pollFixture(n int, token string) []byte {
	workers := make([]map[string]any, n)
	for i := range workers {
		workers[i] = map[string]any{"id": fmt.Sprintf("00000000-0000-0000-0000-%012x", i+1), "join_token": token}
	}
	b, _ := json.Marshal(map[string]any{"workers": workers})
	return b
}

func TestBoundedPollRejectsWholeSnapshot(t *testing.T) {
	secret := "join-token-plaintext-fixture"
	valid := pollFixture(1, secret)
	duplicate := []byte(fmt.Sprintf("{\"workers\":[%s,%s]}", valid[len("{\"workers\":["):len(valid)-2], valid[len("{\"workers\":["):len(valid)-2]))
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"too_many_workers", pollFixture(maxPollWorkers+1, secret)},
		{"duplicate_ids", duplicate},
		{"oversized_bytes", []byte("{\"workers\":[],\"ignored\":\"" + strings.Repeat("x", maxPollBytes) + "\"}")},
		{"long_field", pollFixture(1, strings.Repeat("x", 4097))},
		{"trailing_json", append(append([]byte{}, valid...), []byte("{}")...)},
		{"truncated", valid[:len(valid)-1]},
		{"missing_workers", []byte("{}")},
		{"duplicate_workers_key", []byte("{\"workers\":[],\"workers\":[]}")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{Body: io.NopCloser(bytes.NewReader(tc.body)), Header: make(http.Header), ContentLength: 1}
			got, err := decodePoll(resp)
			if err == nil || got.Workers != nil {
				t.Fatalf("rejected snapshot must have no desired state: workers=%d err=%v", len(got.Workers), err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("error exposed join token")
			}
		})
	}
	resp := &http.Response{Body: io.NopCloser(bytes.NewReader(pollFixture(maxPollWorkers, secret))), Header: make(http.Header)}
	got, err := decodePoll(resp)
	if err != nil || len(got.Workers) != maxPollWorkers {
		t.Fatalf("supported fleet rejected: workers=%d err=%v", len(got.Workers), err)
	}
}

func TestBoundedPollDecompressionAndChunking(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprint(oversized), func(t *testing.T) {
			body := pollFixture(1, "token-fixture")
			if oversized {
				body = []byte("{\"workers\":[],\"ignored\":\"" + strings.Repeat("x", maxPollBytes) + "\"}")
			}
			var encoded bytes.Buffer
			gz := gzip.NewWriter(&encoded)
			if _, err := gz.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			// Exercise explicit compressed responses and the HTTP transport's
			// automatic decompression of a chunked response independently.
			resp := &http.Response{Body: io.NopCloser(bytes.NewReader(encoded.Bytes())), Header: http.Header{"Content-Encoding": {"gzip"}}, ContentLength: 1}
			got, err := decodePoll(resp)
			if oversized {
				if err == nil || got.Workers != nil {
					t.Fatal("compressed byte overflow accepted")
				}
			} else if err != nil || len(got.Workers) != 1 {
				t.Fatalf("gzip decode: %v", err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				_, _ = w.Write(encoded.Bytes())
			}))
			defer srv.Close()
			got, err = New(srv.URL, "controller-fixture", 5*time.Second, nil, testLogger()).Poll(context.Background())
			if oversized {
				if err == nil || got.Workers != nil {
					t.Fatal("chunked decompressed overflow accepted")
				}
			} else if err != nil || len(got.Workers) != 1 {
				t.Fatalf("chunked response: %v", err)
			}
		})
	}
}

func TestBoundedPollEvidenceSetsBeforeTypedAllocation(t *testing.T) {
	caps := make([]string, 65)
	for i := range caps {
		caps[i] = fmt.Sprintf("cap_%d", i)
	}
	body, err := json.Marshal(map[string]any{"workers": []any{map[string]any{
		"id":            "11111111-1111-1111-1111-111111111111",
		"roll_evidence": map[string]any{"scheduling_capabilities": caps, "protocol_capabilities": []string{}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if boundedWorker(body[len("{\"workers\":[") : len(body)-2]) {
		t.Fatal("oversized evidence set passed pre-materialization bound")
	}
}

func TestBoundedPollAllocationBudget(t *testing.T) {
	// Near the byte ceiling with the maximum worker count and long strings.
	// Total allocation bounds peak allocation from this decode conservatively.
	var nearLimit map[string][]map[string]any
	if err := json.Unmarshal(pollFixture(maxPollWorkers, strings.Repeat("x", 4000)), &nearLimit); err != nil {
		t.Fatal(err)
	}
	for _, worker := range nearLimit["workers"] {
		worker["template"] = strings.Repeat("y", 4000)
	}
	payload, err := json.Marshal(nearLimit)
	if err != nil || len(payload) > maxPollBytes {
		t.Fatalf("near-limit fixture: bytes=%d err=%v", len(payload), err)
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := decodePoll(&http.Response{Body: io.NopCloser(bytes.NewReader(payload)), Header: make(http.Header)})
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(got)
	if err != nil || len(got.Workers) != maxPollWorkers {
		t.Fatalf("budget fixture: %v", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 128<<20 {
		t.Fatalf("decode allocated %d bytes; budget is 128 MiB within 256 MiB controller limit", allocated)
	} else {
		t.Logf("decoded %d workers (%d bytes) allocating %d bytes", len(got.Workers), len(payload), allocated)
	}
}

func TestPollErrorResponseDoesNotExposePayload(t *testing.T) {
	secret := "join-token-error-fixture"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, secret, http.StatusInternalServerError)
	}))
	defer srv.Close()
	_, err := New(srv.URL, "controller-fixture", time.Second, nil, testLogger()).Poll(context.Background())
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "500") {
		t.Fatalf("unsafe or missing status error: %v", err)
	}
}

func TestImageRollDrainReasonTransport(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["reason"] != "image_roll" {
			t.Errorf("drain provenance: %v %v", body, err)
		}
		methods = append(methods, r.Method)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	client := New(srv.URL, "controller-fixture", time.Second, nil, testLogger())
	if err := client.RequestDrainWithReason(context.Background(), "11111111-1111-1111-1111-111111111111", "image_roll"); err != nil {
		t.Fatal(err)
	}
	if err := client.ClearDrainWithReason(context.Background(), "11111111-1111-1111-1111-111111111111", "image_roll"); err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 || methods[0] != http.MethodPost || methods[1] != http.MethodDelete {
		t.Fatalf("drain methods: %v", methods)
	}
}
