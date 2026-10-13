package forge

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

const (
	ciRepairJSONBytes = 4 << 20
	ciRepairFireBytes = 16 << 20
)

// ErrCIReadBudget identifies incomplete CI discovery evidence.
var ErrCIReadBudget = errors.New("CI discovery read budget exceeded")

// CIReadBudgetError contains only a fixed, trusted reason, never forge content.
type CIReadBudgetError struct{ reason string }

func (e *CIReadBudgetError) Error() string { return ErrCIReadBudget.Error() + ": " + e.reason }
func (e *CIReadBudgetError) Unwrap() error { return ErrCIReadBudget }

type ciRepairBudget struct {
	mu        sync.Mutex
	remaining int64
	err       *CIReadBudgetError
}
type ciRepairBudgetKey struct{}
type ciRepairJSONKey struct{}

// WithCIRepairReadLimits installs one shared discovery budget for a CI-watch fire.
// Derived and cloned contexts retain it. Workflow/job methods opt in individually;
// log tails and other forge reads do not consume it.
func WithCIRepairReadLimits(ctx context.Context) context.Context {
	return context.WithValue(ctx, ciRepairBudgetKey{}, &ciRepairBudget{remaining: ciRepairFireBytes})
}

func ciBudget(ctx context.Context) *ciRepairBudget {
	b, _ := ctx.Value(ciRepairBudgetKey{}).(*ciRepairBudget)
	return b
}

func markCIJSON(ctx context.Context) context.Context {
	if ciBudget(ctx) == nil {
		return ctx
	}
	return context.WithValue(ctx, ciRepairJSONKey{}, true)
}

func isCIJSON(ctx context.Context) bool {
	marked, _ := ctx.Value(ciRepairJSONKey{}).(bool)
	return marked && ciBudget(ctx) != nil
}

func (b *ciRepairBudget) fail(reason string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err == nil {
		b.err = &CIReadBudgetError{reason: reason}
	}
	return b.err
}

func ciReadError(ctx context.Context, err error) error {
	if b := ciBudget(ctx); b != nil {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.err != nil {
			return b.err
		}
	}
	return err
}

func validateCIWorkflow(ctx context.Context, run WorkflowRun) error {
	if b := ciBudget(ctx); b != nil && (run.ID <= 0 || run.WorkflowKey == "" || len(run.WorkflowKey) > 200) {
		return b.fail("invalid workflow identity")
	}
	return ciReadError(ctx, nil)
}

func validateCIWorkflows(ctx context.Context, runs []WorkflowRun) ([]WorkflowRun, error) {
	for _, run := range runs {
		if err := validateCIWorkflow(ctx, run); err != nil {
			return nil, err
		}
	}
	if err := ciReadError(ctx, nil); err != nil {
		return nil, err
	}
	return runs, nil
}

// ciRepairTransport sits around the existing protected transport. Marked JSON is
// fully read through a bounded reader before SDK decoding, including error bodies,
// retries and bytes a decoder would otherwise leave for a drain. Close never drains
// the underlying stream. Unmarked calls pass through unchanged.
type ciRepairTransport struct{ base http.RoundTripper }

func (t ciRepairTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !isCIJSON(req.Context()) {
		return t.base.RoundTrip(req)
	}
	b := ciBudget(req.Context())
	if err := ciReadError(req.Context(), nil); err != nil {
		return nil, err
	}
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	body := resp.Body
	defer func() { _ = body.Close() }()
	var reader io.Reader = resp.Body
	switch resp.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		z, zerr := gzip.NewReader(resp.Body)
		if zerr != nil {
			return nil, b.fail("invalid gzip discovery body")
		}
		defer func() { _ = z.Close() }()
		reader = z
	default:
		return nil, b.fail("unsupported discovery encoding")
	}
	data, err := io.ReadAll(&ciRepairReader{reader: reader, budget: b, remaining: ciRepairJSONBytes})
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(data))
	resp.ContentLength = int64(len(data))
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.Uncompressed = true
	return resp, nil
}

type ciRepairReader struct {
	reader    io.Reader
	budget    *ciRepairBudget
	remaining int64
}

func (r *ciRepairReader) Read(p []byte) (int, error) {
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return 0, b.err
	}
	allowance := min(r.remaining, b.remaining)
	if int64(len(p)) > allowance+1 {
		p = p[:allowance+1]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	b.remaining -= int64(n)
	if int64(n) > allowance {
		b.err = &CIReadBudgetError{reason: "decompressed JSON bytes"}
		return 0, b.err
	}
	return n, err
}
