package providers

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	probeBase = "https://example.com/anthropic"
	// A dummy key: printable ASCII, nothing provider-token-shaped.
	probeKey = "dummy-gateway-key-0001"

	okMessage      = `{"id":"m1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"stop_reason":"max_tokens"}`
	nullStop       = `{"type":"message","role":"assistant","content":[],"stop_reason":null}`
	toolMessage    = `{"type":"message","role":"assistant","content":[{"type":"tool_use","id":"t1","name":"uzi_probe","input":{"ok":true}}],"stop_reason":"tool_use"}`
	htmlBody       = `<html><body>Welcome to the corporate portal SENTINEL_BODY_BYTES</body></html>`
	unrelatedJSON  = `{"hello":"world","SENTINEL_BODY_BYTES":1}`
	truncatedBody  = `{"type":"message","role":"assistant","content":[{"type":"text","te`
	noStopReason   = `{"type":"message","role":"assistant","content":[]}`
	userRole       = `{"type":"message","role":"user","content":[],"stop_reason":null}`
	contentNotList = `{"type":"message","role":"assistant","content":"hi","stop_reason":null}`
)

type reply struct {
	status   int
	ctype    string
	body     string
	location string
	hang     bool
}

func (r reply) write(w http.ResponseWriter, req *http.Request) {
	if r.hang {
		<-req.Context().Done()
		return
	}
	if r.location != "" {
		w.Header().Set("Location", r.location)
	}
	ct := r.ctype
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(r.status)
	_, _ = w.Write([]byte(r.body))
}

type captured struct {
	path   string
	header http.Header
	body   []byte
}

type gateway struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []captured
}

func (g *gateway) requests() []captured {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]captured(nil), g.reqs...)
}

// newGateway starts a TLS gateway answering the auth step with auth and the forced-tool step with tool.
func newGateway(t *testing.T, auth, tool reply) *gateway {
	t.Helper()
	g := &gateway{}
	g.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(http.MaxBytesReader(w, r.Body, 1<<20))
		g.mu.Lock()
		g.reqs = append(g.reqs, captured{path: r.URL.Path, header: r.Header.Clone(), body: buf.Bytes()})
		g.mu.Unlock()
		if bytes.Contains(buf.Bytes(), []byte(`"tools"`)) {
			tool.write(w, r)
			return
		}
		auth.write(w, r)
	}))
	t.Cleanup(g.srv.Close)
	return g
}

type dialRecorder struct {
	mu    sync.Mutex
	dials []netip.AddrPort
	to    string // the real address every recorded dial is connected to

	// When altPort is non-zero, a dial to that port is connected to altTo instead of to: a test's
	// stand-in for a second origin the gateway could redirect to.
	altPort uint16
	altTo   string
}

func (d *dialRecorder) dial(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
	d.mu.Lock()
	d.dials = append(d.dials, ap)
	d.mu.Unlock()
	to := d.to
	if d.altPort != 0 && ap.Port() == d.altPort {
		to = d.altTo
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", to)
}

func (d *dialRecorder) recorded() []netip.AddrPort {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]netip.AddrPort(nil), d.dials...)
}

// testProber builds a Prober on the production allowlist path with an injected resolver answering
// every name with addrs, and a dial recorder that connects to gw.
func testProber(t *testing.T, gw *gateway, loopback bool, addrs ...string) (*Prober, *dialRecorder) {
	t.Helper()
	pool := x509.NewCertPool()
	to := "127.0.0.1:1"
	if gw != nil {
		pool.AddCert(gw.srv.Certificate())
		to = gw.srv.Listener.Addr().String()
	}
	list := []string{probeBase}
	p := NewProber(ProbeConfig{
		Allowed:       func(u string) (string, bool) { return MatchAllowed(list, u, loopback) },
		AllowLoopback: loopback,
		RootCAs:       pool,
	})
	var parsed []netip.Addr
	for _, a := range addrs {
		parsed = append(parsed, netip.MustParseAddr(a))
	}
	p.resolve = func(context.Context, string) ([]netip.Addr, error) { return parsed, nil }
	rec := &dialRecorder{to: to}
	p.dial = rec.dial
	p.stepTimeout = 5 * time.Second
	return p, rec
}

