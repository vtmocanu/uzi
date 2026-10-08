package pushbroker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// packetObserver retains at most one protocol-sized packet. Every input byte is
// visited once; malformed input disables evidence without interrupting the reader.
type packetObserver struct {
	have    int
	length  int
	payload []byte
	invalid bool
}

func (p *packetObserver) feed(data []byte, packet func([]byte)) {
	for len(data) > 0 && !p.invalid {
		if p.have < 4 {
			b := data[0]
			data = data[1:]
			var digit int
			switch {
			case b >= '0' && b <= '9':
				digit = int(b - '0')
			case b >= 'a' && b <= 'f':
				digit = int(b-'a') + 10
			default:
				p.invalid = true
				return
			}
			p.have++
			p.length = p.length*16 + digit
			if p.have < 4 {
				continue
			}
			if p.length == 0 {
				packet(nil)
				p.have, p.length = 0, 0
				continue
			}
			if p.length <= 4 || p.length > 65520 {
				p.invalid = true
				return
			}
			p.length -= 4
			if cap(p.payload) < p.length {
				p.payload = make([]byte, 0, p.length)
			} else {
				p.payload = p.payload[:0]
			}
		}
		take := min(p.length-len(p.payload), len(data))
		p.payload = append(p.payload, data[:take]...)
		data = data[take:]
		if len(p.payload) == p.length {
			packet(p.payload)
			p.have, p.length = 0, 0
			p.payload = p.payload[:0]
		}
	}
}

type rawReport struct {
	ref         string
	sideband    bool
	sideband64k bool
	outer       packetObserver
	inner       packetObserver
	invalid     bool
	exhausted   bool
	flushed     bool
	unpack      string
	marker      string
	reason      string
	terminal    bool
	http        bool
	attached    bool
	eligible    bool
	finalized   bool
	eof         bool
	bytes       int
	readErr     error
	closeErr    error
	finalErr    error
}

func (r *rawReport) feed(data []byte) {
	r.outer.feed(data, func(payload []byte) {
		if r.terminal {
			r.invalid = true
			return
		}
		if !r.sideband {
			r.reportPacket(payload)
			return
		}
		if payload == nil {
			// An outer flush is never evidence of the inner report's completion.
			if !r.flushed {
				r.invalid = true
			}
			r.terminal = true
			return
		}
		limit := sideband.MaxPackedSize
		if r.sideband64k {
			limit = sideband.MaxPackedSize64k
		}
		if len(payload) == 0 || len(payload) > limit {
			r.invalid = true
			return
		}
		switch payload[0] {
		case 1:
			if r.flushed {
				r.invalid = true
				return
			}
			r.inner.feed(payload[1:], r.reportPacket)
		case 2: // Progress is not report evidence.
		default:
			r.invalid = true // Includes fatal channel 3.
		}
	})
}

func (r *rawReport) reportPacket(payload []byte) {
	if r.flushed {
		r.invalid = true
		return
	}
	if payload == nil {
		r.flushed = true
		if r.unpack == "" {
			r.invalid = true
		}
		return
	}
	line := strings.TrimSuffix(string(payload), "\n")
	if strings.ContainsAny(line, "\r\n\x00") {
		r.invalid = true
		return
	}
	if strings.HasPrefix(line, "unpack ") {
		if r.unpack != "" || r.marker != "" {
			r.invalid = true
			return
		}
		r.unpack = strings.TrimPrefix(line, "unpack ")
		if r.unpack == "" {
			r.invalid = true
		}
		return
	}
	if r.unpack == "" || r.marker != "" {
		r.invalid = true
		return
	}
	if line == "ok "+r.ref {
		r.marker = "ok"
		return
	}
	if strings.HasPrefix(line, "ng "+r.ref+" ") {
		r.marker = "ng"
		r.reason = strings.TrimPrefix(line, "ng "+r.ref+" ")
		if r.reason == "" {
			r.invalid = true
		}
		return
	}
	r.invalid = true
}

func (r *rawReport) complete() bool {
	return !r.invalid && !r.outer.invalid && !r.inner.invalid &&
		r.outer.have == 0 && r.inner.have == 0 && r.flushed && (!r.sideband || r.terminal)
}

// maxReportResponseBytes caps the actual single-command receive-pack response at
// 1 MiB, including pkt-line framing and every sideband channel. This bounds the
// bytes reaching go-git, whose report decoder retains each command status.
const maxReportResponseBytes = 1 << 20

var errReportResponseLimit = errors.New("pushbroker: receive-pack response byte limit exhausted (1 MiB)")

type rawReportReader struct {
	reader io.Reader
	report *rawReport
	read   int
}

