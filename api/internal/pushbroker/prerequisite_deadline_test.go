package pushbroker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/google/uuid"
)

type prereqDeadlineCall struct {
	stage     string
	remaining time.Duration
}
type prereqDeadlineTransport struct {
	ref, tip                    string
	prepared, present, deleting bool
	calls                       []prereqDeadlineCall
}

func (f *prereqDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	stage := r.Method + " " + r.URL.Query().Get("service")
	if r.Method == http.MethodPost {
		stage = r.Method + " receive"
	}
	if f.prepared {
		d, ok := r.Context().Deadline()
		left := time.Duration(-1)
		if ok {
			left = time.Until(d)
		}
		f.calls = append(f.calls, prereqDeadlineCall{stage, left})
	}
	var body bytes.Buffer
	enc := pktline.NewEncoder(&body)
	service := r.URL.Query().Get("service")
	contentType := "application/x-" + service + "-advertisement"
	if r.Method == http.MethodPost {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return nil, err
		}
		f.present = !f.deleting
		contentType = "application/x-git-receive-pack-result"
		_ = enc.EncodeString("unpack ok\n")
		_ = enc.EncodeString("ok " + f.ref + "\n")
		_ = enc.Flush()
	} else {
		_ = enc.EncodeString("# service=" + service + "\n")
		_ = enc.Flush()
		_ = enc.EncodeString(f.tip + " refs/heads/main\x00report-status delete-refs\n")
		if f.present {
			_ = enc.EncodeString(f.tip + " " + f.ref + "\n")
		}
		_ = enc.Flush()
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(bytes.NewReader(body.Bytes())), Request: r}, nil
}

// Receive discovery/send share one second; readback gets its own child second.
// Delete's discovery, receive/send and readback are capped too, even when the
// caller supplies a much larger budget. This observes actual driver requests.
func TestPrereqTransportOperationDeadlines(t *testing.T) {
	for _, budget := range []time.Duration{8 * time.Second, 500 * time.Millisecond} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			id := uuid.New()
			f := &prereqDeadlineTransport{ref: PrereqRef(id), tip: strings.Repeat("a", 40)}
			original := brokerHTTPClient.Transport
			brokerHTTPClient.Transport = f
			defer func() { brokerHTTPClient.Transport = original }()
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			p, err := PreparePrereqCreate(ctx, PrereqCreateOptions{CloneURL: "https://example.invalid/repo", DefaultBranch: "main", CaptureID: id})
			if err != nil {
				t.Fatal(err)
			}
			f.prepared = true
			out, err := SendPrereqCreate(ctx, p)
			if err != nil || out != PrereqCreateCreated {
				t.Fatalf("create=%v err=%v", out, err)
			}
			if len(f.calls) != 3 {
				t.Fatalf("create requests=%d, want receive advertisement/send/readback", len(f.calls))
			}
			f.deleting = true
			if err := Delete(ctx, DeleteOptions{CloneURL: "https://example.invalid/repo", Ref: f.ref, ExpectedOldTip: f.tip}); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 7 {
				t.Fatalf("total requests=%d, want three create plus four delete", len(f.calls))
			}
			for i, c := range f.calls {
				if c.remaining <= 0 || c.remaining > min(time.Second, budget) {
					t.Fatalf("operation[%d] %s deadline=%s, want positive <=%s", i, c.stage, c.remaining, min(time.Second, budget))
				}
			}
		})
	}
}