func target() ProbeTarget {
	return ProbeTarget{BaseURL: probeBase, AuthStyle: AuthBearer, Model: "corp-haiku", Key: []byte(probeKey)}
}

func TestProbeClassification(t *testing.T) {
	redirectTarget := func(hits *atomic.Int32) string {
		s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
		t.Cleanup(s.Close)
		return s.URL + "/steal"
	}
	big := `{"type":"message","role":"assistant","content":[{"type":"text","text":"` + strings.Repeat("a", MaxProbeResponseBytes+10) + `"}],"stop_reason":null}`
	atLimit := okMessage[:len(okMessage)-1] + `,"pad":"` + strings.Repeat("a", MaxProbeResponseBytes-len(okMessage)-12) + `"}`
	if len(atLimit) > MaxProbeResponseBytes {
		t.Fatalf("fixture is %d bytes, over the cap", len(atLimit))
	}

	// Valid Messages JSON followed by whitespace past the cap: still valid JSON, so only the size
	// check can reject it.
	padded := okMessage + strings.Repeat(" ", MaxProbeResponseBytes)
	toolPadded := toolMessage + strings.Repeat(" ", MaxProbeResponseBytes)

	cases := []struct {
		name       string
		auth, tool reply
		timeout    time.Duration
		wantAuth   string
		wantTool   string
		wantTools  int // requests expected to carry a tool definition
	}{
		{"ok and tool passed", reply{status: 200, body: okMessage}, reply{status: 200, body: toolMessage}, 0, AuthOK, ToolPassed, 1},
		{"null stop_reason is fine", reply{status: 200, body: nullStop}, reply{status: 200, body: toolMessage}, 0, AuthOK, ToolPassed, 1},
		{"response exactly at the cap is read", reply{status: 200, body: atLimit}, reply{status: 200, body: toolMessage}, 0, AuthOK, ToolPassed, 1},
		{"tool not supported: text only", reply{status: 200, body: okMessage}, reply{status: 200, body: okMessage}, 0, AuthOK, ToolNotSupported, 1},
		{"tool not supported: other tool name", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, "uzi_probe", "other", 1)}, 0, AuthOK, ToolNotSupported, 1},
		{"tool inconclusive: input not an object", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `{"ok":true}`, `"x"`, 1)}, 0, AuthOK, ToolInconclusive, 1},
		{"tool inconclusive: content is not an array of objects", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `[{"type":"tool_use","id":"t1","name":"uzi_probe","input":{"ok":true}}]`, `[1]`, 1)}, 0, AuthOK, ToolInconclusive, 1},
		{"tool inconclusive: block type is not a string", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `"type":"tool_use"`, `"type":7`, 1)}, 0, AuthOK, ToolInconclusive, 1},
		{"tool inconclusive: block without a type", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `[{"type":"tool_use","id":"t1","name":"uzi_probe","input":{"ok":true}}]`, `[{}]`, 1)}, 0, AuthOK, ToolInconclusive, 1},
		{"tool inconclusive: text block with non-string text", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `"content":[`, `"content":[{"type":"text","text":7},`, 1)}, 0, AuthOK, ToolInconclusive, 1},
		{"tool inconclusive: tool_use without an id", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `"id":"t1",`, ``, 1)}, 0, AuthOK, ToolInconclusive, 1},
		{"tool inconclusive: tool_use without a name", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `"name":"uzi_probe",`, ``, 1)}, 0, AuthOK, ToolInconclusive, 1},
		{"tool inconclusive: tool_use with a null name", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `"name":"uzi_probe"`, `"name":null`, 1)}, 0, AuthOK, ToolInconclusive, 1},
		{"tool passed: unknown block type with odd fields beside a valid tool_use", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `"content":[`, `"content":[{"type":"weird","id":5},`, 1)}, 0, AuthOK, ToolPassed, 1},
		{"tool inconclusive: valid tool_use beside a malformed sibling", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `"content":[`, `"content":[{"type":"text"},`, 1)}, 0, AuthOK, ToolInconclusive, 1},
		{"tool passed: valid text sibling", reply{status: 200, body: okMessage},
			reply{status: 200, body: strings.Replace(toolMessage, `"content":[`, `"content":[{"type":"text","text":"calling"},`, 1)}, 0, AuthOK, ToolPassed, 1},
		{"tool not supported: empty content", reply{status: 200, body: okMessage}, reply{status: 200, body: nullStop}, 0, AuthOK, ToolNotSupported, 1},
		{"tool step html", reply{status: 200, body: okMessage}, reply{status: 200, ctype: "text/html", body: htmlBody}, 0, AuthOK, ToolInconclusive, 1},
		{"tool step malformed json", reply{status: 200, body: okMessage}, reply{status: 200, body: truncatedBody}, 0, AuthOK, ToolInconclusive, 1},
		{"tool step unrelated json", reply{status: 200, body: okMessage}, reply{status: 200, body: unrelatedJSON}, 0, AuthOK, ToolInconclusive, 1},
		{"tool step 500", reply{status: 200, body: okMessage}, reply{status: 500, body: "boom"}, 0, AuthOK, ToolInconclusive, 1},
		{"tool step 401", reply{status: 200, body: okMessage}, reply{status: 401, body: "no"}, 0, AuthOK, ToolInconclusive, 1},
		{"tool step oversized", reply{status: 200, body: okMessage}, reply{status: 200, body: big}, 0, AuthOK, ToolInconclusive, 1},
		{"tool step oversized but valid json", reply{status: 200, body: okMessage}, reply{status: 200, body: toolPadded}, 0, AuthOK, ToolInconclusive, 1},
		{"tool step timeout", reply{status: 200, body: okMessage}, reply{hang: true}, 150 * time.Millisecond, AuthOK, ToolInconclusive, 1},
		{"401 rejected", reply{status: 401, body: "unauthorized"}, reply{status: 200, body: toolMessage}, 0, AuthRejected, ToolNotRun, 0},
		{"403 rejected", reply{status: 403, body: "forbidden"}, reply{status: 200, body: toolMessage}, 0, AuthRejected, ToolNotRun, 0},
		{"200 html is inconclusive", reply{status: 200, ctype: "text/html", body: htmlBody}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 malformed json", reply{status: 200, body: "{not json"}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 truncated", reply{status: 200, body: truncatedBody}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 unrelated json", reply{status: 200, body: unrelatedJSON}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 empty body", reply{status: 200, body: ""}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 json null", reply{status: 200, body: "null"}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 without stop_reason", reply{status: 200, body: noStopReason}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 role user", reply{status: 200, body: userRole}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 content not a list", reply{status: 200, body: contentNotList}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 oversized but valid json", reply{status: 200, body: padded}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"200 oversized", reply{status: 200, body: big}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"404", reply{status: 404, body: "nope"}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"429", reply{status: 429, body: "slow"}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"500", reply{status: 500, body: "boom"}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"204", reply{status: 204}, reply{status: 200, body: toolMessage}, 0, AuthInconclusive, ToolNotRun, 0},
		{"timeout", reply{hang: true}, reply{status: 200, body: toolMessage}, 150 * time.Millisecond, AuthInconclusive, ToolNotRun, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gw := newGateway(t, c.auth, c.tool)
			p, _ := testProber(t, gw, false, "10.0.0.5")
			if c.timeout > 0 {
				p.stepTimeout = c.timeout
			}
			res, err := p.Probe(context.Background(), target())
			if err != nil {
				t.Fatal(err)
			}
			if res.Auth != c.wantAuth || res.Tool != c.wantTool {
				t.Fatalf("result = %+v, want auth %q tool %q", res, c.wantAuth, c.wantTool)
			}
			withTools := 0
			for _, r := range gw.requests() {
				if bytes.Contains(r.body, []byte(`"tools"`)) {
					withTools++
				}
			}
			if withTools != c.wantTools {
				t.Errorf("%d tool requests sent, want %d", withTools, c.wantTools)
			}
		})
	}

	t.Run("302 is inconclusive and never followed", func(t *testing.T) {
		var hits atomic.Int32
		loc := redirectTarget(&hits)
		gw := newGateway(t, reply{status: 302, location: loc}, reply{status: 200, body: toolMessage})
		p, rec := testProber(t, gw, false, "10.0.0.5")
		res, err := p.Probe(context.Background(), target())
		if err != nil || res.Auth != AuthInconclusive || res.Tool != ToolNotRun {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if n := len(rec.recorded()); n != 1 {
			t.Errorf("%d dials, want 1 (a followed redirect opens another connection)", n)
		}
		if hits.Load() != 0 {
			t.Errorf("redirect target was contacted %d time(s)", hits.Load())
		}
		if n := len(gw.requests()); n != 1 {
			t.Errorf("gateway saw %d requests, want 1", n)
		}
	})
	for _, style := range []string{AuthBearer, AuthXAPIKey} {
		// The redirect points at another path on the SAME host the dialer is pinned to, so the pin
		// does not stop it: only the client's refusal to follow does.
		const other = "/other-tenant/v1/messages"
		t.Run("same-host redirect on the auth step, "+style, func(t *testing.T) {
			gw := newGateway(t, reply{status: 307, location: other}, reply{status: 200, body: toolMessage})
			p, _ := testProber(t, gw, false, "10.0.0.5")
			tg := target()
			tg.AuthStyle = style
			res, err := p.Probe(context.Background(), tg)
			if err != nil || res.Auth != AuthInconclusive || res.Tool != ToolNotRun {
				t.Fatalf("result = %+v, %v", res, err)
			}
			reqs := gw.requests()
			if len(reqs) != 1 {
				t.Fatalf("gateway saw %d requests, want 1", len(reqs))
			}
			for _, r := range reqs {
				if r.path == other {
					t.Errorf("the redirect was followed to %s", other)
				}
			}
		})
		t.Run("same-host redirect on the tool step, "+style, func(t *testing.T) {
			gw := newGateway(t, reply{status: 200, body: okMessage}, reply{status: 307, location: other})
			p, _ := testProber(t, gw, false, "10.0.0.5")
			tg := target()
			tg.AuthStyle = style
			res, err := p.Probe(context.Background(), tg)
			if err != nil || res.Auth != AuthOK || res.Tool != ToolInconclusive {
				t.Fatalf("result = %+v, %v", res, err)
			}
			reqs := gw.requests()
			if len(reqs) != 2 {
				t.Fatalf("gateway saw %d requests, want 2", len(reqs))
			}
			for _, r := range reqs {
				if r.path == other {
					t.Errorf("the redirect was followed to %s", other)
				}
			}
		})
	}
	t.Run("redirect on the tool step", func(t *testing.T) {
		var hits atomic.Int32
		gw := newGateway(t, reply{status: 200, body: okMessage}, reply{status: 307, location: redirectTarget(&hits)})
		p, rec := testProber(t, gw, false, "10.0.0.5")
		res, _ := p.Probe(context.Background(), target())
		if res.Auth != AuthOK || res.Tool != ToolInconclusive || hits.Load() != 0 {
			t.Fatalf("result = %+v, redirect hits %d", res, hits.Load())
		}
		if n := len(rec.recorded()); n != 2 {
			t.Errorf("%d dials, want 2 (one per step; a followed redirect adds one)", n)
		}
	})
}

