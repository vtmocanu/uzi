package forge

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Tests for the GitLab GetMergeRequest response ceilings (#3038): 32 MiB for a 2xx
// body, 4 KiB for any other body. They drive the real driver and SDK against
// httptest servers, or against fabricated responses injected through
// forgeTransportOverride (labelled "wrapper-level" where the wire cannot
// produce the shape). They run serially: they lower a package var and swap the
// shared transport override.

const (
	mrTestPath = "/api/v4/projects/7/merge_requests/9"
	fabBase    = "http://gitlab.test"
	fabMRURL   = fabBase + mrTestPath
	fabMRMsg   = "gitlab: get merge request: "
)

// testToken is assembled from fragments so no provider-token-shaped literal sits
// in source; the runtime value is what the redactor sees.
func testToken() string { return "glpat-" + "Zq7xKm" + "Wp3NrT" + "5vBc9Y" + "dLh2" }

// patPrefix is a PAT prefix (not the whole token, so the redactor cannot scrub
// it): it only stays out of an error if the partial body is not passed on.
func patPrefix() string { return testToken()[:12] }

func lowerGitLabLimit(t *testing.T, n int64) {
	t.Helper()
	prev := gitLabMaxResponseBytes
	gitLabMaxResponseBytes = n
	t.Cleanup(func() { gitLabMaxResponseBytes = prev })
}

func zeroBackoff(_, _ time.Duration, _ int, _ *http.Response) time.Duration { return 0 }

func mrJSON(iid, padTo int) []byte {
	b := fmt.Appendf(nil, `{"id":%d,"iid":%d,"state":"merged","web_url":"https://gl/mr/%d"}`, iid+1000, iid, iid)
	if len(b) < padTo {
		b = append(b, bytes.Repeat([]byte(" "), padTo-len(b))...)
	}
	return b
}

// trackedBody counts the bytes the layers above ask of the underlying body and
// whether they closed it.
type trackedBody struct {
	r      io.Reader
	read   atomic.Int64
	closed atomic.Bool
}

func (b *trackedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (b *trackedBody) Close() error {
	b.closed.Store(true)
	if c, ok := b.r.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// bodyLog keeps every tracked response body so a test can assert read totals and
// that each one was closed.
type bodyLog struct {
	mu     sync.Mutex
	bodies []*trackedBody
}

func (l *bodyLog) add(r io.Reader) *trackedBody {
	b := &trackedBody{r: r}
	l.mu.Lock()
	l.bodies = append(l.bodies, b)
	l.mu.Unlock()
	return b
}

func (l *bodyLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.bodies)
}

func (l *bodyLog) totalRead() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for _, b := range l.bodies {
		n += b.read.Load()
	}
	return n
}

func (l *bodyLog) maxRead() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var n int64
	for _, b := range l.bodies {
		n = max(n, b.read.Load())
	}
	return n
}

func (l *bodyLog) allClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, b := range l.bodies {
		if !b.closed.Load() {
			return false
		}
	}
	return len(l.bodies) > 0
}

// trackRT sits at the bottom of the transport chain (the override), so it sees
// exactly the bytes responseLimitTransport and the layers above read.
type trackRT struct {
	next http.RoundTripper
	reqs atomic.Int64
	bodyLog
}

func (t *trackRT) RoundTrip(req *http.Request) (*http.Response, error) {
	t.reqs.Add(1)
	resp, err := t.next.RoundTrip(req)
	if err == nil && resp != nil && resp.Body != nil {
		resp.Body = t.add(resp.Body)
	}
	return resp, err
}

// fabRT fabricates responses (wrapper-level tests). fn receives the 1-based
// attempt number.
type fabRT struct {
	calls atomic.Int64
	fn    func(n int, req *http.Request) (*http.Response, error)
	bodyLog
}

func (f *fabRT) RoundTrip(req *http.Request) (*http.Response, error) {
	n := int(f.calls.Add(1))
	return f.fn(n, req)
}

func (f *fabRT) resp(req *http.Request, code int, body io.Reader) *http.Response {
	return &http.Response{
		StatusCode:    code,
		Status:        fmt.Sprintf("%d fabricated", code),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{},
		Body:          f.add(body),
		ContentLength: -1,
		Request:       req,
	}
}

func installOverride(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	prev := forgeTransportOverride
	forgeTransportOverride = rt
	t.Cleanup(func() { forgeTransportOverride = prev })
}

func newDriverOver(t *testing.T, rt http.RoundTripper, baseURL string, timeout time.Duration) Forge {
	t.Helper()
	installOverride(t, rt)
	d, err := NewWithGitLabBackoff(TypeGitLab, baseURL, testToken(), timeout, zeroBackoff)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// wireMR is a driver pointed at an httptest server through a tracking transport.
type wireMR struct {
	d   Forge
	rt  *trackRT
	srv *httptest.Server
}

func newWireMR(t *testing.T, h http.HandlerFunc, timeout time.Duration) *wireMR {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	rt := &trackRT{next: forgeHTTPTransport}
	return &wireMR{d: newDriverOver(t, rt, srv.URL, timeout), rt: rt, srv: srv}
}

func (w *wireMR) get(ctx context.Context) (MergeRequest, error) {
	return w.d.GetMergeRequest(ctx, 7, 9)
}

// readers used for fabricated bodies

type errAfter struct {
	data []byte
	err  error
}

func (r *errAfter) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// blockAfter yields data, signals onBlock, then blocks until ctx is done.
type blockAfter struct {
	data    []byte
	ctx     context.Context
	onBlock func()
}

func (r *blockAfter) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	if r.onBlock != nil {
		r.onBlock()
	}
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

type fillReader struct{ b byte }

func (r fillReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.b
	}
	return len(p), nil
}

// bigBody is prefix followed by n filler bytes.
func bigBody(prefix []byte, n int64, filler byte) io.Reader {
	return io.MultiReader(bytes.NewReader(prefix), io.LimitReader(fillReader{filler}, n))
}

// tempErr is a Temporary() net error, the shape the SDK retry policy retries.
type tempErr struct{}

func (tempErr) Error() string   { return "temporary read problem" }
func (tempErr) Timeout() bool   { return false }
func (tempErr) Temporary() bool { return true }

