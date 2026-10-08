package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

type boundaryBody struct {
	data       []byte
	chunk      int
	boundaries []int
	closes     int
	terminal   error
	closeErr   error
}

func (b *boundaryBody) Read(p []byte) (int, error) {
	n := min(len(p), len(b.data), b.chunk)
	copy(p, b.data[:n])
	b.data = b.data[n:]
	b.boundaries = append(b.boundaries, n)
	if len(b.data) == 0 {
		return n, b.terminal
	}
	return n, nil
}
func (b *boundaryBody) Close() error { b.closes++; return b.closeErr }

type finalizationFixture struct {
	body   io.ReadCloser
	status int
}

func (f finalizationFixture) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		var wire bytes.Buffer
		enc := pktline.NewEncoder(&wire)
		_ = enc.EncodeString("# service=git-receive-pack\n")
		_ = enc.Flush()
		ar := packp.NewAdvRefs()
		_ = ar.Capabilities.Set(capability.ReportStatus)
		_ = ar.Encode(&wire)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(&wire)}, nil
	}
	return &http.Response{StatusCode: f.status, Header: make(http.Header), Body: f.body}, nil
}

func realFinalizationSession(t *testing.T, body io.ReadCloser, status int) (forwardPackResult, *rawReport, error) {
	t.Helper()
	ep, _ := transport.NewEndpoint("https://example.invalid/origin.git")
	raw := &rawReport{ref: "refs/uzi-checkpoints/main"}
	copied := *brokerHTTPClient
	copied.Transport = &rawReportRoundTripper{original: finalizationFixture{body, status}, report: raw}
	sess, err := githttp.NewClient(&copied).NewReceivePackSession(ep, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	ar, err := sess.AdvertisedReferencesContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	req.Commands = []*packp.Command{{Name: plumbing.ReferenceName(raw.ref), New: plumbing.NewHash(strings.Repeat("1", 40))}}
	result, err := receiveObservedPack(context.Background(), sess, req, raw)
	return result, raw, err
}

func serveFinalizationAdvertisement(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	enc := pktline.NewEncoder(w)
	if err := enc.EncodeString("# service=git-receive-pack\n"); err != nil {
		t.Error(err)
		return
	}
	if err := enc.Flush(); err != nil {
		t.Error(err)
		return
	}
	ar := packp.NewAdvRefs()
	_ = ar.Capabilities.Set(capability.ReportStatus)
	if err := ar.Encode(w); err != nil {
		t.Error(err)
	}
}

func realServerFinalization(t *testing.T, ctx context.Context, url string) (forwardPackResult, *rawReport, error) {
	t.Helper()
	ep, _ := transport.NewEndpoint(url)
	raw := &rawReport{ref: "refs/uzi-checkpoints/main"}
	c, err := rawHTTPTransport(ep, raw)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := c.NewReceivePackSession(ep, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Close() }()
	ar, err := sess.AdvertisedReferencesContext(ctx)
	if err != nil {
		return forwardPackResult{}, raw, err
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
	req.Commands = []*packp.Command{{Name: plumbing.ReferenceName(raw.ref), New: plumbing.NewHash(strings.Repeat("1", 40))}}
	result, err := receiveObservedPack(ctx, sess, req, raw)
	return result, raw, err
}

func TestHTTPInvocationCanonicalDiscovery(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/origin.git/info/refs":
			http.Redirect(w, r, "/canonical.git/info/refs?service=git-receive-pack", http.StatusFound)
		case "/canonical.git/info/refs":
			serveFinalizationAdvertisement(t, w)
		case "/canonical.git/git-receive-pack":
			posts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			_, _ = w.Write(rawFixtureWire(t, "unpack ok", "ok refs/uzi-checkpoints/main"))
		default:
			t.Errorf("unexpected canonical request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, raw, err := realServerFinalization(t, ctx, server.URL+"/origin.git")
	if err != nil || !result.success || !raw.attached || posts.Load() != 1 {
		t.Fatalf("canonical attachment: result=%+v raw=%+v error=%v posts=%d", result, raw, err, posts.Load())
	}
}

func TestHTTPInvocationRedirectRefusal(t *testing.T) {
	for _, discovery := range []bool{true, false} {
		t.Run(fmt.Sprint(discovery), func(t *testing.T) {
			var forbidden atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbidden.Add(1); serveFinalizationAdvertisement(t, w) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !discovery && r.Method == http.MethodGet {
					serveFinalizationAdvertisement(t, w)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				http.Redirect(w, r, target.URL+"/origin.git/info/refs?service=git-receive-pack", http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result, _, err := realServerFinalization(t, ctx, server.URL+"/origin.git")
			if err == nil || result.success || result.rejected || forbidden.Load() != 0 {
				t.Fatalf("redirect refusal: result=%+v error=%v prohibited=%d", result, err, forbidden.Load())
			}
		})
	}
}

func TestHTTPInvocationSameOriginPOSTRefusal(t *testing.T) {
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			serveFinalizationAdvertisement(t, w)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/redirected.git/git-receive-pack" {
			redirected.Add(1)
			_, _ = w.Write(rawFixtureWire(t, "unpack ok", "ok refs/uzi-checkpoints/main"))
			return
		}
		http.Redirect(w, r, "/redirected.git/git-receive-pack", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, _, err := realServerFinalization(t, ctx, server.URL+"/origin.git")
	if err == nil || result.success || result.rejected || redirected.Load() != 0 {
		t.Fatalf("same-origin POST redirect followed: result=%+v error=%v requests=%d", result, err, redirected.Load())
	}
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.cancel()
	}
	return n, err
}

type cancelBodyTransport struct {
	http.RoundTripper
	cancel context.CancelFunc
}

func (c cancelBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := c.RoundTripper.RoundTrip(req)
	if err == nil && req.Method == http.MethodPost {
		res.Body = &cancelBody{ReadCloser: res.Body, cancel: c.cancel}
	}
	return res, err
}

func TestHTTPFinalizationOriginalContext(t *testing.T) {
	for _, deadline := range []bool{true, false} {
		t.Run(fmt.Sprint(deadline), func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					serveFinalizationAdvertisement(t, w)
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = w.Write(rawFixtureWire(t, "unpack ok", "ok refs/uzi-checkpoints/main"))
				w.(http.Flusher).Flush()
				close(started)
				<-r.Context().Done()
				close(stopped)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			if !deadline {
				original := brokerHTTPClient.Transport
				brokerHTTPClient.Transport = cancelBodyTransport{RoundTripper: original, cancel: cancel}
				defer func() { brokerHTTPClient.Transport = original }()
			}
			start := time.Now()
			result, raw, err := realServerFinalization(t, ctx, server.URL+"/origin.git")
			expected := context.DeadlineExceeded
			if !deadline {
				expected = context.Canceled
			}
			if result.success || result.rejected || !errors.Is(err, expected) || raw.finalized || !strings.Contains(err.Error(), "case=deadline") {
				t.Fatalf("context finalization: result=%+v raw=%+v error=%v", result, raw, err)
			}
			if time.Since(start) > 2*time.Second {
				t.Fatal("caller deadline not honored")
			}
			select {
			case <-stopped:
			case <-time.After(2 * time.Second):
				t.Fatal("server handler did not terminate")
			}
		})
	}
}

func TestHTTPInvocationEvidenceIsolation(t *testing.T) {
	for _, tc := range []struct {
		name, method, path string
		mark               int
		attached           bool
	}{
		{"discovery", http.MethodGet, "/origin.git/info/refs", 1, false},
		{"upload", http.MethodPost, "/origin.git/git-upload-pack", 1, false},
		{"unmarked", http.MethodPost, "/origin.git/git-receive-pack", 0, false},
		{"other invocation", http.MethodPost, "/origin.git/git-receive-pack", 2, false},
		{"canonical marked", http.MethodPost, "/canonical.git/git-receive-pack", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := &rawReport{ref: "refs/uzi-checkpoints/main"}
			body := &boundaryBody{data: rawFixtureWire(t, "unpack ok", "ok "+raw.ref), chunk: 3, terminal: io.EOF}
			rt := &rawReportRoundTripper{original: rawFixtureRoundTripper{body: body}, report: raw}
			req, _ := http.NewRequest(tc.method, "https://example.invalid"+tc.path, nil)
			if tc.mark == 1 {
				req = req.WithContext(context.WithValue(req.Context(), reportInvocationKey{}, raw))
			}
			if tc.mark == 2 {
				req = req.WithContext(context.WithValue(req.Context(), reportInvocationKey{}, &rawReport{}))
			}
			res, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			if raw.attached != tc.attached || (body.closes == 1) != tc.attached || (body.boundaries != nil) != tc.attached {
				t.Fatalf("isolation: raw=%+v reads=%v closes=%d", raw, body.boundaries, body.closes)
			}
			_, _ = io.Copy(io.Discard, res.Body)
			_ = res.Body.Close()
			if raw.complete() != tc.attached || body.closes != 1 {
				t.Fatal("replay observed twice or unrelated supplied evidence")
			}
		})
	}
}

type typedReportFailure struct{ text string }

func (e *typedReportFailure) Error() string { return e.text }

func TestHTTPFinalizationErrorIdentity(t *testing.T) {
	cause := &typedReportFailure{text: "https://credential@remote.invalid/\u202e" + "glpat-" + strings.Repeat("D", 20)}
	body := &boundaryBody{data: rawFixtureWire(t, "unpack ok", "ok refs/uzi-checkpoints/main"), chunk: 1024, terminal: cause}
	result, _, err := realFinalizationSession(t, body, 200)
	var got *typedReportFailure
	if result.success || result.rejected || !errors.As(err, &got) || got != cause {
		t.Fatalf("error identity lost: %v", err)
	}
	if strings.Contains(fmt.Sprintf("%#v", err), "remote.invalid") {
		t.Fatal("verbose error leaked cause")
	}
}

func TestHTTPFinalizationEvidence(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	poison := "https://user:credential@remote.invalid/" + ref + "\n\r\x00\u202e" + "glpat-" + strings.Repeat("A", 20)
	cause := errors.New(poison)
	for _, tc := range []struct {
		name               string
		wire               []byte
		terminal, closeErr error
		status             int
		success, rejected  bool
		kind               string
	}{
		{"ok", rawFixtureWire(t, "unpack ok", "ok "+ref), io.EOF, nil, 200, true, false, ""},
		{"ng", rawFixtureWire(t, "unpack ok", "ng "+ref+" ok"), io.EOF, nil, 200, false, true, ""},
		{"unpack rejection", rawFixtureWire(t, "unpack "+poison), io.EOF, nil, 200, false, false, "malformed"},
		{"unpack only rejection", rawFixtureWire(t, "unpack non-fast-forward"), io.EOF, nil, 200, false, true, ""},
		{"empty", nil, io.EOF, nil, 200, false, false, "empty_body"},
		{"no marker", rawFixtureWire(t, "unpack ok"), io.EOF, nil, 200, false, false, "no_command_marker"},
		{"read failure", rawFixtureWire(t, "unpack ok", "ok "+ref), cause, nil, 200, false, false, "read_failure"},
		{"close failure", rawFixtureWire(t, "unpack ok", "ng "+ref+" non-fast-forward"), io.EOF, cause, 200, false, false, "close_failure"},
		{"non200 ok", rawFixtureWire(t, "unpack ok", "ok "+ref), io.EOF, nil, 201, false, false, "http_status"},
		{"non200 ng", rawFixtureWire(t, "unpack ok", "ng "+ref+" non-fast-forward"), io.EOF, nil, 500, false, false, "http_status"},
		{"decode error", rawFixtureWire(t, "unpack ok", "ng "+ref+" "+poison), io.EOF, nil, 200, false, false, "malformed"},
		{"partial header", []byte("00"), io.EOF, nil, 200, false, false, "incomplete"},
		{"partial payload", []byte("000dunp"), io.EOF, nil, 200, false, false, "incomplete"},
		{"exact budget", bytes.Repeat([]byte("x"), maxReportResponseBytes), io.EOF, nil, 200, false, false, "budget"},
		{"budget", bytes.Repeat([]byte("x"), maxReportResponseBytes+1), io.EOF, nil, 200, false, false, "budget"},
		{"report then overbudget tail", append(rawFixtureWire(t, "unpack ok", "ok "+ref), bytes.Repeat([]byte("x"), maxReportResponseBytes+1-len(rawFixtureWire(t, "unpack ok", "ok "+ref)))...), io.EOF, nil, 200, false, false, "budget"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &boundaryBody{data: append([]byte(nil), tc.wire...), chunk: 97, terminal: tc.terminal, closeErr: tc.closeErr}
			result, raw, err := realFinalizationSession(t, body, tc.status)
			if result.success != tc.success || result.rejected != tc.rejected || body.closes != 1 {
				t.Fatalf("result=%+v closes=%d error=%v", result, body.closes, err)
			}
			if tc.name == "exact budget" && (raw.eof || len(body.data) != 0) {
				t.Fatal("exact boundary must stay unknown without EOF probe")
			}
			if tc.kind != "" && (err == nil || !strings.Contains(err.Error(), "case="+tc.kind)) {
				t.Fatalf("missing diagnostic %s: %v", tc.kind, err)
			}
			if tc.terminal == cause || tc.closeErr == cause {
				if !errors.Is(err, cause) {
					t.Fatal("lost cause identity")
				}
			}
			if tc.name == "report then overbudget tail" {
				if raw.bytes != maxReportResponseBytes || raw.marker != "ok" || !raw.flushed || raw.eof || !raw.exhausted || len(body.data) != 1 {
					t.Fatalf("finalization budget: raw=%+v remaining=%d", raw, len(body.data))
				}
				var expected []int
				for remaining := maxReportResponseBytes; remaining > 0; {
					n := min(remaining, 97)
					expected = append(expected, n)
					remaining -= n
				}
				if !reflect.DeepEqual(body.boundaries, expected) {
					t.Fatalf("budget read sequence: actual=%v want=%v", body.boundaries, expected)
				}
			}
			if raw.bytes > maxReportResponseBytes || (tc.name == "budget" && len(body.data) != 1) {
				t.Fatalf("read beyond cap: bytes=%d remaining=%d", raw.bytes, len(body.data))
			}
			if err != nil {
				var logged bytes.Buffer
				log.New(&logged, "", 0).Print(err)
				for _, text := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err), logged.String()} {
					for _, secret := range []string{"remote.invalid", "credential", ref, "glpat-", "\u202e", "\r", "\x00", poison} {
						if strings.Contains(text, secret) {
							t.Fatalf("unsafe rendered error: %q", text)
						}
					}
					if len(text) > 1200 {
						t.Fatalf("unbounded error: %d", len(text))
					}
				}
			}
		})
	}
}