// TestProbeNeverFollowsACrossOriginRedirect proves the probe re-sends neither the key nor the body
// to the Location of a 3xx. The redirect target is a real second TLS origin the dialer can reach
// (routed by port), the resolver admits it, and its certificate is trusted, so the only thing that
// keeps the attacker unreached is the client refusing to follow.
func TestProbeNeverFollowsACrossOriginRedirect(t *testing.T) {
	const attackerPort = 8443
	const location = "https://attacker.example:8443/steal"
	for _, step := range []string{"auth", "tool"} {
		for _, style := range []string{AuthBearer, AuthXAPIKey} {
			for _, status := range []int{302, 307, 308} {
				t.Run(fmt.Sprintf("%s step, %s, %d", step, style, status), func(t *testing.T) {
					var mu sync.Mutex
					var got []captured
					attacker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body := mustRead(r)
						mu.Lock()
						got = append(got, captured{path: r.URL.Path, header: r.Header.Clone(), body: body})
						mu.Unlock()
						reply{status: 200, body: okMessage}.write(w, r)
					}))
					t.Cleanup(attacker.Close)

					redirect := reply{status: status, location: location}
					auth, tool := redirect, reply{status: 200, body: toolMessage}
					wantAuth, wantTool, wantDials := AuthInconclusive, ToolNotRun, 1
					if step == "tool" {
						auth, tool = reply{status: 200, body: okMessage}, redirect
						wantAuth, wantTool, wantDials = AuthOK, ToolInconclusive, 2
					}
					gw := newGateway(t, auth, tool)
					p, rec := testProber(t, gw, false, "10.0.0.5")
					rec.altPort, rec.altTo = attackerPort, attacker.Listener.Addr().String()
					pool := x509.NewCertPool()
					pool.AddCert(gw.srv.Certificate())
					pool.AddCert(attacker.Certificate())
					p.cfg.RootCAs = pool
					tg := target()
					tg.AuthStyle = style

					res, err := p.Probe(context.Background(), tg)
					if err != nil || res.Auth != wantAuth || res.Tool != wantTool {
						t.Fatalf("result = %+v, %v; want auth %q tool %q", res, err, wantAuth, wantTool)
					}
					mu.Lock()
					defer mu.Unlock()
					for _, r := range got {
						t.Errorf("attacker received %s with Authorization=%q x-api-key=%q body=%d bytes",
							r.path, r.header.Get("Authorization"), r.header.Get("x-api-key"), len(r.body))
					}
					for _, d := range rec.recorded() {
						if d.Port() == attackerPort {
							t.Errorf("dialed the redirect target port: %v", d)
						}
					}
					if n := len(rec.recorded()); n != wantDials {
						t.Errorf("%d dials, want %d", n, wantDials)
					}
				})
			}
		}
	}
}