func isTooLarge(err error) bool { return errors.Is(err, ErrResponseTooLarge) }

// wantRefusal asserts the typed, fixed, content-free refusal and a zero MR.
func wantRefusal(t *testing.T, mr MergeRequest, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("want the size refusal, got success %+v", mr)
	}
	if !isTooLarge(err) {
		t.Fatalf("error %q is not ErrResponseTooLarge", err.Error())
	}
	if err.Error() != "forge response body exceeds size limit" {
		t.Fatalf("refusal text = %q, want the fixed message", err.Error())
	}
	var typed *ResponseTooLargeError
	if !errors.As(err, &typed) {
		t.Fatalf("error %T is not *ResponseTooLargeError", err)
	}
	if Class(err) != ErrorClassOther {
		t.Fatalf("class = %q, want other", Class(err))
	}
	if mr != (MergeRequest{}) {
		t.Fatalf("a refused response must return the zero MergeRequest, got %+v", mr)
	}
}

func TestGitLabLimitCeilings(t *testing.T) {
	if gitLabMaxResponseBytes != 32<<20 || gitLabMaxErrorResponseBytes != 4<<10 {
		t.Fatalf("ceilings = %d / %d, want 32 MiB / 4 KiB", gitLabMaxResponseBytes, gitLabMaxErrorResponseBytes)
	}
}

func TestGitLabGetMergeRequestNormal(t *testing.T) {
	w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
		_, _ = rw.Write(mrJSON(9, 0))
	}, 5*time.Second)
	mr, err := w.get(context.Background())
	if err != nil {
		t.Fatalf("GetMergeRequest: %v", err)
	}
	if mr.IID != 9 || mr.State != MRStateMerged || mr.WebURL != "https://gl/mr/9" {
		t.Fatalf("mapped MR = %+v", mr)
	}
	if !w.rt.allClosed() {
		t.Fatal("response body not closed")
	}
}

// The real 32 MiB ceiling, limit+1 bytes, refused with the typed error.
func TestGitLabGetMergeRequestRealCeilingTypedRefusal(t *testing.T) {
	const limit = 32 << 20
	w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
		body := mrJSON(9, 0)
		rw.Header().Set("Content-Length", fmt.Sprint(limit+1))
		_, _ = rw.Write(body)
		chunk := bytes.Repeat([]byte(" "), 64<<10)
		for written := int64(len(body)); written < limit+1; {
			n := min(int64(len(chunk)), limit+1-written)
			if _, err := rw.Write(chunk[:n]); err != nil {
				return
			}
			written += n
		}
	}, 30*time.Second)
	mr, err := w.get(context.Background())
	wantRefusal(t, mr, err)
	if got := w.rt.totalRead(); got > limit+1 {
		t.Fatalf("read %d bytes, want at most limit+1", got)
	}
}

func TestGitLabGetMergeRequestAtLimit(t *testing.T) {
	const limit = 8192
	lowerGitLabLimit(t, limit)

	t.Run("exactly_limit_decodes", func(t *testing.T) {
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(mrJSON(9, limit)) }, 5*time.Second)
		mr, err := w.get(context.Background())
		if err != nil || mr.IID != 9 {
			t.Fatalf("limit-sized body: mr=%+v err=%v", mr, err)
		}
		if w.rt.maxRead() > limit+1 || !w.rt.allClosed() {
			t.Fatalf("read=%d closed=%v", w.rt.maxRead(), w.rt.allClosed())
		}
	})
	t.Run("limit_plus_one_refused", func(t *testing.T) {
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(mrJSON(9, limit+1)) }, 5*time.Second)
		mr, err := w.get(context.Background())
		wantRefusal(t, mr, err)
		if w.rt.reqs.Load() != 1 {
			t.Fatalf("requests = %d, want 1", w.rt.reqs.Load())
		}
		if w.rt.maxRead() > limit+1 {
			t.Fatalf("read %d bytes, want at most limit+1 = %d", w.rt.maxRead(), limit+1)
		}
		if !w.rt.allClosed() {
			t.Fatal("original body not closed on refusal")
		}
	})
}

func TestGitLabGetMergeRequestBodyShapes(t *testing.T) {
	const limit = 8192
	lowerGitLabLimit(t, limit)

	chunked := func(total int) http.HandlerFunc {
		return func(rw http.ResponseWriter, _ *http.Request) {
			body := mrJSON(9, total)
			half := len(body) / 2
			_, _ = rw.Write(body[:half])
			rw.(http.Flusher).Flush() // no Content-Length: chunked on the wire
			_, _ = rw.Write(body[half:])
		}
	}
	t.Run("chunked_within_limit", func(t *testing.T) {
		w := newWireMR(t, chunked(limit), 5*time.Second)
		if mr, err := w.get(context.Background()); err != nil || mr.IID != 9 {
			t.Fatalf("mr=%+v err=%v", mr, err)
		}
	})
	t.Run("chunked_over_limit", func(t *testing.T) {
		w := newWireMR(t, chunked(limit*3), 5*time.Second)
		mr, err := w.get(context.Background())
		wantRefusal(t, mr, err)
	})
	t.Run("large_json_string_within_limit", func(t *testing.T) {
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(rw, `{"iid":9,"state":"merged","description":"%s"}`, strings.Repeat("d", 6000))
		}, 5*time.Second)
		if mr, err := w.get(context.Background()); err != nil || mr.IID != 9 {
			t.Fatalf("mr=%+v err=%v", mr, err)
		}
	})
	t.Run("large_json_string_over_limit", func(t *testing.T) {
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(rw, `{"iid":9,"state":"merged","description":"%s"}`, strings.Repeat("d", limit+500))
		}, 5*time.Second)
		mr, err := w.get(context.Background())
		wantRefusal(t, mr, err)
	})
	t.Run("valid_mr_then_excess_trailing_bytes", func(t *testing.T) {
		// A streaming decode would accept the valid prefix and ignore the rest.
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
			_, _ = rw.Write(mrJSON(9, 0))
			_, _ = rw.Write(bytes.Repeat([]byte("x"), limit*2))
		}, 5*time.Second)
		mr, err := w.get(context.Background())
		wantRefusal(t, mr, err)
	})
	// Wrapper-level: the wire cannot express these Content-Length shapes.
	t.Run("wrapper_unknown_length_over_limit", func(t *testing.T) {
		f := &fabRT{}
		f.fn = func(_ int, req *http.Request) (*http.Response, error) {
			return f.resp(req, 200, bigBody(mrJSON(9, 0), limit*2, ' ')), nil
		}
		d := newDriverOver(t, f, fabBase, 5*time.Second)
		mr, err := d.GetMergeRequest(context.Background(), 7, 9)
		wantRefusal(t, mr, err)
		if f.maxRead() > limit+1 || !f.allClosed() {
			t.Fatalf("read=%d closed=%v", f.maxRead(), f.allClosed())
		}
	})
	t.Run("wrapper_misleading_small_content_length", func(t *testing.T) {
		f := &fabRT{}
		f.fn = func(_ int, req *http.Request) (*http.Response, error) {
			r := f.resp(req, 200, bigBody(mrJSON(9, 0), limit*2, ' '))
			r.ContentLength = 10
			r.Header.Set("Content-Length", "10")
			return r, nil
		}
		d := newDriverOver(t, f, fabBase, 5*time.Second)
		mr, err := d.GetMergeRequest(context.Background(), 7, 9)
		wantRefusal(t, mr, err)
	})
	t.Run("wrapper_misleading_huge_content_length_small_body", func(t *testing.T) {
		f := &fabRT{}
		f.fn = func(_ int, req *http.Request) (*http.Response, error) {
			r := f.resp(req, 200, bytes.NewReader(mrJSON(9, 0)))
			r.ContentLength = 1 << 40
			r.Header.Set("Content-Length", fmt.Sprint(int64(1)<<40))
			return r, nil
		}
		d := newDriverOver(t, f, fabBase, 5*time.Second)
		if mr, err := d.GetMergeRequest(context.Background(), 7, 9); err != nil || mr.IID != 9 {
			t.Fatalf("mr=%+v err=%v", mr, err)
		}
	})
}

