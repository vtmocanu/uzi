package forge

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// emptyGzipMember is one valid gzip member that inflates to zero bytes.
func emptyGzipMember(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gzip.NewWriter(&buf).Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	_, _ = io.WriteString(z, s)
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// ciGzipBomb is many empty members followed by one valid small JSON member: it
// needs more than ciRepairCompressedJSONBytes of network input yet inflates to
// a few bytes.
func ciGzipBomb(t *testing.T, provider Type) []byte {
	t.Helper()
	empty := emptyGzipMember(t)
	var buf bytes.Buffer
	for buf.Len() <= ciRepairCompressedJSONBytes+len(empty) {
		buf.Write(empty)
	}
	buf.Write(gzipBytes(t, ciRunsJSON(provider, ciRunJSON(provider, map[bool]string{true: "42", false: " workflow.yml "}[provider == TypeGitHub]))))
	return buf.Bytes()
}

func TestCICompressedGzipMemberBombThroughDrivers(t *testing.T) {
	for _, provider := range ciProviders {
		t.Run(string(provider), func(t *testing.T) {
			bomb := ciGzipBomb(t, provider)
			d := ciWireDriver(t, provider, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				_, _ = w.Write(bomb)
			})
			done := make(chan error, 1)
			go func() {
				runs, err := d.ListWorkflowRuns(WithCIRepairReadLimits(context.Background()), 7, ListWorkflowRunsOptions{Limit: 100})
				if runs != nil && err != nil {
					t.Error("partial result escaped")
				}
				done <- err
			}()
			select {
			case err := <-done:
				requireCIBudget(t, err)
				if !strings.Contains(err.Error(), "compressed JSON bytes") {
					t.Fatalf("reason = %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("deadlock or unbounded read")
			}
		})
	}
}

func ciTransportRT(t *testing.T, ctx context.Context, status http.RoundTripper) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequestWithContext(markCIJSON(ctx), "GET", "https://example.test", nil)
	return (ciRepairTransport{base: status}).RoundTrip(req)
}

func ciFixedResponse(body []byte, enc string) http.RoundTripper {
	return ciRoundTripper(func(*http.Request) (*http.Response, error) {
		h := make(http.Header)
		if enc != "" {
			h.Set("Content-Encoding", enc)
		}
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
}

func TestCITransportRequestsRawGzip(t *testing.T) {
	var seen string
	base := ciRoundTripper(func(r *http.Request) (*http.Response, error) {
		seen = r.Header.Get("Accept-Encoding")
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})
	if _, err := ciTransportRT(t, WithCIRepairReadLimits(context.Background()), base); err != nil || seen != "gzip" {
		t.Fatalf("accept-encoding=%q err=%v", seen, err)
	}
}

func TestCICompressedPerResponseLimitTrips(t *testing.T) {
	empty := emptyGzipMember(t)
	body := bytes.Repeat(empty, ciRepairCompressedJSONBytes/len(empty)+2)
	_, err := ciTransportRT(t, WithCIRepairReadLimits(context.Background()), ciFixedResponse(body, "gzip"))
	requireCIBudget(t, err)
	// identity bodies are counted too: pad is whitespace, within decompressed bounds
	_, err = ciTransportRT(t, WithCIRepairReadLimits(context.Background()), ciFixedResponse(bytes.Repeat([]byte(" "), ciRepairCompressedJSONBytes+1), ""))
	requireCIBudget(t, err)
}

func TestCICompressedBytesCountedForIdentityAndGzip(t *testing.T) {
	ctx := WithCIRepairReadLimits(context.Background())
	b := ciBudget(ctx)
	start := b.compressedRemaining.Load()
	if _, err := ciTransportRT(t, ctx, ciFixedResponse([]byte("abcd"), "")); err != nil {
		t.Fatal(err)
	}
	if got := start - b.compressedRemaining.Load(); got != 4 {
		t.Fatalf("identity charged %d", got)
	}
	gz := gzipBytes(t, `{"a":1}`)
	mid := b.compressedRemaining.Load()
	resp, err := ciTransportRT(t, ctx, ciFixedResponse(gz, "gzip"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	if string(data) != `{"a":1}` {
		t.Fatalf("normal gzip decoded to %q", data)
	}
	if got := mid - b.compressedRemaining.Load(); got != int64(len(gz)) {
		t.Fatalf("gzip charged %d, want %d", got, len(gz))
	}
}

func TestCICompressedPerFireLimitTripsAcrossResponses(t *testing.T) {
	ctx := WithCIRepairReadLimits(context.Background())
	chunk := bytes.Repeat([]byte(" "), ciRepairCompressedJSONBytes)
	for i := 0; i < ciRepairCompressedFireBytes/ciRepairCompressedJSONBytes; i++ {
		resp, err := ciTransportRT(t, ctx, ciFixedResponse(chunk, ""))
		if err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}
	_, err := ciTransportRT(t, ctx, ciFixedResponse([]byte(" "), ""))
	requireCIBudget(t, err)
	// later calls fail fast, including the shared validators
	requireCIBudget(t, ciReadError(ctx, nil))
}

func TestCIDecompressedLimitsStillTrip(t *testing.T) {
	// per response
	_, err := ciTransportRT(t, WithCIRepairReadLimits(context.Background()),
		ciFixedResponse(gzipBytes(t, strings.Repeat(" ", ciRepairJSONBytes+1)), "gzip"))
	requireCIBudget(t, err)
	if !strings.Contains(err.Error(), "decompressed JSON bytes") {
		t.Fatalf("reason = %v", err)
	}
	// per fire
	ctx := WithCIRepairReadLimits(context.Background())
	ciBudget(ctx).remaining = 10
	_, err = ciTransportRT(t, ctx, ciFixedResponse(gzipBytes(t, strings.Repeat(" ", 11)), "gzip"))
	requireCIBudget(t, err)
}
