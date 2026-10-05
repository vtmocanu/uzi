package kube

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestMaintenanceTransportActualClient(t *testing.T) {
	for _, tc := range []struct {
		name       string
		size       int
		compressed bool
		marked     bool
		wantErr    bool
	}{
		{"ordinary", 16, false, true, false},
		{"exact cap", (8 << 20) - len(`{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[],"padding":""}`), false, true, false},
		{"chunked oversized", 8 << 20, false, true, true},
		{"gzip oversized", 8 << 20, true, true, true},
		{"legacy unchanged", 8 << 20, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				var out io.Writer = w
				if tc.compressed {
					w.Header().Set("Content-Encoding", "gzip")
					zw := gzip.NewWriter(w)
					defer func() {
						if err := zw.Close(); err != nil {
							t.Errorf("close gzip response: %v", err)
						}
					}()
					out = zw
				} else {
					w.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(out, `{"apiVersion":"v1","kind":"PodList","metadata":{},"items":[],"padding":"`+strings.Repeat("x", tc.size)+`"}`)
			}))
			defer server.Close()
			cfg := &rest.Config{Host: server.URL}
			cfg.Wrap(WrapMaintenanceReads)
			client, err := kubernetes.NewForConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.marked {
				ctx = markMaintenanceRead(ctx)
			}
			_, err = client.CoreV1().Pods("workers").List(ctx, metav1.ListOptions{})
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

type maintenanceTestRoundTripper func(*http.Request) (*http.Response, error)

func (f maintenanceTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type maintenanceTestBody struct {
	io.Reader
	closed bool
}

func (b *maintenanceTestBody) Close() error { b.closed = true; return nil }

func TestMaintenanceTransportLyingLengthAndClose(t *testing.T) {
	body := &maintenanceTestBody{Reader: strings.NewReader(strings.Repeat("x", (8<<20)+1))}
	transportErr := errors.New("transport sentinel")
	transport := WrapMaintenanceReads(maintenanceTestRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{Body: body, ContentLength: 1}, transportErr
	}))
	req, err := http.NewRequestWithContext(markMaintenanceRead(context.Background()), "GET", "http://example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if !errors.Is(err, transportErr) {
		t.Fatalf("transport error lost: %v", err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("actual bytes not capped: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !body.closed {
		t.Fatal("underlying body not closed")
	}
}