// The limit counts transfer-decoded wire bytes: the request asks for identity
// encoding, and a response that still declares a content encoding is refused
// without a body, so a tiny compressed body can never inflate past the limit.
func TestGitLabGetMergeRequestRefusesContentEncoding(t *testing.T) {
	const limit = 8192
	lowerGitLabLimit(t, limit)
	gzBody := func() []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(mrJSON(9, limit*50))
		_ = zw.Close()
		return buf.Bytes()
	}()
	t.Run("requests_identity_and_decodes_plain", func(t *testing.T) {
		var seen atomic.Value
		w := newWireMR(t, func(rw http.ResponseWriter, r *http.Request) {
			seen.Store(r.Header.Get("Accept-Encoding"))
			_, _ = rw.Write(mrJSON(9, limit/2))
		}, 5*time.Second)
		if mr, err := w.get(context.Background()); err != nil || mr.IID != 9 {
			t.Fatalf("mr=%+v err=%v", mr, err)
		}
		if got, _ := seen.Load().(string); got != "identity" {
			t.Fatalf("server saw Accept-Encoding %q, want identity", got)
		}
	})
	t.Run("forced_gzip_success_is_a_fixed_error", func(t *testing.T) {
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header().Set("Content-Encoding", "gzip")
			_, _ = rw.Write(gzBody)
		}, 5*time.Second)
		mr, err := w.get(context.Background())
		if err == nil || isTooLarge(err) {
			t.Fatalf("err = %v, want the content-encoding error, not the size refusal", err)
		}
		if !strings.Contains(err.Error(), "unsupported content encoding") {
			t.Fatalf("err = %q, want the fixed content-encoding text", err.Error())
		}
		if mr != (MergeRequest{}) {
			t.Fatalf("mr = %+v, want zero", mr)
		}
		if w.rt.reqs.Load() != 1 {
			t.Fatalf("requests = %d, want 1 (not retried)", w.rt.reqs.Load())
		}
		if w.rt.totalRead() != 0 {
			t.Fatalf("read %d body bytes of a refused encoding, want 0", w.rt.totalRead())
		}
	})
	t.Run("second_content_encoding_line_is_refused", func(t *testing.T) {
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header()["Content-Encoding"] = []string{"identity", "gzip"}
			_, _ = rw.Write(gzBody)
		}, 5*time.Second)
		mr, err := w.get(context.Background())
		if err == nil || !strings.Contains(err.Error(), "unsupported content encoding") {
			t.Fatalf("err = %v, want the content-encoding error", err)
		}
		if mr != (MergeRequest{}) || w.rt.totalRead() != 0 {
			t.Fatalf("mr = %+v, read %d bytes; want zero MR and nothing read", mr, w.rt.totalRead())
		}
	})
	// Forced gzip on a non-success status keeps the status outcome of an empty body.
	norm := func(w *wireMR, err error) string {
		return strings.ReplaceAll(err.Error(), w.srv.URL, "SRV")
	}
	for _, c := range []struct {
		code     int
		class    ErrorClass
		attempts int64
	}{{404, ErrorClassOther, 1}, {401, ErrorClassAuth, 1}, {500, ErrorClassServerError, 6}} {
		t.Run(fmt.Sprintf("forced_gzip_%d_matches_empty_body", c.code), func(t *testing.T) {
			plain := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(c.code) }, 5*time.Second)
			_, plainErr := plain.get(context.Background())
			enc := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
				rw.Header().Set("Content-Encoding", "gzip")
				rw.WriteHeader(c.code)
				_, _ = rw.Write(gzBody)
			}, 5*time.Second)
			mr, err := enc.get(context.Background())
			if err == nil || plainErr == nil || isTooLarge(err) {
				t.Fatalf("err = %v, plain err = %v", err, plainErr)
			}
			if norm(enc, err) != norm(plain, plainErr) {
				t.Fatalf("message = %q, want the empty-body message %q", norm(enc, err), norm(plain, plainErr))
			}
			if Class(err) != c.class {
				t.Fatalf("class = %q, want %q", Class(err), c.class)
			}
			if enc.rt.reqs.Load() != c.attempts {
				t.Fatalf("attempts = %d, want %d", enc.rt.reqs.Load(), c.attempts)
			}
			if strings.ContainsRune(err.Error(), '\x1f') || strings.ContainsRune(err.Error(), '\ufffd') {
				t.Fatalf("binary body in the message: %q", err.Error())
			}
			if mr != (MergeRequest{}) {
				t.Fatalf("mr = %+v, want zero", mr)
			}
			if enc.rt.totalRead() != 0 {
				t.Fatalf("read %d body bytes of a refused encoding, want 0", enc.rt.totalRead())
			}
		})
	}
}

