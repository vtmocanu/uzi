package pushbroker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// These requests explicitly select sideband in test-owned sessions. Publish's
// default capability selection remains covered by TestPublishHTTPDisposition.
func TestNegotiatedSidebandHTTP(t *testing.T) {
	const ref = "refs/uzi-checkpoints/main"
	for _, selected := range []capability.Capability{capability.Sideband, capability.Sideband64k} {
		t.Run(selected.String(), func(t *testing.T) {
			for _, tc := range []struct {
				name       string
				line       string
				channel    byte
				innerFlush bool
				complete   bool
				success    bool
				rejected   bool
				wantError  bool
			}{
				{"valid progress tail", "ok " + ref, 1, true, true, true, false, false},
				{"oversized progress tail", "ok " + ref, 1, true, false, false, false, true},
				{"fatal tail", "ok " + ref, 1, true, false, false, false, true},
				{"data tail", "ok " + ref, 1, true, false, false, false, true},
				{"progress byte budget", "ok " + ref, 1, true, false, false, false, true},
				{"duplicate outer terminator", "ok " + ref, 1, true, false, false, false, true},
				{"progress after outer terminator", "ok " + ref, 1, true, false, false, false, true},
				{"report after inner completion", "ok " + ref, 1, true, false, false, false, true},
				{"partial outer header", "ok " + ref, 1, true, false, false, false, true},
				{"partial outer payload", "ok " + ref, 1, true, false, false, false, true},
				{"unknown channel", "ok " + ref, 1, true, false, false, false, true},
				{"missing outer terminator", "ok " + ref, 1, true, false, false, false, true},
				{"split headers", "ok " + ref, 1, true, true, true, false, false},
				{"ng reason exactly ok", "ng " + ref + " ok", 1, true, true, false, true, true},
				{"outer flush only", "ok " + ref, 1, false, false, false, false, true},
				{"progress spoof", "ok " + ref, 2, true, false, false, false, true},
				{"fatal channel", "ok " + ref, 3, true, false, false, false, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					inner := rawFixtureWire(t, "unpack ok", tc.line)
					if !tc.innerFlush {
						inner = inner[:len(inner)-4]
					}
					var wire bytes.Buffer
					if tc.name == "progress byte budget" {
						wire.WriteString(strings.Repeat("0006\x02x", (1<<20)/6+1))
					}
					enc := pktline.NewEncoder(&wire)
					for _, b := range inner {
						// Split every inner header across outer packets. Progress
						// packets carry a spoofed acknowledgement between bytes.
						if err := enc.Encode([]byte{tc.channel, b}); err != nil {
							t.Fatal(err)
						}
						if err := enc.Encode([]byte("\x02ok " + ref + "\n")); err != nil {
							t.Fatal(err)
						}
					}
					if err := enc.Flush(); err != nil {
						t.Fatal(err)
					}
					switch tc.name {
					case "valid progress tail", "oversized progress tail", "fatal tail", "data tail":
						b := append([]byte(nil), wire.Bytes()[:wire.Len()-4]...)
						wire.Reset()
						wire.Write(b)
						size := 1000 // Includes the channel byte, as in go-git Demuxer.
						if selected == capability.Sideband64k {
							size = 65516 // Maximum payload under canonical pkt-line framing.
						}
						channel := byte(2)
						if tc.name == "oversized progress tail" {
							size++
						}
						if tc.name == "fatal tail" {
							channel = 3
						}
						if tc.name == "data tail" {
							channel = 1
						}
						fmt.Fprintf(&wire, "%04x", size+4)
						wire.WriteByte(channel)
						wire.WriteString(strings.Repeat("x", size-1))
						wire.WriteString("0000")
					case "duplicate outer terminator":
						wire.WriteString("0000")
					case "progress after outer terminator":
						wire.WriteString("0006\x02x")
					case "report after inner completion":
						b := append([]byte(nil), wire.Bytes()[:wire.Len()-4]...)
						wire.Reset()
						wire.Write(b)
						wire.WriteString("0006\x01x0000")
					case "partial outer header":
						wire.WriteString("00")
					case "partial outer payload":
						wire.WriteString("0006\x02")
					case "unknown channel":
						b := append([]byte(nil), wire.Bytes()[:wire.Len()-4]...)
						wire.Reset()
						wire.Write(b)
						wire.WriteString("0006\x04x0000")
					case "missing outer terminator":
						b := append([]byte(nil), wire.Bytes()[:wire.Len()-4]...)
						wire.Reset()
						wire.Write(b)
					}
					t.Run("observer bytes", func(t *testing.T) {
						raw := &rawReport{ref: ref, sideband: true, sideband64k: selected == capability.Sideband64k}
						for _, b := range wire.Bytes() {
							raw.feed([]byte{b})
						}
						if raw.complete() != (tc.complete || tc.name == "progress byte budget") {
							t.Fatalf("complete=%v, want %v", raw.complete(), tc.complete)
						}
						if tc.complete && (raw.marker != strings.Fields(tc.line)[0] || (tc.rejected && raw.reason != "ok")) {
							t.Fatalf("observed marker=%q reason=%q", raw.marker, raw.reason)
						}
					})
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodGet && r.URL.Path == "/origin.git/info/refs":
							w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
							advertisement := pktline.NewEncoder(w)
							if err := advertisement.EncodeString("# service=git-receive-pack\n"); err != nil {
								t.Error(err)
								return
							}
							if err := advertisement.Flush(); err != nil {
								t.Error(err)
								return
							}
							ar := packp.NewAdvRefs()
							for _, cap := range []capability.Capability{capability.ReportStatus, selected} {
								if err := ar.Capabilities.Set(cap); err != nil {
									t.Error(err)
									return
								}
							}
							if err := ar.Encode(w); err != nil {
								t.Error(err)
							}
						case r.Method == http.MethodPost && r.URL.Path == "/origin.git/git-receive-pack":
							scanner := pktline.NewScanner(r.Body)
							if !scanner.Scan() || !strings.Contains(string(scanner.Bytes()), selected.String()) {
								t.Error("test request did not select sideband")
								return
							}
							if _, err := io.Copy(io.Discard, r.Body); err != nil {
								t.Error(err)
								return
							}
							w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
							if _, err := w.Write(wire.Bytes()); err != nil && tc.name != "progress byte budget" {
								t.Error(err)
							}
						default:
							t.Errorf("unexpected request: %s %s", r.Method, r.URL)
							http.Error(w, "unexpected request", http.StatusNotFound)
						}
					}))
					defer server.Close()
					ep, err := transport.NewEndpoint(server.URL + "/origin.git")
					if err != nil {
						t.Fatal(err)
					}
					raw := &rawReport{ref: ref}
					c, err := rawHTTPTransport(ep, raw)
					if err != nil {
						t.Fatal(err)
					}
					sess, err := c.NewReceivePackSession(ep, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = sess.Close() }()
					ar, err := sess.AdvertisedReferencesContext(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					req := packp.NewReferenceUpdateRequestFromCapabilities(ar.Capabilities)
					if err := req.Capabilities.Set(selected); err != nil {
						t.Fatal(err)
					}
					req.Commands = []*packp.Command{{
						Name: ref, Old: plumbing.ZeroHash,
						New: plumbing.NewHash(strings.Repeat("1", 40)),
					}}
					result, err := receiveObservedPack(context.Background(), sess, req, raw)
					if !result.invoked || result.success != tc.success || result.rejected != tc.rejected || (err != nil) != tc.wantError {
						t.Fatalf("result=%+v error=%v", result, err)
					}
					if tc.name == "progress byte budget" && (err == nil || !strings.Contains(err.Error(), "response byte limit")) {
						t.Fatalf("missing budget cause: %v", err)
					}
					if !raw.sideband || raw.complete() != tc.complete || (tc.rejected && result.reason != "ok") {
						t.Fatalf("raw=%+v result=%+v", raw, result)
					}
				})
			}
		})
	}
}
