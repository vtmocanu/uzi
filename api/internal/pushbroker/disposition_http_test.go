package pushbroker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
)

// This loopback server exercises the broker's real HTTP transport and go-git's
// report decoder. It needs no git-http-backend and sends no sideband framing.
func TestPublishHTTPDisposition(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	cases := []struct {
		name      string
		lines     []string
		flush     bool
		want      pushbroker.PublishDisposition
		wantError bool
		mapped    error
	}{
		{name: "acknowledged", lines: []string{"unpack ok", "ok " + ref}, flush: true, want: pushbroker.PublishAdvanced},
		{name: "unfamiliar rejection", lines: []string{"unpack ok", "ng " + ref + " novel policy"}, flush: true, wantError: true},
		{name: "unpack rejection", lines: []string{"unpack invalid fixture"}, flush: true, wantError: true},
		{name: "known workflow rejection", lines: []string{"unpack ok", "ng " + ref + " missing workflow scope"}, flush: true, wantError: true, mapped: pushbroker.ErrWorkflowScopeRejected},
		{name: "known non-fast-forward rejection", lines: []string{"unpack ok", "ng " + ref + " non-fast-forward"}, flush: true, wantError: true, mapped: pushbroker.ErrNotDescendant},
		{name: "missing response", want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "unpack only", lines: []string{"unpack ok"}, flush: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "wrong ref", lines: []string{"unpack ok", "ok refs/heads/main"}, flush: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "wrong ref workflow rejection", lines: []string{"unpack ok", "ng refs/heads/main missing workflow scope"}, flush: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "wrong ref non-fast-forward rejection", lines: []string{"unpack ok", "ng refs/heads/main non-fast-forward"}, flush: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "truncated acknowledgement", lines: []string{"unpack ok", "ok " + ref}, want: pushbroker.PublishOutcomeUnknown, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGitFixture(t)
			tip := f.commit("tip.txt", "tip\n", "tip")
			pack := []byte(f.git("pack-objects", "--all", "--stdout"))
			var response bytes.Buffer
			encoder := pktline.NewEncoder(&response)
			for _, line := range tc.lines {
				if err := encoder.EncodeString(line + "\n"); err != nil {
					t.Fatal(err)
				}
			}
			if tc.flush {
				if err := encoder.Flush(); err != nil {
					t.Fatal(err)
				}
			}
			var invocations atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") {
					service := r.URL.Query().Get("service")
					w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
					enc := pktline.NewEncoder(w)
					if err := enc.EncodeString("# service=" + service + "\n"); err != nil {
						t.Error(err)
						return
					}
					if err := enc.Flush(); err != nil {
						t.Error(err)
						return
					}
					ar := packp.NewAdvRefs()
					if err := ar.Capabilities.Set(capability.ReportStatus); err != nil {
						t.Error(err)
						return
					}
					if err := ar.Encode(w); err != nil {
						t.Error(err)
					}
					return
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
					invocations.Add(1)
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
					if _, err := w.Write(response.Bytes()); err != nil {
						t.Error(err)
					}
					return
				}
				t.Errorf("unexpected broker request: %s %s", r.Method, r.URL)
				http.Error(w, "unexpected request", http.StatusNotFound)
			}))
			defer server.Close()
			res, err := pushbroker.Publish(context.Background(), pushbroker.Options{
				CloneURL: server.URL + "/origin.git", Branch: "main", DeclaredTip: tip, Pack: pack,
			})
			if (err != nil) != tc.wantError {
				t.Errorf("error = %v, wantError %v", err, tc.wantError)
			}
			if tc.mapped != nil && !errors.Is(err, tc.mapped) {
				t.Errorf("error = %v, want %v", err, tc.mapped)
			}
			if tc.want == pushbroker.PublishOutcomeUnknown && (errors.Is(err, pushbroker.ErrNotDescendant) || errors.Is(err, pushbroker.ErrWorkflowScopeRejected)) {
				t.Errorf("unknown outcome mapped to definitive rejection: %v", err)
			}
			if res.Disposition != tc.want {
				t.Errorf("Disposition = %v, want %v", res.Disposition, tc.want)
			}
			if invocations.Load() != 1 {
				t.Errorf("HTTP receive-pack invocations = %d, want 1", invocations.Load())
			}
		})
	}
}