// countingConn counts the bytes the client reads off the socket, headers and
// framing included.
type countingConn struct {
	net.Conn
	n *atomic.Int64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Wire probe: a server that streams forever (endless empty stored deflate
// blocks under Content-Encoding: gzip, or endless plain bytes) must cost the
// client at most the limit plus headers, and must end the call event-driven, far
// before the 30 s client timeout.
func TestGitLabGetMergeRequestWireByteBound(t *testing.T) {
	const limit = 64 << 10
	lowerGitLabLimit(t, limit)
	gzHeader := []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0, 0, 0xff}
	cases := []struct {
		name string
		gzip bool
	}{{"zero_yield_deflate_blocks_gzip", true}, {"endless_plain_stream", false}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var wire, dials atomic.Int64
			tr := newHTTP1Transport(forgeHTTPTransport)
			dial := tr.DialContext
			tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				dials.Add(1)
				conn, err := dial(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return countingConn{Conn: conn, n: &wire}, nil
			}
			t.Cleanup(tr.CloseIdleConnections)
			srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				if c.gzip {
					rw.Header().Set("Content-Encoding", "gzip")
				}
				rw.WriteHeader(200)
				chunk := bytes.Repeat([]byte(" "), 16<<10)
				if c.gzip {
					_, _ = rw.Write(gzHeader)
					chunk = bytes.Repeat([]byte{0, 0, 0, 0xff, 0xff}, 3000)
				}
				fl, _ := rw.(http.Flusher)
				for r.Context().Err() == nil {
					if _, err := rw.Write(chunk); err != nil {
						return
					}
					if fl != nil {
						fl.Flush()
					}
				}
			}))
			t.Cleanup(srv.Close)
			d := newDriverOver(t, tr, srv.URL, 30*time.Second)
			start := time.Now()
			mr, err := d.GetMergeRequest(context.Background(), 7, 9)
			elapsed := time.Since(start)
			if err == nil || mr != (MergeRequest{}) {
				t.Fatalf("mr=%+v err=%v, want a refusal and the zero MR", mr, err)
			}
			if !c.gzip {
				wantRefusal(t, mr, err)
			}
			// limit+1, headers and read-ahead slack, plus the bounded drain net/http
			// itself runs in the background when a body is closed early (it reads up
			// to 256 KiB+1 more so the connection could be reused; the connection is
			// then closed). That drain is the only wire cost past the limit.
			const stdlibEarlyCloseDrain = 256<<10 + 1
			if got, max := wire.Load(), int64(limit+1+2048+4096+stdlibEarlyCloseDrain); got > max {
				t.Fatalf("client read %d wire bytes, want at most %d (dials %d, err %v)", got, max, dials.Load(), err)
			}
			if elapsed > 10*time.Second {
				t.Fatalf("took %v, want far below the 30 s client timeout", elapsed)
			}
			t.Logf("%s: wire=%d elapsed=%v dials=%d err=%v", c.name, wire.Load(), elapsed, dials.Load(), err)
		})
	}
}

func TestResponseLimitedRequestsUseHTTP1Route(t *testing.T) {
	var hit string
	mk := func(name string) func() http.RoundTripper {
		return func() http.RoundTripper {
			return ciRoundTripper(func(*http.Request) (*http.Response, error) {
				hit = name
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
			})
		}
	}
	r := &ciRouteTransport{general: mk("general"), ciJSON: mk("http1")}
	for name, tc := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"unmarked":       {context.Background(), "general"},
		"limit marked":   {withResponseLimit(context.Background(), &responseLimitState{}), "http1"},
		"ci json (kept)": {markCIJSON(WithCIRepairReadLimits(context.Background())), "http1"},
	} {
		req, _ := http.NewRequestWithContext(tc.ctx, http.MethodGet, "https://example.test", nil)
		if _, err := r.RoundTrip(req); err != nil || hit != tc.want {
			t.Fatalf("%s: routed to %q (err %v), want %q", name, hit, err, tc.want)
		}
	}
}

// Only the SDK success statuses (200, 201, 202, 204) take the 32 MiB eager path;
// another 2xx is an error to the SDK and takes the lazy 4 KiB path.
func TestGitLabGetMergeRequestNonSuccess2xxIsLazy(t *testing.T) {
	t.Run("oversized_206_typed_refusal", func(t *testing.T) {
		w := newWireMR(t, oversizedHandler(206, ""), 5*time.Second)
		mr, err := w.get(context.Background())
		wantRefusal(t, mr, err)
		if w.rt.reqs.Load() != 1 {
			t.Fatalf("requests = %d, want 1", w.rt.reqs.Load())
		}
		if w.rt.maxRead() > gitLabMaxErrorResponseBytes+1 {
			t.Fatalf("read %d bytes, want at most 4 KiB+1", w.rt.maxRead())
		}
	})
	t.Run("small_206_is_the_sdk_error", func(t *testing.T) {
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(206)
			_, _ = rw.Write([]byte(`{"message":"partial"}`))
		}, 5*time.Second)
		mr, err := w.get(context.Background())
		if err == nil || isTooLarge(err) {
			t.Fatalf("err = %v, want the status error", err)
		}
		want := fmt.Sprintf("%sGET %s/api/v4/projects/7/merge_requests/9: 206 {message: partial}", fabMRMsg, w.srv.URL)
		if err.Error() != want {
			t.Fatalf("message = %q, want %q", err.Error(), want)
		}
		if w.rt.reqs.Load() != 1 || mr != (MergeRequest{}) {
			t.Fatalf("requests = %d, mr = %+v", w.rt.reqs.Load(), mr)
		}
	})
	t.Run("mid_body_failure_206_is_the_sdk_error", func(t *testing.T) {
		f := &fabRT{}
		f.fn = func(_ int, req *http.Request) (*http.Response, error) {
			return f.resp(req, 206, &errAfter{data: []byte(`{"message":"` + patPrefix()), err: io.ErrUnexpectedEOF}), nil
		}
		d := newDriverOver(t, f, fabBase, 5*time.Second)
		mr, err := d.GetMergeRequest(context.Background(), 7, 9)
		want := fmt.Sprintf("%sGET %s: 206", fabMRMsg, fabMRURL)
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q (not the body-read error)", err, want)
		}
		if f.calls.Load() != 1 || mr != (MergeRequest{}) {
			t.Fatalf("attempts = %d, mr = %+v", f.calls.Load(), mr)
		}
	})
}