func (r *rawReportReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := maxReportResponseBytes - r.read
	if remaining == 0 {
		r.report.invalid, r.report.exhausted = true, true
		return 0, errReportResponseLimit
	}
	// Never probe or drain beyond the budget, even to distinguish EOF at the
	// boundary. Reaching the boundary conservatively invalidates prior evidence.
	n, err := r.reader.Read(p[:min(len(p), remaining)])
	r.read += n
	r.report.bytes += n
	r.report.feed(p[:n])
	if r.read == maxReportResponseBytes {
		r.report.invalid, r.report.exhausted = true, true
		return n, errors.Join(err, errReportResponseLimit)
	}
	return n, err
}

func (r *rawReportReader) Close() error {
	if c, ok := r.reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// attachRawStdout accepts only the direct exported reader seam used by file/SSH
// compatible sessions. It never traverses embedded/private fields or alters the
// session's command lifecycle. Unsupported shapes fail before ReceivePack.
func attachRawStdout(sess transport.ReceivePackSession, report *rawReport) error {
	v := reflect.ValueOf(sess)
	if !v.IsValid() || v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("pushbroker: raw stdout attachment requires pointer to struct (got %T)", sess)
	}
	v = v.Elem()
	field, ok := v.Type().FieldByName("Stdout")
	if !ok || len(field.Index) != 1 || field.PkgPath != "" {
		return fmt.Errorf("pushbroker: raw stdout attachment requires direct exported Stdout (got %T)", sess)
	}
	stdout := v.FieldByIndex(field.Index)
	if !stdout.CanSet() || !stdout.CanInterface() {
		return fmt.Errorf("pushbroker: raw stdout attachment requires accessible Stdout (got %T)", sess)
	}
	reader, ok := stdout.Interface().(io.Reader)
	if !ok || nilReflectValue(reflect.ValueOf(reader)) {
		return fmt.Errorf("pushbroker: raw stdout attachment requires nonnil io.Reader (got %T)", sess)
	}
	wrapper := reflect.ValueOf(&rawReportReader{reader: reader, report: report})
	if !wrapper.Type().AssignableTo(stdout.Type()) {
		return fmt.Errorf("pushbroker: raw stdout wrapper is not assignable to %s (session %T)", stdout.Type(), sess)
	}
	stdout.Set(wrapper)
	return nil
}

func nilReflectValue(v reflect.Value) bool {
	if !v.IsValid() {
		return true
	}
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

type rawReportRoundTripper struct {
	original http.RoundTripper
	report   *rawReport
}

// reportInvocationKey is private to the observer; only receiveObservedPack marks
// the context of this invocation. Discovery redirects may canonicalize the URL.
type reportInvocationKey struct{}

func (t *rawReportRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	t.report.http = true
	res, err := t.original.RoundTrip(req)
	if req.Method != http.MethodPost || req.Context().Value(reportInvocationKey{}) != t.report ||
		!strings.HasSuffix(req.URL.Path, "/"+transport.ReceivePackServiceName) {
		return res, err
	}
	if res == nil || res.Body == nil {
		t.report.finalErr = err
		return res, err
	}
	r := t.report
	r.attached = true
	r.eligible = res.StatusCode == http.StatusOK && err == nil
	original := res.Body
	reader := &rawReportReader{reader: original, report: r}
	var replay bytes.Buffer
	buffer := make([]byte, 32<<10)
	// The byte cap bounds productive reads; 100 consecutive empty reads bound a
	// broken reader. The original request context bounds actual HTTP body stalls.
	idle := 0
	for {
		if cause := req.Context().Err(); cause != nil {
			r.readErr = cause
			break
		}
		n, cause := reader.Read(buffer)
		replay.Write(buffer[:n])
		if cause != nil {
			if cause == io.EOF {
				r.eof = true
			} else {
				r.readErr = cause
			}
			break
		}
		if n == 0 {
			idle++
		} else {
			idle = 0
		}
		if idle == 100 {
			r.readErr = io.ErrNoProgress
			break
		}
	}
	r.closeErr = original.Close()
	r.finalErr = errors.Join(err, r.readErr, r.closeErr, req.Context().Err())
	r.finalized = r.eof && r.finalErr == nil && !r.exhausted
	// Replays contain at most maxReportResponseBytes and never observe twice.
	res.Body = io.NopCloser(bytes.NewReader(replay.Bytes()))
	return res, err
}

func rawHTTPTransport(ep *transport.Endpoint, report *rawReport) (transport.Transport, error) {
	// forwardPack derives ep exclusively through NewEndpoint(URL). Guard options
	// which would cause go-git to assert the wrapped RoundTripper is *http.Transport.
	if len(ep.ClientKey) != 0 || len(ep.ClientCert) != 0 || len(ep.CaBundle) != 0 ||
		ep.InsecureSkipTLS || ep.Proxy != (transport.ProxyOptions{}) {
		return nil, errors.New("pushbroker: raw HTTP observer does not support endpoint TLS/proxy options")
	}
	report.http = true
	copied := *brokerHTTPClient
	copied.Transport = &rawReportRoundTripper{
		original: brokerHTTPClient.Transport,
		report:   report,
	}
	return githttp.NewClient(&copied), nil
}