func TestProbeRequestShape(t *testing.T) {
	for _, style := range []string{AuthBearer, AuthXAPIKey} {
		t.Run(style, func(t *testing.T) {
			gw := newGateway(t, reply{status: 200, body: okMessage}, reply{status: 200, body: toolMessage})
			p, _ := testProber(t, gw, false, "10.0.0.5")
			tg := target()
			tg.AuthStyle = style
			if _, err := p.Probe(context.Background(), tg); err != nil {
				t.Fatal(err)
			}
			reqs := gw.requests()
			if len(reqs) != 2 {
				t.Fatalf("%d requests, want 2", len(reqs))
			}
			for i, r := range reqs {
				if r.path != "/anthropic/v1/messages" {
					t.Errorf("request %d path = %q", i, r.path)
				}
				bearer, xkey := r.header.Get("Authorization"), r.header.Get("x-api-key")
				switch style {
				case AuthBearer:
					if bearer != "Bearer "+probeKey || xkey != "" {
						t.Errorf("request %d auth headers = %q / %q, want bearer only", i, bearer, xkey)
					}
				case AuthXAPIKey:
					if xkey != probeKey || bearer != "" {
						t.Errorf("request %d auth headers = %q / %q, want x-api-key only", i, bearer, xkey)
					}
				}
				if r.header.Get("anthropic-version") != "2023-06-01" || r.header.Get("content-type") != "application/json" {
					t.Errorf("request %d headers = %v", i, r.header)
				}
				if len(r.body) == 0 || len(r.body) > MaxProbeRequestBytes {
					t.Errorf("request %d body is %d bytes", i, len(r.body))
				}
			}
			var a, b map[string]any
			if json.Unmarshal(reqs[0].body, &a) != nil || json.Unmarshal(reqs[1].body, &b) != nil {
				t.Fatal("request bodies are not JSON objects")
			}
			if a["model"] != "corp-haiku" || a["max_tokens"] != float64(1) || a["tools"] != nil || a["tool_choice"] != nil {
				t.Errorf("auth step body = %s", reqs[0].body)
			}
			msgs, _ := a["messages"].([]any)
			if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
				t.Errorf("auth step messages = %v", a["messages"])
			}
			tc, _ := b["tool_choice"].(map[string]any)
			tools, _ := b["tools"].([]any)
			if b["model"] != "corp-haiku" || tc["type"] != "tool" || tc["name"] != "uzi_probe" || len(tools) != 1 || b["max_tokens"] != float64(64) {
				t.Errorf("tool step body = %s", reqs[1].body)
			}
			tool := tools[0].(map[string]any)
			schema, _ := tool["input_schema"].(map[string]any)
			if tool["name"] != "uzi_probe" || tool["description"] == "" || schema["type"] != "object" {
				t.Errorf("tool definition = %v", tool)
			}
		})
	}
}