// lazyCappedBody hands on exactly the limit, then fails with the typed refusal and
// records the overflow. The loop is bounded so a regressed cap fails by assertion.
func TestLazyCappedBodyYieldsExactlyLimit(t *testing.T) {
	const limit = 100
	for _, bufSize := range []int{7, 100, 101, 4096} {
		state := &responseLimitState{}
		b := &lazyCappedBody{rc: io.NopCloser(fillReader{'x'}), remaining: limit, state: state}
		var total int
		var err error
		buf := make([]byte, bufSize)
		for i := 0; i < 1000 && err == nil; i++ {
			var n int
			n, err = b.Read(buf)
			total += n
			if total > limit {
				t.Fatalf("buf %d: returned %d bytes, past the limit %d", bufSize, total, limit)
			}
		}
		var tooLarge *ResponseTooLargeError
		if !errors.As(err, &tooLarge) {
			t.Fatalf("buf %d: err = %v after %d bytes, want *ResponseTooLargeError", bufSize, err, total)
		}
		if total != limit {
			t.Fatalf("buf %d: returned %d bytes, want exactly %d", bufSize, total, limit)
		}
		if !state.overflow.Load() {
			t.Fatalf("buf %d: overflow not recorded", bufSize)
		}
	}
}

func TestGitLabGetMergeRequestCancellation(t *testing.T) {
	partial := func(started chan struct{}) http.HandlerFunc {
		var once sync.Once
		return func(rw http.ResponseWriter, r *http.Request) {
			rw.Header().Set("Content-Length", "100000")
			rw.WriteHeader(200)
			_, _ = rw.Write(mrJSON(9, 0)[:20])
			rw.(http.Flusher).Flush()
			once.Do(func() { close(started) })
			<-r.Context().Done()
		}
	}
	t.Run("caller_cancel", func(t *testing.T) {
		started := make(chan struct{})
		w := newWireMR(t, partial(started), 10*time.Second)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			// Cancel only once the response (headers) reached the client, so the
			// cancel lands inside the body read.
			<-started
			for i := 0; i < 500 && w.rt.count() == 0; i++ {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
		}()
		mr, err := w.get(ctx)
		if err == nil || isTooLarge(err) || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("err = %v, want context canceled and not the size refusal", err)
		}
		if mr != (MergeRequest{}) {
			t.Fatalf("mr = %+v, want zero", mr)
		}
		if !w.rt.allClosed() {
			t.Fatal("original body not closed after cancellation")
		}
	})
	t.Run("caller_deadline_is_timeout_class", func(t *testing.T) {
		w := newWireMR(t, partial(make(chan struct{})), 10*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		mr, err := w.get(ctx)
		if err == nil || isTooLarge(err) {
			t.Fatalf("err = %v, want a deadline error", err)
		}
		if Class(err) != ErrorClassTimeout {
			t.Fatalf("class = %q, want timeout (err %v)", Class(err), err)
		}
		if mr != (MergeRequest{}) {
			t.Fatalf("mr = %+v, want zero", mr)
		}
	})
}

// 2xx mid-body attempt-count controls (a)-(g).
func TestGitLabGetMergeRequestMidBodyReadFailureAttempts(t *testing.T) {
	prefix := mrJSON(9, 0)[:12]
	fabricated := func(readErr error) *fabRT {
		f := &fabRT{}
		f.fn = func(_ int, req *http.Request) (*http.Response, error) {
			return f.resp(req, 200, &errAfter{data: append([]byte(nil), prefix...), err: readErr}), nil
		}
		return f
	}
	run := func(t *testing.T, f *fabRT) (MergeRequest, error) {
		t.Helper()
		d := newDriverOver(t, f, fabBase, 5*time.Second)
		return d.GetMergeRequest(context.Background(), 7, 9)
	}
	notRefusal := func(t *testing.T, mr MergeRequest, err error, wantSub string) {
		t.Helper()
		if err == nil {
			t.Fatalf("want a read error, got %+v", mr)
		}
		if isTooLarge(err) {
			t.Fatalf("a read failure must not be the size refusal: %v", err)
		}
		if !strings.Contains(err.Error(), wantSub) {
			t.Fatalf("error %q lacks %q", err.Error(), wantSub)
		}
		if mr != (MergeRequest{}) {
			t.Fatalf("mr = %+v, want zero", mr)
		}
	}

	t.Run("a_temporary", func(t *testing.T) {
		f := fabricated(&net.OpError{Op: "read", Net: "tcp", Err: tempErr{}})
		mr, err := run(t, f)
		notRefusal(t, mr, err, "")
		if f.calls.Load() != 1 {
			t.Fatalf("attempts = %d, want 1 (a body already partly delivered is not retried)", f.calls.Load())
		}
		if !f.allClosed() {
			t.Fatal("original body not closed")
		}
	})
	t.Run("b_timeout", func(t *testing.T) {
		f := fabricated(&net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded})
		mr, err := run(t, f)
		notRefusal(t, mr, err, "")
		if f.calls.Load() != 1 {
			t.Fatalf("attempts = %d, want 1", f.calls.Load())
		}
		if Class(err) != ErrorClassTimeout {
			t.Fatalf("class = %q, want timeout", Class(err))
		}
	})
	t.Run("c_truncated_real_wire", func(t *testing.T) {
		var hits atomic.Int64
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			conn, buf, err := rw.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\nContent-Type: application/json\r\n\r\n" + string(prefix))
			_ = buf.Flush()
			_ = conn.Close()
		}, 5*time.Second)
		mr, err := w.get(context.Background())
		notRefusal(t, mr, err, "")
		if hits.Load() != 1 {
			t.Fatalf("attempts = %d, want 1", hits.Load())
		}
	})
	t.Run("d_below_limit_5xx_retries", func(t *testing.T) {
		var hits atomic.Int64
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			rw.WriteHeader(500)
			_, _ = rw.Write([]byte(`{"message":"boom"}`))
		}, 5*time.Second)
		_, err := w.get(context.Background())
		if err == nil || isTooLarge(err) || Class(err) != ErrorClassServerError {
			t.Fatalf("err = %v class=%q, want a server_error", err, Class(err))
		}
		if hits.Load() != 6 {
			t.Fatalf("attempts = %d, want RetryMax+1 = 6", hits.Load())
		}
	})
	t.Run("e_client_timeout_real_wire", func(t *testing.T) {
		var hits atomic.Int64
		w := newWireMR(t, func(rw http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			rw.Header().Set("Content-Length", "100000")
			rw.WriteHeader(200)
			_, _ = rw.Write(prefix)
			rw.(http.Flusher).Flush()
			<-r.Context().Done()
		}, 400*time.Millisecond)
		mr, err := w.get(context.Background())
		notRefusal(t, mr, err, "")
		if Class(err) != ErrorClassTimeout {
			t.Fatalf("class = %q, want timeout (err %v)", Class(err), err)
		}
		if hits.Load() != 1 {
			t.Fatalf("attempts = %d, want 1", hits.Load())
		}
	})
	t.Run("f_connection_refused_text_mid_body", func(t *testing.T) {
		f := fabricated(&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNREFUSED})
		mr, err := run(t, f)
		notRefusal(t, mr, err, "")
		if f.calls.Load() != 1 {
			t.Fatalf("attempts = %d, want 1 (mid-body text must not trigger a retry)", f.calls.Load())
		}
		if strings.Contains(err.Error(), "connection refused") {
			t.Fatalf("OS detail leaked into the error: %q", err.Error())
		}
	})
	t.Run("g_pre_response_dial_retries", func(t *testing.T) {
		f := &fabRT{}
		f.fn = func(int, *http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("boom")}
		}
		d := newDriverOver(t, f, fabBase, 5*time.Second)
		_, err := d.GetMergeRequest(context.Background(), 7, 9)
		if err == nil || isTooLarge(err) {
			t.Fatalf("err = %v, want a dial failure", err)
		}
		if f.calls.Load() != 6 {
			t.Fatalf("attempts = %d, want 6 (pre-response errors keep their retry)", f.calls.Load())
		}
	})
}

