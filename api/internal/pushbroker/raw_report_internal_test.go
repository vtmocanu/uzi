package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
)

type rawFixtureTransport struct {
	transport.Transport
	session transport.ReceivePackSession
}

func (t *rawFixtureTransport) NewReceivePackSession(*transport.Endpoint, transport.AuthMethod) (transport.ReceivePackSession, error) {
	return t.session, nil
}

type rawFixtureSession struct {
	Stdout   io.Reader
	calls    int
	closeErr error
}

func (*rawFixtureSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	ar := packp.NewAdvRefs()
	if err := ar.Capabilities.Set(capability.ReportStatus); err != nil {
		return nil, err
	}
	return ar, nil
}
func (s *rawFixtureSession) AdvertisedReferencesContext(context.Context) (*packp.AdvRefs, error) {
	return s.AdvertisedReferences()
}
func (*rawFixtureSession) Close() error { return nil }
func (s *rawFixtureSession) ReceivePack(_ context.Context, _ *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	s.calls++
	report := packp.NewReportStatus()
	if err := report.Decode(s.Stdout); err != nil {
		return nil, err
	}
	if err := report.Error(); err != nil {
		return report, err
	}
	return report, s.closeErr
}

// Embedding a compatible session deliberately does not expose the direct seam.
type unsupportedRawSession struct{ *rawFixtureSession }
type missingRawSession struct{ transport.ReceivePackSession }
type concreteRawSession struct {
	*rawFixtureSession
	Stdout *bytes.Reader
}

func rawFixtureForward(t *testing.T, ctx context.Context, sess transport.ReceivePackSession) (forwardPackResult, error) {
	t.Helper()
	const scheme = "rawsessionfixture"
	previous := client.Protocols[scheme]
	client.InstallProtocol(scheme, &rawFixtureTransport{session: sess})
	defer func() {
		if previous == nil {
			delete(client.Protocols, scheme)
		} else {
			client.InstallProtocol(scheme, previous)
		}
	}()
	remote, err := newOriginRemote(scheme + ":///fixture")
	if err != nil {
		t.Fatal(err)
	}
	return forwardPack(ctx, remote, nil, "refs/uzi-checkpoints/main", plumbing.ZeroHash, plumbing.NewHash(strings.Repeat("1", 40)), nil)
}

func rawFixtureWire(t *testing.T, lines ...string) []byte {
	t.Helper()
	var wire bytes.Buffer
	enc := pktline.NewEncoder(&wire)
	for _, line := range lines {
		if err := enc.EncodeString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}
	return wire.Bytes()
}

func TestForwardRawAttachmentFailsBeforeInvocation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session func(*rawFixtureSession) transport.ReceivePackSession
		want    string
	}{
		{"promoted seam", func(s *rawFixtureSession) transport.ReceivePackSession { return &unsupportedRawSession{s} }, "direct exported Stdout"},
		{"nonpointer session", func(s *rawFixtureSession) transport.ReceivePackSession { return unsupportedRawSession{s} }, "pointer to struct"},
		{"missing seam", func(s *rawFixtureSession) transport.ReceivePackSession { return &missingRawSession{s} }, "direct exported Stdout"},
		{"nil reader", func(s *rawFixtureSession) transport.ReceivePackSession { s.Stdout = nil; return s }, "nonnil io.Reader"},
		{"typed nil reader", func(s *rawFixtureSession) transport.ReceivePackSession { s.Stdout = (*bytes.Reader)(nil); return s }, "nonnil io.Reader"},
		{"concrete field", func(s *rawFixtureSession) transport.ReceivePackSession {
			return &concreteRawSession{s, bytes.NewReader(nil)}
		}, "not assignable"},
		{"nil session", func(*rawFixtureSession) transport.ReceivePackSession { return (*rawFixtureSession)(nil) }, "pointer to struct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &rawFixtureSession{Stdout: bytes.NewReader(nil)}
			result, err := rawFixtureForward(t, context.Background(), tc.session(base))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("attachment error = %v, want %q", err, tc.want)
			}
			if result.invoked || result.success || result.rejected || base.calls != 0 {
				t.Fatalf("unsupported session invoked: %+v calls=%d", result, base.calls)
			}
		})
	}
}