// captureLogs routes the default slog logger into a buffer for the test.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestProbeNeverLogsOrReturnsBodies(t *testing.T) {
	buf := &logBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, body := range []string{htmlBody, unrelatedJSON, truncatedBody} {
		gw := newGateway(t, reply{status: 200, body: body}, reply{status: 200, body: toolMessage})
		p, _ := testProber(t, gw, false, "10.0.0.5")
		res, err := p.Probe(context.Background(), target())
		if err != nil {
			t.Fatal(err)
		}
		if out := fmt.Sprintf("%+v", res); strings.Contains(out, "SENTINEL_BODY_BYTES") || strings.Contains(out, "portal") {
			t.Errorf("result carries body bytes: %s", out)
		}
	}
	if s := buf.String(); strings.Contains(s, "SENTINEL_BODY_BYTES") || strings.Contains(s, "portal") || strings.Contains(s, probeKey) {
		t.Errorf("logs carry body bytes or the key: %s", s)
	}
}

func TestProbeRefusesAnUnallowedBaseURLWithoutNetwork(t *testing.T) {
	p, rec := testProber(t, nil, false, "10.0.0.5")
	for _, u := range []string{"https://example.com/other", "https://example.com", "http://example.com/anthropic", "https://evil.test/anthropic", ""} {
		tg := target()
		tg.BaseURL = u
		if _, err := p.Probe(context.Background(), tg); !errors.Is(err, ErrBaseURLNotAllowed) {
			t.Errorf("Probe(%q) error = %v, want ErrBaseURLNotAllowed", u, err)
		}
	}
	if got := rec.recorded(); len(got) != 0 {
		t.Errorf("dialed %v for refused base URLs", got)
	}
	none := NewProber(ProbeConfig{})
	if _, err := none.Probe(context.Background(), target()); !errors.Is(err, ErrBaseURLNotAllowed) {
		t.Errorf("a prober without an allowlist = %v, want ErrBaseURLNotAllowed", err)
	}
}