// Non-2xx mid-body parity controls (h)-(m), (o), run for both failure shapes.
func TestGitLabGetMergeRequestNon2xxMidBodyParity(t *testing.T) {
	cases := []struct {
		name     string
		code     int
		attempts int64
		class    ErrorClass
	}{
		{"h_401_lazy", 401, 1, ErrorClassAuth},
		{"i_403_lazy", 403, 1, ErrorClassAuth},
		{"j_404_lazy", 404, 1, ErrorClassOther},
		{"k_500_eager", 500, 6, ErrorClassServerError},
		{"l_503_eager", 503, 6, ErrorClassServerError},
		{"m_429_eager", 429, 6, ErrorClassRateLimited},
		{"o_501_lazy", 501, 1, ErrorClassServerError},
	}
	failures := map[string]error{
		"temporary":      &net.OpError{Op: "read", Net: "tcp", Err: tempErr{}},
		"unexpected_eof": io.ErrUnexpectedEOF,
	}
	for _, c := range cases {
		for fname, ferr := range failures {
			t.Run(c.name+"/"+fname, func(t *testing.T) {
				f := &fabRT{}
				f.fn = func(_ int, req *http.Request) (*http.Response, error) {
					return f.resp(req, c.code, &errAfter{data: []byte(`{"message":"` + patPrefix()), err: ferr}), nil
				}
				d := newDriverOver(t, f, fabBase, 5*time.Second)
				mr, err := d.GetMergeRequest(context.Background(), 7, 9)
				if err == nil || isTooLarge(err) {
					t.Fatalf("err = %v, want the status error, not the size refusal", err)
				}
				want := fmt.Sprintf("%sGET %s: %d", fabMRMsg, fabMRURL, c.code)
				if c.code == 404 {
					want = fabMRMsg + "404 Not Found"
				}
				if err.Error() != want {
					t.Fatalf("message = %q, want %q", err.Error(), want)
				}
				if strings.Contains(err.Error(), patPrefix()) {
					t.Fatalf("partial body bytes reached the error: %q", err.Error())
				}
				if Class(err) != c.class {
					t.Fatalf("class = %q, want %q", Class(err), c.class)
				}
				if f.calls.Load() != c.attempts {
					t.Fatalf("attempts = %d, want %d", f.calls.Load(), c.attempts)
				}
				if mr != (MergeRequest{}) {
					t.Fatalf("mr = %+v, want zero", mr)
				}
				if !f.allClosed() {
					t.Fatal("original body not closed")
				}
			})
		}
	}
}

