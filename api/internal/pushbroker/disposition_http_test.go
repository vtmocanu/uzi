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
// report decoder. Sideband responses are unnegotiated protocol errors: Publish
// preserves go-git's default capability selection even when sideband is advertised.
func TestPublishHTTPDisposition(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	flood := append([]string{"unpack ok"}, strings.Split(strings.TrimSuffix(strings.Repeat("ok "+ref+"\n", 100000), "\n"), "\n")...)
	cases := []struct {
		name         string
		lines        []string
		flush        bool
		want         pushbroker.PublishDisposition
		wantError    bool
		mapped       error
		sideband     bool
		outerOnly    bool
		progressOnly bool
		fatal        bool
		raw          string
	}{
		{name: "response byte budget", lines: flood, flush: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "acknowledged", lines: []string{"unpack ok", "ok " + ref}, flush: true, want: pushbroker.PublishAdvanced},
		{name: "rejection reason exactly ok", lines: []string{"unpack ok", "ng " + ref + " ok"}, flush: true, wantError: true},
		{name: "unfamiliar rejection", lines: []string{"unpack ok", "ng " + ref + " novel policy"}, flush: true, wantError: true},
		{name: "unpack rejection", lines: []string{"unpack invalid fixture"}, flush: true, wantError: true},
		{name: "known workflow rejection", lines: []string{"unpack ok", "ng " + ref + " missing workflow scope"}, flush: true, wantError: true, mapped: pushbroker.ErrWorkflowScopeRejected},
		{name: "known non-fast-forward rejection", lines: []string{"unpack ok", "ng " + ref + " non-fast-forward"}, flush: true, wantError: true, mapped: pushbroker.ErrNotDescendant},
		{name: "duplicate success", lines: []string{"unpack ok", "ok " + ref, "ok " + ref}, flush: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "conflicting markers", lines: []string{"unpack ok", "ok " + ref, "ng " + ref + " ok"}, flush: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "duplicate unpack", lines: []string{"unpack ok", "unpack ok", "ok " + ref}, flush: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "overflow packet", raw: "ffff", want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "malformed header", raw: "zzzz", want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "empty packet is not flush", raw: "000dunpack ok0021ok refs/uzi-checkpoints/main\n0004", want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "partial header", raw: "00", want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "sideband split headers", lines: []string{"unpack ok", "ok " + ref}, flush: true, sideband: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "sideband ng ok", lines: []string{"unpack ok", "ng " + ref + " ok"}, flush: true, sideband: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "outer flush only", lines: []string{"unpack ok", "ok " + ref}, sideband: true, outerOnly: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "progress spoof", lines: []string{"unpack ok", "ok " + ref}, flush: true, sideband: true, progressOnly: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
		{name: "fatal sideband", lines: []string{"unpack ok", "ok " + ref}, flush: true, sideband: true, fatal: true, want: pushbroker.PublishOutcomeUnknown, wantError: true},
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
			if tc.raw != "" {
				response.Reset()
				response.WriteString(tc.raw)
			}
			if tc.sideband {
				inner := append([]byte(nil), response.Bytes()...)
				response.Reset()
				enc := pktline.NewEncoder(&response)
				// Every inner header crosses outer packet boundaries.
				for _, b := range inner {
					channel := byte(1)
					if tc.progressOnly {
						channel = 2
					}
					if tc.fatal {
						channel = 3
					}
					if err := enc.Encode([]byte{channel, b}); err != nil {
						t.Fatal(err)
					}
					if err := enc.Encode([]byte("\x02ok " + ref + "\n")); err != nil {
						t.Fatal(err)
					}
				}
				if tc.outerOnly || tc.progressOnly {
					if err := enc.Flush(); err != nil {
						t.Fatal(err)
					}
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
					if tc.sideband {
						if err := ar.Capabilities.Set(capability.Sideband64k); err != nil {
							t.Error(err)
							return
						}
					}
					if err := ar.Encode(w); err != nil {
						t.Error(err)
					}
					return
				}
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-receive-pack") {
					invocations.Add(1)
					scanner := pktline.NewScanner(r.Body)
					if !scanner.Scan() {
						t.Errorf("missing receive-pack command: %v", scanner.Err())
						return
					}
					if strings.Contains(string(scanner.Bytes()), "side-band") {
						t.Error("Publish selected an optional sideband capability")
					}
					if _, err := io.Copy(io.Discard, r.Body); err != nil {
						t.Error(err)
						return
					}
					w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
					if _, err := w.Write(response.Bytes()); err != nil && tc.name != "response byte budget" {
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
			if tc.name == "response byte budget" && (err == nil || !strings.Contains(err.Error(), "response byte limit")) {
				t.Errorf("missing budget cause: %v", err)
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