func TestProbeAddressPolicy(t *testing.T) {
	refused := []string{
		"169.254.169.254", "169.254.0.1", "fd00:ec2::254", "0.0.0.0", "0.1.2.3", "::", "224.0.0.1", "239.255.255.250",
		"ff02::1", "255.255.255.255", "fe80::1", "::ffff:169.254.169.254", "::ffff:0.0.0.0", "::ffff:224.0.0.1",
		"127.0.0.1", "127.1.2.3", "::1", "::ffff:127.0.0.1",
	}
	for _, ip := range refused {
		t.Run("refused "+ip, func(t *testing.T) {
			gw := newGateway(t, reply{status: 200, body: okMessage}, reply{status: 200, body: toolMessage})
			p, rec := testProber(t, gw, false, ip)
			res, err := p.Probe(context.Background(), target())
			if err != nil || res.Auth != AuthInconclusive || res.Tool != ToolNotRun {
				t.Fatalf("result = %+v, %v", res, err)
			}
			if got := rec.recorded(); len(got) != 0 {
				t.Errorf("dialed %v", got)
			}
			if n := len(gw.requests()); n != 0 {
				t.Errorf("gateway saw %d requests", n)
			}
		})
	}
	// The dev flag admits loopback only; the always-refused set stays refused with it.
	for _, ip := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1"} {
		t.Run("loopback with flag "+ip, func(t *testing.T) {
			gw := newGateway(t, reply{status: 200, body: okMessage}, reply{status: 200, body: toolMessage})
			p, rec := testProber(t, gw, true, ip)
			res, _ := p.Probe(context.Background(), target())
			if res.Auth != AuthOK || res.Tool != ToolPassed {
				t.Fatalf("result = %+v", res)
			}
			want := netip.MustParseAddr(ip).Unmap()
			for _, got := range rec.recorded() {
				if got.Addr() != want {
					t.Errorf("dialed %v, want the checked address %v", got, want)
				}
			}
		})
	}
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "0.0.0.0", "224.0.0.1", "255.255.255.255"} {
		t.Run("still refused with flag "+ip, func(t *testing.T) {
			p, rec := testProber(t, nil, true, ip)
			res, _ := p.Probe(context.Background(), target())
			if res.Auth != AuthInconclusive || len(rec.recorded()) != 0 {
				t.Errorf("result = %+v, dials %v", res, rec.recorded())
			}
		})
	}
	// Private gateway ranges are admitted, and the dial goes to the pinned, checked address.
	for _, ip := range []string{"10.0.0.5", "172.16.4.4", "192.168.1.9", "fd12::5", "fc00::1", "100.64.0.7", "8.8.8.8", "2001:4860::1"} {
		t.Run("admitted "+ip, func(t *testing.T) {
			gw := newGateway(t, reply{status: 200, body: okMessage}, reply{status: 200, body: toolMessage})
			p, rec := testProber(t, gw, false, ip)
			res, err := p.Probe(context.Background(), target())
			if err != nil || res.Auth != AuthOK || res.Tool != ToolPassed {
				t.Fatalf("result = %+v, %v", res, err)
			}
			dials := rec.recorded()
			if len(dials) == 0 {
				t.Fatal("no dial recorded")
			}
			for _, d := range dials {
				if d.Addr() != netip.MustParseAddr(ip) || d.Port() != 443 {
					t.Errorf("dialed %v, want %s:443", d, ip)
				}
			}
		})
	}
	t.Run("mixed answers refuse the whole host", func(t *testing.T) {
		for _, order := range [][]string{{"10.0.0.5", "169.254.169.254"}, {"169.254.169.254", "10.0.0.5"}} {
			gw := newGateway(t, reply{status: 200, body: okMessage}, reply{status: 200, body: toolMessage})
			p, rec := testProber(t, gw, false, order...)
			res, _ := p.Probe(context.Background(), target())
			if res.Auth != AuthInconclusive || res.Tool != ToolNotRun {
				t.Errorf("%v: result = %+v", order, res)
			}
			if got := rec.recorded(); len(got) != 0 {
				t.Errorf("%v: dialed %v despite a refused answer", order, got)
			}
			if n := len(gw.requests()); n != 0 {
				t.Errorf("%v: gateway saw %d requests", order, n)
			}
		}
	})
	t.Run("an IP literal base URL is checked too", func(t *testing.T) {
		p, rec := testProber(t, nil, false)
		p.cfg.Allowed = func(u string) (string, bool) {
			return MatchAllowed([]string{"https://169.254.169.254/anthropic"}, u, false)
		}
		// NormalizeBaseURL already refuses a link-local literal; the dial policy is the second layer.
		tg := target()
		tg.BaseURL = "https://169.254.169.254/anthropic"
		if _, err := p.Probe(context.Background(), tg); !errors.Is(err, ErrBaseURLNotAllowed) {
			t.Errorf("error = %v", err)
		}
		if len(rec.recorded()) != 0 {
			t.Error("dialed")
		}
	})
	t.Run("each connection re-resolves and is checked on its own", func(t *testing.T) {
		gw := newGateway(t, reply{status: 200, body: okMessage}, reply{status: 200, body: toolMessage})
		p, rec := testProber(t, gw, false)
		var calls atomic.Int32
		answers := [][]netip.Addr{{netip.MustParseAddr("10.0.0.5")}, {netip.MustParseAddr("169.254.169.254")}}
		p.resolve = func(context.Context, string) ([]netip.Addr, error) {
			return answers[min(int(calls.Add(1))-1, len(answers)-1)], nil
		}
		// First connection resolves to the private address and is dialed there; the dial hook itself
		// never resolves, so a rebinding second answer can only affect a later connection, which is
		// checked on its own.
		res, _ := p.Probe(context.Background(), target())
		if res.Auth != AuthOK {
			t.Fatalf("auth = %q", res.Auth)
		}
		if res.Tool != ToolInconclusive {
			t.Errorf("tool = %q; the second connection resolved to a refused address", res.Tool)
		}
		for _, d := range rec.recorded() {
			if d.Addr().String() != "10.0.0.5" {
				t.Errorf("dialed %v", d)
			}
		}
	})
}