// Cancellation controls (p), (q), (n0), (n).
func TestGitLabGetMergeRequestNon2xxCancellation(t *testing.T) {
	// blockingStatus serves code with a body that yields a few bytes and then
	// blocks until the request context ends; the first block cancels the caller.
	run := func(t *testing.T, code int, blockFrom int) (*fabRT, MergeRequest, error) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		var once sync.Once
		f := &fabRT{}
		f.fn = func(n int, req *http.Request) (*http.Response, error) {
			if n < blockFrom {
				return f.resp(req, code, bytes.NewReader(nil)), nil
			}
			return f.resp(req, code, &blockAfter{
				data:    []byte(`{"message":"` + patPrefix()),
				ctx:     req.Context(),
				onBlock: func() { once.Do(cancel) },
			}), nil
		}
		d := newDriverOver(t, f, fabBase, 10*time.Second)
		mr, err := d.GetMergeRequest(ctx, 7, 9)
		return f, mr, err
	}
	t.Run("p_401_cancel_mid_body_lazy", func(t *testing.T) {
		f, mr, err := run(t, 401, 1)
		if want := fmt.Sprintf("%sGET %s: 401", fabMRMsg, fabMRURL); err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if Class(err) != ErrorClassAuth || mr != (MergeRequest{}) || f.calls.Load() != 1 {
			t.Fatalf("class=%q mr=%+v attempts=%d", Class(err), mr, f.calls.Load())
		}
	})
	t.Run("q_404_cancel_mid_body_lazy", func(t *testing.T) {
		f, mr, err := run(t, 404, 1)
		if want := fabMRMsg + "404 Not Found"; err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if mr != (MergeRequest{}) || f.calls.Load() != 1 {
			t.Fatalf("mr=%+v attempts=%d", mr, f.calls.Load())
		}
	})
	t.Run("n0_500_cancel_first_attempt_eager", func(t *testing.T) {
		f, mr, err := run(t, 500, 1)
		if want := fabMRMsg + "context canceled"; err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q (bare context error, as on base)", err, want)
		}
		if Class(err) != ErrorClassOther || mr != (MergeRequest{}) || f.calls.Load() != 1 {
			t.Fatalf("class=%q mr=%+v attempts=%d", Class(err), mr, f.calls.Load())
		}
	})
	// Recorded deviation (design item 10a): a caller cancel during the FINAL
	// attempt's eager body read surfaces the bare context error. Base returns
	// ErrorResponse{500} for that attempt (CheckResponse sees the failed read).
	t.Run("n_500_cancel_final_attempt_eager_deviation", func(t *testing.T) {
		f, mr, err := run(t, 500, 6)
		if want := fabMRMsg + "context canceled"; err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
		if Class(err) != ErrorClassOther || mr != (MergeRequest{}) || f.calls.Load() != 6 {
			t.Fatalf("class=%q mr=%+v attempts=%d, want other and 6 attempts", Class(err), mr, f.calls.Load())
		}
	})
}

func oversizedHandler(code int, location string) http.HandlerFunc {
	return func(rw http.ResponseWriter, _ *http.Request) {
		if location != "" {
			rw.Header().Set("Location", location)
		}
		rw.WriteHeader(code)
		_, _ = rw.Write(bytes.Repeat([]byte("BODYMARK"), 700)) // 5600 bytes
	}
}

func TestGitLabGetMergeRequestLazyPathOverflow(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"401", oversizedHandler(401, "")},
		{"404", oversizedHandler(404, "")},
		{"501", oversizedHandler(501, "")},
		{"302_without_location", oversizedHandler(302, "")},
		{"302_off_origin", oversizedHandler(302, "http://other.invalid/x")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWireMR(t, c.h, 5*time.Second)
			mr, err := w.get(context.Background())
			wantRefusal(t, mr, err)
			if w.rt.reqs.Load() != 1 {
				t.Fatalf("requests = %d, want 1 (these statuses are never status-retried)", w.rt.reqs.Load())
			}
			if w.rt.maxRead() > gitLabMaxErrorResponseBytes+1 {
				t.Fatalf("read %d bytes, want at most 4 KiB+1", w.rt.maxRead())
			}
			if !w.rt.allClosed() {
				t.Fatal("original body not closed")
			}
			if strings.Contains(err.Error(), "BODYMARK") || strings.Contains(err.Error(), patPrefix()) {
				t.Fatalf("remote content in the refusal: %q", err.Error())
			}
		})
	}
}

// Eager status set: statuses the SDK retries on status alone (0, 429, 5xx but
// 501, 600-999) read eagerly; an overflow is one request, a small body retries.
func TestGitLabGetMergeRequestEagerStatusSet(t *testing.T) {
	t.Run("oversized_999_single_request", func(t *testing.T) {
		w := newWireMR(t, oversizedHandler(999, ""), 5*time.Second)
		mr, err := w.get(context.Background())
		wantRefusal(t, mr, err)
		if w.rt.reqs.Load() != 1 {
			t.Fatalf("requests = %d, want 1", w.rt.reqs.Load())
		}
	})
	t.Run("oversized_status_0_single_request_wrapper_level", func(t *testing.T) {
		f := &fabRT{}
		f.fn = func(_ int, req *http.Request) (*http.Response, error) {
			return f.resp(req, 0, bigBody(nil, 6000, 'z')), nil
		}
		d := newDriverOver(t, f, fabBase, 5*time.Second)
		mr, err := d.GetMergeRequest(context.Background(), 7, 9)
		wantRefusal(t, mr, err)
		if f.calls.Load() != 1 {
			t.Fatalf("requests = %d, want 1", f.calls.Load())
		}
	})
	t.Run("below_limit_999_retries", func(t *testing.T) {
		w := newWireMR(t, func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(999)
			_, _ = rw.Write([]byte(`{"message":"odd"}`))
		}, 5*time.Second)
		_, err := w.get(context.Background())
		if err == nil || isTooLarge(err) {
			t.Fatalf("err = %v, want a status error", err)
		}
		if w.rt.reqs.Load() != 6 {
			t.Fatalf("requests = %d, want 6", w.rt.reqs.Load())
		}
	})
}

// Overflow retry counts: an oversized body is never retried.
func TestGitLabGetMergeRequestOverflowIsNotRetried(t *testing.T) {
	lowerGitLabLimit(t, 8192)
	cases := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"200", func(rw http.ResponseWriter, _ *http.Request) { _, _ = rw.Write(mrJSON(9, 10000)) }},
		{"500", oversizedHandler(500, "")},
		{"429", oversizedHandler(429, "")},
		{"999", oversizedHandler(999, "")},
		{"501", oversizedHandler(501, "")},
		{"404", oversizedHandler(404, "")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newWireMR(t, c.h, 5*time.Second)
			mr, err := w.get(context.Background())
			wantRefusal(t, mr, err)
			if w.rt.reqs.Load() != 1 {
				t.Fatalf("requests = %d, want exactly 1", w.rt.reqs.Load())
			}
		})
	}
}

