package pushbroker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// MaxRefAdvertisementBytes bounds ListRefTips discovery input before go-git's
// parser allocates reference maps. Packet framing and capabilities count too.
const MaxRefAdvertisementBytes = 1 << 20

// ErrRefAdvertisementTooLarge means the lookup cannot prove presence or absence.
// No partial tips are returned when discovery exceeds MaxRefAdvertisementBytes.
var ErrRefAdvertisementTooLarge = errors.New("pushbroker: ref advertisement exceeds 1 MiB")

type refAdvertisementTransport struct{ original http.RoundTripper }

func (t refAdvertisementTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return t.original.RoundTrip(req)
	}
	res, err := t.original.RoundTrip(req)
	if err != nil || res == nil || res.Body == nil {
		return res, err
	}
	// Bound actual (including automatically decompressed) bytes, never the
	// server's Content-Length. Reading one extra byte distinguishes exact size
	// from overflow. The request context retains the owner's time ceiling.
	body, readErr := io.ReadAll(io.LimitReader(res.Body, MaxRefAdvertisementBytes+1))
	closeErr := res.Body.Close()
	if len(body) > MaxRefAdvertisementBytes {
		return nil, ErrRefAdvertisementTooLarge
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	res.Body = io.NopCloser(bytes.NewReader(body))
	return res, nil
}

// refAdvertisementReader bounds the existing command stdout parser seam for
// file/SSH transports. A discovery parser stops at the protocol flush packet;
// it must not drain stdout to EOF while the command awaits another request.
type refAdvertisementReader struct {
	reader io.Reader
	left   int
}

func (r *refAdvertisementReader) Read(p []byte) (int, error) {
	if r.left == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n != 0 {
			return 0, ErrRefAdvertisementTooLarge
		}
		return 0, err
	}
	if len(p) > r.left {
		p = p[:r.left]
	}
	n, err := r.reader.Read(p)
	r.left -= n
	return n, err
}

func boundRefStdout(sess any) error {
	v := reflect.ValueOf(sess)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return errors.New("pushbroker: ref discovery requires stdout reader seam")
	}
	v = v.Elem()
	field, ok := v.Type().FieldByName("Stdout")
	if !ok || len(field.Index) != 1 || field.PkgPath != "" {
		return errors.New("pushbroker: ref discovery requires direct exported Stdout")
	}
	stdout := v.FieldByIndex(field.Index)
	if !stdout.CanSet() || !stdout.CanInterface() {
		return errors.New("pushbroker: ref discovery requires accessible Stdout")
	}
	reader, ok := stdout.Interface().(io.Reader)
	if !ok || nilReflectValue(reflect.ValueOf(reader)) {
		return errors.New("pushbroker: ref discovery requires nonnil stdout reader")
	}
	wrapper := reflect.ValueOf(&refAdvertisementReader{reader: reader, left: MaxRefAdvertisementBytes})
	if !wrapper.Type().AssignableTo(stdout.Type()) {
		return errors.New("pushbroker: ref discovery requires assignable stdout reader")
	}
	stdout.Set(wrapper)
	return nil
}

func boundedRefTips(ctx context.Context, url string, auth transport.AuthMethod, want map[plumbing.ReferenceName]bool) (out map[string]string, err error) {
	ep, err := transport.NewEndpoint(url)
	if err != nil {
		return nil, err
	}
	var c transport.Transport
	if ep.Protocol == "http" || ep.Protocol == "https" {
		// NewEndpoint(URL) cannot set TLS/proxy options requiring go-git's
		// concrete *http.Transport assertion. Retain the broker pool and policy.
		copied := *brokerHTTPClient
		copied.CheckRedirect = func(req *http.Request, via []*http.Request) error { return checkRedirect(ctx, req, via) }
		copied.Transport = refAdvertisementTransport{original: brokerHTTPClient.Transport}
		c = githttp.NewClient(&copied)
	} else {
		c, err = transportFor(ep)
		if err != nil {
			return nil, err
		}
	}
	sess, err := c.NewUploadPackSession(ep, auth)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, sess.Close())
		if err != nil {
			out = nil
		}
	}()
	if ep.Protocol != "http" && ep.Protocol != "https" {
		if err := boundRefStdout(sess); err != nil {
			return nil, err
		}
	}
	adv, err := sess.AdvertisedReferencesContext(ctx)
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pushbroker: list: %w", err)
	}
	all, err := adv.AllReferences()
	if err != nil {
		return nil, err
	}
	out = make(map[string]string, len(want))
	for name, ref := range all {
		if want[name] {
			out[name.String()] = ref.Hash().String()
		}
	}
	return out, nil
}