func TestHTTPFinalizationActualBoundaries(t *testing.T) {
	report := rawFixtureWire(t, "unpack ok", "ok refs/uzi-checkpoints/main")
	flush := len(report)
	var contradictory bytes.Buffer
	if err := pktline.NewEncoder(&contradictory).EncodeString("ng refs/uzi-checkpoints/main non-fast-forward\n"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		tail []byte
		kind string
	}{
		{"valid report", nil, ""},
		{"trailing flush", []byte("0000"), "malformed"},
		{"partial header 1", []byte("0"), "incomplete"},
		{"partial header 2", []byte("00"), "incomplete"},
		{"partial header 3", []byte("000"), "incomplete"},
		{"contradictory status after flush", contradictory.Bytes(), "malformed"},
		{"invalid packet tail", []byte("0005x"), "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := append(append([]byte(nil), report...), tc.tail...)
			for _, schedule := range []struct {
				name  string
				chunk int
			}{
				{"one byte", 1},
				{"exact flush", flush},
				{"flush+1", flush + 1},
				{"flush+2", flush + 2},
				{"flush+3", flush + 3},
				{"coalesced", len(wire)},
			} {
				t.Run(schedule.name, func(t *testing.T) {
					body := &boundaryBody{data: append([]byte(nil), wire...), chunk: schedule.chunk, terminal: io.EOF}
					result, _, err := realFinalizationSession(t, body, 200)
					t.Logf("actual read boundaries=%v report flush=%d result=%+v error=%v", body.boundaries, flush, result, err)
					if !result.invoked || result.success != (tc.kind == "") || result.rejected || (err == nil) != (tc.kind == "") {
						t.Fatalf("unexpected disposition: result=%+v error=%v", result, err)
					}
					if tc.kind != "" && !strings.Contains(err.Error(), "case="+tc.kind) {
						t.Fatalf("missing diagnostic %s: %v", tc.kind, err)
					}
					if body.closes != 1 {
						t.Fatalf("original closes=%d", body.closes)
					}
					var expected []int
					for remaining := len(wire); remaining > 0; {
						n := min(remaining, schedule.chunk)
						expected = append(expected, n)
						remaining -= n
					}
					if !reflect.DeepEqual(body.boundaries, expected) {
						t.Fatalf("body not finalized: actual=%v want=%v", body.boundaries, expected)
					}
				})
			}
		})
	}
}