func TestProbeTransportHasNoProxy(t *testing.T) {
	p := NewProber(ProbeConfig{})
	if p.transport("example.com").Proxy != nil {
		t.Fatal("the probe transport must not consult proxy environment variables")
	}
}

// recordingProxy counts every request and CONNECT that reaches it.
func recordingProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	t.Cleanup(s.Close)
	return s, &hits
}

func TestProbeIgnoresProxyEnvironment(t *testing.T) {
	proxy, hits := recordingProxy(t)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	t.Run("refusals hold and the proxy sees nothing", func(t *testing.T) {
		for _, ip := range []string{"169.254.169.254", "127.0.0.1", "0.0.0.0"} {
			p, rec := testProber(t, nil, false, ip)
			res, _ := p.Probe(context.Background(), target())
			if res.Auth != AuthInconclusive || len(rec.recorded()) != 0 {
				t.Errorf("%s: result %+v, dials %v", ip, res, rec.recorded())
			}
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("proxy saw %d request(s)", n)
		}
	})
	t.Run("an admitted destination is dialed directly", func(t *testing.T) {
		gw := newGateway(t, reply{status: 200, body: okMessage}, reply{status: 200, body: toolMessage})
		p, rec := testProber(t, gw, false, "10.0.0.5")
		res, err := p.Probe(context.Background(), target())
		if err != nil || res.Auth != AuthOK || res.Tool != ToolPassed {
			t.Fatalf("result = %+v, %v", res, err)
		}
		if len(rec.recorded()) == 0 || rec.recorded()[0].Addr() != netip.MustParseAddr("10.0.0.5") {
			t.Errorf("dials = %v, want the checked address", rec.recorded())
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("proxy saw %d request(s)", n)
		}
	})
}