func TestForwardRawSuccessCloseCause(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	cause := &typedReportFailure{text: "https://credential@remote.invalid/" + ref + "\n\r\x00\u202e" + "glpat-" + strings.Repeat("E", 20)}
	for _, wording := range []string{"ordinary close failure", "missing workflow scope", "non-fast-forward"} {
		t.Run(wording, func(t *testing.T) {
			closeErr := fmt.Errorf("%s: %w", wording, cause)
			sess := &rawFixtureSession{Stdout: bytes.NewReader(rawFixtureWire(t, "unpack ok", "ok "+ref)), closeErr: closeErr}
			result, err := rawFixtureForward(t, context.Background(), sess)
			if !result.invoked || !result.success || result.rejected || !errors.Is(err, cause) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			var got *typedReportFailure
			if !errors.As(err, &got) || got != cause {
				t.Fatal("lost typed session error")
			}
			for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
				if !strings.Contains(rendered, "case=session_failure") || !strings.Contains(rendered, "marker=true") || !strings.Contains(rendered, "flush=true") {
					t.Fatalf("missing parser diagnostics: %q", rendered)
				}
				for _, secret := range []string{"credential", "remote.invalid", ref, "glpat-", "\n", "\r", "\x00", "\u202e", wording} {
					if strings.Contains(rendered, secret) {
						t.Fatalf("unsafe rendered error: %q", rendered)
					}
				}
			}
		})
	}
}

type chunkErrorReader struct {
	data     []byte
	chunk    int
	terminal error
	reads    int
	closes   int
}

func (r *chunkErrorReader) Read(p []byte) (int, error) {
	r.reads++
	n := min(len(p), len(r.data), r.chunk)
	copy(p, r.data[:n])
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.terminal
	}
	return n, nil
}
func (r *chunkErrorReader) Close() error { r.closes++; return r.terminal }

func TestRawReaderPreservesReadsAndClose(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	wire := rawFixtureWire(t, "unpack ok", "ng "+ref+" ok")
	cause := errors.New("reader terminal cause")
	for _, chunk := range []int{1, 2, 3, 7, len(wire)} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			original := &chunkErrorReader{data: append([]byte(nil), wire...), chunk: chunk, terminal: cause}
			report := &rawReport{ref: ref}
			reader := &rawReportReader{reader: original, report: report}
			var got bytes.Buffer
			buffer := make([]byte, len(wire))
			calls := 0
			for got.Len() < len(wire) {
				n, err := reader.Read(buffer)
				calls++
				got.Write(buffer[:n])
				if n != min(chunk, len(wire)-(got.Len()-n)) {
					t.Fatalf("changed n=%d", n)
				}
				if got.Len() == len(wire) {
					if err != cause {
						t.Fatalf("lost bytes-with-error cause: %v", err)
					}
				} else if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
			}
			if !bytes.Equal(got.Bytes(), wire) || original.reads != calls {
				t.Fatal("observer changed bytes or read independently")
			}
			if !report.complete() || report.marker != "ng" || report.reason != "ok" {
				t.Fatalf("raw marker lost: %+v", report)
			}
			if reader.Close() != cause || original.closes != 1 {
				t.Fatal("Close did not delegate")
			}
		})
	}
}