// State isolation: the overflow flag is per call, never per driver or package.
func TestGitLabGetMergeRequestOverflowStateIsPerCall(t *testing.T) {
	handler := func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/9") {
			oversizedHandler(404, "")(rw, r)
			return
		}
		_, _ = rw.Write(mrJSON(10, 0))
	}
	t.Run("sequential", func(t *testing.T) {
		w := newWireMR(t, handler, 5*time.Second)
		mr, err := w.d.GetMergeRequest(context.Background(), 7, 9)
		wantRefusal(t, mr, err)
		mr, err = w.d.GetMergeRequest(context.Background(), 7, 10)
		if err != nil || mr.IID != 10 {
			t.Fatalf("the next call must not inherit the overflow: mr=%+v err=%v", mr, err)
		}
	})
	t.Run("concurrent", func(t *testing.T) {
		w := newWireMR(t, handler, 5*time.Second)
		var wg sync.WaitGroup
		errs := make(chan string, 64)
		for range 20 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				mr, err := w.d.GetMergeRequest(context.Background(), 7, 9)
				if err == nil || !isTooLarge(err) || mr != (MergeRequest{}) {
					errs <- fmt.Sprintf("oversized call: mr=%+v err=%v", mr, err)
				}
			}()
			go func() {
				defer wg.Done()
				mr, err := w.d.GetMergeRequest(context.Background(), 7, 10)
				if err != nil || mr.IID != 10 {
					errs <- fmt.Sprintf("normal call: mr=%+v err=%v", mr, err)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			t.Error(e)
		}
	})
}

func TestGitLabGetMergeRequestRedirectParity(t *testing.T) {
	redirecting := func(kind string) http.HandlerFunc {
		return func(rw http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/10") {
				_, _ = rw.Write(mrJSON(10, 0))
				return
			}
			rw.Header().Set("Location", "/api/v4/projects/7/merge_requests/10")
			switch kind {
			case "small_body":
				rw.WriteHeader(302)
				_, _ = rw.Write([]byte("moved"))
			case "body_over_4k":
				rw.WriteHeader(302)
				_, _ = rw.Write(bytes.Repeat([]byte("m"), 6000))
			case "stalled_body":
				rw.Header().Set("Content-Length", "100000")
				rw.WriteHeader(302)
				_, _ = rw.Write(bytes.Repeat([]byte("m"), 3072))
				rw.(http.Flusher).Flush()
				<-r.Context().Done()
			}
		}
	}
	for _, kind := range []string{"small_body", "body_over_4k", "stalled_body"} {
		t.Run(kind, func(t *testing.T) {
			w := newWireMR(t, redirecting(kind), 5*time.Second)
			mr, err := w.get(context.Background())
			if err != nil || mr.IID != 10 {
				t.Fatalf("same-origin redirect must be followed as without the limit: mr=%+v err=%v", mr, err)
			}
		})
	}
}

// Non-2xx size boundary at the real 4 KiB limit, token straddling the cut.
func TestGitLabGetMergeRequestNon2xxSizeBoundary(t *testing.T) {
	tok := testToken()
	// body returns exactly n bytes with the token starting at offset at.
	body := func(n, at int) []byte {
		b := bytes.Repeat([]byte("x"), n)
		copy(b[at:], tok)
		return b
	}
	serve := func(code int, b []byte) http.HandlerFunc {
		return func(rw http.ResponseWriter, _ *http.Request) {
			rw.WriteHeader(code)
			_, _ = rw.Write(b)
		}
	}
	t.Run("exactly_4096_keeps_existing_behaviour", func(t *testing.T) {
		b := body(4096, 100)
		for _, c := range []struct {
			code  int
			class ErrorClass
		}{{401, ErrorClassAuth}, {403, ErrorClassAuth}, {404, ErrorClassOther}, {500, ErrorClassServerError}} {
			w := newWireMR(t, serve(c.code, b), 5*time.Second)
			mr, err := w.get(context.Background())
			if err == nil || isTooLarge(err) {
				t.Fatalf("%d: err = %v, want the status error", c.code, err)
			}
			if Class(err) != c.class {
				t.Fatalf("%d: class = %q, want %q", c.code, Class(err), c.class)
			}
			if strings.Contains(err.Error(), tok) {
				t.Fatalf("%d: token reached the error: %q", c.code, err.Error())
			}
			if c.code == 404 {
				if want := fabMRMsg + "404 Not Found"; err.Error() != want {
					t.Fatalf("404: err = %q, want %q", err.Error(), want)
				}
			} else if !strings.Contains(err.Error(), redactPlaceholder) {
				t.Fatalf("%d: want the token redacted in %q", c.code, err.Error())
			}
			if mr != (MergeRequest{}) {
				t.Fatalf("%d: mr = %+v", c.code, mr)
			}
			wantAttempts := int64(1)
			if c.code == 500 {
				wantAttempts = 6
			}
			if w.rt.reqs.Load() != wantAttempts {
				t.Fatalf("%d: attempts = %d, want %d", c.code, w.rt.reqs.Load(), wantAttempts)
			}
		}
	})
	for _, code := range []int{403, 500} {
		t.Run(fmt.Sprintf("4097_straddling_token_%d", code), func(t *testing.T) {
			// The token starts 6 bytes before the 4096 cut.
			w := newWireMR(t, serve(code, body(4097, 4090)), 5*time.Second)
			mr, err := w.get(context.Background())
			wantRefusal(t, mr, err)
			if strings.Contains(err.Error(), tok) || strings.Contains(err.Error(), tok[:6]) || strings.Contains(err.Error(), "xxxx") {
				t.Fatalf("token or body text in the refusal: %q", err.Error())
			}
			if w.rt.reqs.Load() != 1 {
				t.Fatalf("requests = %d, want 1", w.rt.reqs.Load())
			}
		})
	}
}

// Scope: only GetMergeRequest is marked; another method keeps today's behaviour.
func TestGitLabLimitDoesNotApplyToUnmarkedMethods(t *testing.T) {
	lowerGitLabLimit(t, 4096)
	var seenEnc atomic.Value
	w := newWireMR(t, func(rw http.ResponseWriter, r *http.Request) {
		seenEnc.Store(r.Header.Get("Accept-Encoding"))
		b := []byte(`{"id":1,"iid":3,"title":"t","state":"opened","labels":[],"updated_at":"2026-07-03T10:00:00Z"}`)
		b = append(b, bytes.Repeat([]byte(" "), 20000)...)
		_, _ = rw.Write(b)
	}, 5*time.Second)
	issue, err := w.d.GetIssue(context.Background(), 7, 3)
	if got, _ := seenEnc.Load().(string); got == "identity" {
		t.Fatalf("an unmarked request got Accept-Encoding %q", got)
	}
	if err != nil || issue.IID != 3 {
		t.Fatalf("GetIssue over the lowered limit: issue=%+v err=%v", issue, err)
	}
}