// testCA issues a CA and a leaf certificate for dnsName signed by it.
func testCA(t *testing.T, dnsName string) (caPEM []byte, leaf tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "uzi test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

func TestProbeTrustsTheExtraCAOnlyWhenConfigured(t *testing.T) {
	caPEM, leaf := testCA(t, "example.com")
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bytes.Contains(mustRead(r), []byte(`"tools"`)) {
			reply{status: 200, body: toolMessage}.write(w, r)
			return
		}
		reply{status: 200, body: okMessage}.write(w, r)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	build := func(roots *x509.CertPool) *Prober {
		p := NewProber(ProbeConfig{
			Allowed: func(u string) (string, bool) { return MatchAllowed([]string{probeBase}, u, false) },
			RootCAs: roots,
		})
		p.resolve = func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.0.0.5")}, nil
		}
		p.dial = (&dialRecorder{to: srv.Listener.Addr().String()}).dial
		p.stepTimeout = 5 * time.Second
		return p
	}

	if err := CheckCABundle(caPEM); err != nil {
		t.Fatal(err)
	}
	pool, err := RootCAs(caPEM)
	if err != nil || pool == nil {
		t.Fatalf("RootCAs = %v, %v", pool, err)
	}
	res, _ := build(pool).Probe(context.Background(), target())
	if res.Auth != AuthOK || res.Tool != ToolPassed {
		t.Errorf("with the extra CA: %+v, want ok/passed", res)
	}
	res, _ = build(nil).Probe(context.Background(), target())
	if res.Auth != AuthInconclusive || res.Tool != ToolNotRun {
		t.Errorf("without the extra CA: %+v, want inconclusive", res)
	}
	if p, err := RootCAs(nil); p != nil || err != nil {
		t.Errorf("RootCAs(nil) = %v, %v; want system roots (nil, nil)", p, err)
	}
	for _, bad := range [][]byte{[]byte("not pem"), []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), nil} {
		if CheckCABundle(bad) == nil {
			t.Errorf("CheckCABundle(%q) accepted a bundle with no certificate", bad)
		}
	}
	if _, err := RootCAs([]byte("not pem")); err == nil {
		t.Error("RootCAs accepted garbage")
	}
}

func mustRead(r *http.Request) []byte {
	var b bytes.Buffer
	_, _ = b.ReadFrom(http.MaxBytesReader(nil, r.Body, 1<<20))
	return b.Bytes()
}