func TestRawReaderPacketStorageBound(t *testing.T) {
	// An attacker-controlled oversized packet cannot grow evidence storage.
	original := &chunkErrorReader{data: []byte("ffff" + strings.Repeat("x", 1<<20)), chunk: 4096, terminal: io.EOF}
	report := &rawReport{ref: "refs/uzi-checkpoints/main"}
	reader := &rawReportReader{reader: original, report: report}
	if _, err := io.Copy(io.Discard, reader); !errors.Is(err, errReportResponseLimit) {
		t.Fatalf("oversized response error = %v, want byte limit", err)
	}
	if report.complete() || !report.outer.invalid || cap(report.outer.payload) > 65520 || len(report.reason) > 65520 {
		t.Fatal("oversized packet retained evidence")
	}
	// A legal maximum-length packet is retained once, even across small reads.
	const ref = "refs/uzi-checkpoints/main"
	large := rawFixtureWire(t, "unpack ok", "ng "+ref+" "+strings.Repeat("x", 65516-len("ng "+ref+" ")-1))
	original = &chunkErrorReader{data: large, chunk: 7, terminal: io.EOF}
	report = &rawReport{ref: ref}
	reader = &rawReportReader{reader: original, report: report}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
	if !report.complete() || report.marker != "ng" || cap(report.outer.payload) > 65520 || len(report.reason) > 65520 {
		t.Fatal("maximum packet exceeded bounded storage or lost report")
	}
}

func TestForwardRawChunkedDecoder(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	for _, chunk := range []int{1, 2, 3, 7} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			sess := &rawFixtureSession{Stdout: &chunkErrorReader{
				data: rawFixtureWire(t, "unpack ok", "ng "+ref+" ok"), chunk: chunk, terminal: io.EOF,
			}}
			result, err := rawFixtureForward(t, context.Background(), sess)
			if !result.rejected || result.success || err == nil || result.reason != "ok" {
				t.Fatalf("chunked decoder result=%+v error=%v", result, err)
			}
		})
	}
}

func TestForwardRawCancelledBeforeInvocation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sess := &rawFixtureSession{Stdout: bytes.NewReader(nil)}
	result, err := rawFixtureForward(t, ctx, sess)
	if result.invoked || sess.calls != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled result=%+v calls=%d error=%v", result, sess.calls, err)
	}
}

func TestRawHTTPOptionsFailClosed(t *testing.T) {
	for _, ep := range []*transport.Endpoint{
		{ClientKey: []byte("key")}, {ClientCert: []byte("cert")}, {CaBundle: []byte("ca")},
		{InsecureSkipTLS: true}, {Proxy: transport.ProxyOptions{URL: "http://proxy.invalid"}},
		{Proxy: transport.ProxyOptions{Username: "user"}}, {Proxy: transport.ProxyOptions{Password: "password"}},
	} {
		if _, err := rawHTTPTransport(ep, &rawReport{}); err == nil {
			t.Fatal("endpoint options accepted")
		}
	}
}

type rawFixtureRoundTripper struct{ body io.ReadCloser }

func (t rawFixtureRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: t.body}, nil
}

func TestRawHTTPObservesOnlyReceivePackBody(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{http.MethodGet, "/origin.git/info/refs", false},
		{http.MethodPost, "/origin.git/git-upload-pack", false},
		{http.MethodPost, "/other.git/git-receive-pack", false},
		{http.MethodPost, "/origin.git/git-receive-pack", true},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			report := &rawReport{ref: "refs/uzi-checkpoints/main"}
			cause := errors.New("close cause")
			body := &chunkErrorReader{data: rawFixtureWire(t, "unpack ok", "ok "+report.ref), chunk: 1, terminal: io.EOF}
			rt := &rawReportRoundTripper{original: rawFixtureRoundTripper{body}, report: report}
			req, err := http.NewRequest(tc.method, "https://example.invalid"+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want {
				req = req.WithContext(context.WithValue(req.Context(), reportInvocationKey{}, report))
			}
			res, err := rt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			if (body.reads > 0) != tc.want {
				t.Fatal("incorrect eager attachment")
			}
			if _, err := io.Copy(io.Discard, res.Body); err != nil {
				t.Fatal(err)
			}
			if report.complete() != tc.want {
				t.Fatalf("observed wrong response: complete=%v", report.complete())
			}
			body.terminal = cause
			closeErr := res.Body.Close()
			if (tc.want && closeErr != nil) || (!tc.want && closeErr != cause) || body.closes != 1 {
				t.Fatal("body Close did not delegate")
			}
		})
	}
}
