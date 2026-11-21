package providers

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"syscall"
	"time"
)

// The provider Test probe (PRD #4320 section 8). Two requests against <base_url>/v1/messages with
// the provider's key: (a) a one-token request that says whether the key authenticates and the
// endpoint speaks the Messages protocol, (b) only if (a) succeeded, a forced single-tool request
// that says whether the gateway returns a tool_use block. Neither certifies full compatibility.
//
// The destination is user-chosen (within the operator's exact-URL allowlist) and the api dials it,
// so the transport is its own SSRF boundary: no proxy, no redirect, every resolved address checked
// against addrRefused and the checked addresses pinned for the dial.

// Probe outcomes, stored and returned verbatim.
const (
	AuthOK           = "ok"
	AuthRejected     = "rejected"
	AuthInconclusive = "inconclusive"

	ToolPassed       = "passed"
	ToolNotSupported = "not_supported"
	ToolInconclusive = "inconclusive"
	ToolNotRun       = "not_run"
)

const (
	// DefaultProbeStepTimeout bounds each of the two requests, dial to last body byte.
	DefaultProbeStepTimeout = 20 * time.Second
	// MaxProbeRequestBytes bounds a probe request body.
	MaxProbeRequestBytes = 4 << 10
	// MaxProbeResponseBytes bounds how much of a response body is read; more is "oversized".
	MaxProbeResponseBytes = 64 << 10

	probeToolName = "uzi_probe"
)

// ErrBaseURLNotAllowed means the provider's base URL is not (or no longer) on the operator
// allowlist; the probe made no network call.
var ErrBaseURLNotAllowed = errors.New("provider base URL is not allowed")

var errAddressRefused = errors.New("address refused by the provider probe policy")

var (
	broadcastV4 = netip.AddrFrom4([4]byte{255, 255, 255, 255})
	// fd00:ec2::254 is the AWS instance-metadata IPv6 address; it sits inside the admitted ULA range.
	awsMetadataV6 = netip.MustParseAddr("fd00:ec2::254")
)

// addrRefused is the probe's destination address policy. Always refused: unspecified (0.0.0.0/8,
// ::), link-local (169.254.0.0/16, fe80::/10, also in IPv4-mapped form), fd00:ec2::254, multicast
// and broadcast. Loopback is refused unless allowLoopback (the dev flag). Everything else is
// admitted, including RFC 1918, ULA (fc00::/7) and CGNAT, because self-hosted gateways live there.
func addrRefused(a netip.Addr, allowLoopback bool) bool {
	a = a.WithZone("").Unmap()
	switch {
	case !a.IsValid(), a.IsUnspecified(), a.Is4() && a.As4()[0] == 0:
		return true
	case a.IsLinkLocalUnicast(), a.IsMulticast(), a == broadcastV4, a == awsMetadataV6:
		return true
	case a.IsLoopback():
		return !allowLoopback
	}
	return false
}

// CheckCABundle reports whether pem holds at least one parseable certificate.
func CheckCABundle(pem []byte) error {
	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return errors.New("contains no PEM certificate")
	}
	return nil
}

// RootCAs returns the system roots plus the PEM bundle (the operator's extra CA); a nil or empty
// bundle returns nil, which the TLS stack reads as "system roots".
func RootCAs(extraPEM []byte) (*x509.CertPool, error) {
	if len(extraPEM) == 0 {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(extraPEM) {
		return nil, errors.New("contains no PEM certificate")
	}
	return pool, nil
}

// ProbeConfig configures a Prober.
type ProbeConfig struct {
	// Allowed resolves a base URL to the operator allowlist entry it equals exactly; it is called
	// on every probe, before any network use, so a removal takes effect at once.
	Allowed func(baseURL string) (string, bool)
	// AllowLoopback is the dev flag (UZI_PROVIDER_ALLOW_LOOPBACK): loopback destinations are
	// admitted. Never set in production.
	AllowLoopback bool
	// RootCAs is the probe client's trust pool (system roots plus the operator's extra CA); nil
	// means system roots. Used by this client only.
	RootCAs *x509.CertPool
}

// Prober runs the Test probe. It is safe for concurrent use.
type Prober struct {
	cfg         ProbeConfig
	stepTimeout time.Duration
	// resolve and dial are test seams: nil means the system resolver and a net.Dialer whose Control
	// hook re-checks the address at the socket.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	dial    func(ctx context.Context, ap netip.AddrPort) (net.Conn, error)
}

// NewProber returns a Prober for cfg.
func NewProber(cfg ProbeConfig) *Prober {
	return &Prober{cfg: cfg, stepTimeout: DefaultProbeStepTimeout}
}

// ProbeTarget is what one probe needs. Key is the provider key; the caller drops it afterwards.
type ProbeTarget struct {
	Protocol  string
	BaseURL   string
	AuthStyle string
	Model     string // the provider's mapped probe model (Claude haiku or Codex default)
	Key       []byte
}

// ProbeResult is the pair of outcomes of one probe.
type ProbeResult struct {
	Auth string
	Tool string
}

// Probe runs the two steps. It returns ErrBaseURLNotAllowed (and does no network I/O) when the base
// URL is not on the allowlist; every other failure is a classification, never an error, and no
// response bytes leave this function.
func (p *Prober) Probe(ctx context.Context, t ProbeTarget) (ProbeResult, error) {
	if p.cfg.Allowed == nil {
		return ProbeResult{}, ErrBaseURLNotAllowed
	}
	entry, ok := p.cfg.Allowed(t.BaseURL)
	if !ok {
		return ProbeResult{}, ErrBaseURLNotAllowed
	}
	res := ProbeResult{Auth: AuthInconclusive, Tool: ToolNotRun}
	protocol := t.Protocol
	if protocol == "" {
		protocol = ProtocolAnth
	}
	if protocol != ProtocolAnth && protocol != ProtocolResponses || ValidateAuthStyle(t.AuthStyle) != nil || protocol == ProtocolResponses && t.AuthStyle != AuthBearer {
		return res, nil
	}
	target := entry + "/v1/messages"
	authPayload, toolPayload := authBody(t.Model), toolBody(t.Model)
	valid, verdict := isMessage, toolVerdict
	if protocol == ProtocolResponses {
		target = entry + "/responses"
		authPayload, toolPayload = responsesBody(t.Model, false), responsesBody(t.Model, true)
		valid, verdict = isResponse, responseToolVerdict
	}
	host, err := hostOf(target)
	if err != nil {
		return ProbeResult{}, ErrBaseURLNotAllowed
	}
	tr := p.transport(host)
	defer tr.CloseIdleConnections()
	client := &http.Client{
		Transport:     tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	status, body, ok := p.step(ctx, client, target, t, authPayload)
	if !ok {
		return res, nil
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		res.Auth = AuthRejected
		return res, nil
	case status < 200 || status > 299 || !valid(body):
		return res, nil
	}
	res.Auth = AuthOK

	res.Tool = ToolInconclusive
	status, body, ok = p.step(ctx, client, target, t, toolPayload)
	if !ok || status < 200 || status > 299 || !valid(body) {
		return res, nil
	}
	res.Tool = verdict(body)
	return res, nil
}

func hostOf(rawURL string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, rawURL, nil)
	if err != nil {
		return "", err
	}
	return req.URL.Hostname(), nil
}

func authBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
	})
	return b
}

func toolBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 64,
		"messages":   []map[string]string{{"role": "user", "content": "Call the uzi_probe tool with ok set to true."}},
		"tools": []map[string]any{{
			"name":        probeToolName,
			"description": "Connectivity probe. Call it with ok set to true.",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
			},
		}},
		"tool_choice": map[string]string{"type": "tool", "name": probeToolName},
	})
	return b
}

// step sends one request under its own timeout. ok is false for anything that is "inconclusive"
// regardless of status: a transport error, a timeout, an oversized or unreadable 2xx body. The body
// is read (bounded) only for a 2xx; any other status returns a nil body.
func (p *Prober) step(ctx context.Context, client *http.Client, target string, t ProbeTarget, payload []byte) (status int, body []byte, ok bool) {
	if len(payload) > MaxProbeRequestBytes {
		return 0, nil, false
	}
	sctx, cancel := context.WithTimeout(ctx, p.stepTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, false
	}
	if t.Protocol == "" || t.Protocol == ProtocolAnth {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	req.Header.Set("content-type", "application/json")
	if t.AuthStyle == AuthXAPIKey {
		req.Header.Set("x-api-key", string(t.Key))
	} else {
		req.Header.Set("Authorization", "Bearer "+string(t.Key))
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, nil, true
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxProbeResponseBytes+1))
	if err != nil || len(raw) > MaxProbeResponseBytes {
		return resp.StatusCode, nil, false
	}
	return resp.StatusCode, raw, true
}

// messageEnvelope returns the top-level keys of a Messages-valid response body: type "message",
// role "assistant", a content array and a stop_reason key (null allowed). ok is false otherwise.
func messageEnvelope(body []byte) (map[string]json.RawMessage, bool) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	var typ, role string
	if json.Unmarshal(m["type"], &typ) != nil || typ != "message" {
		return nil, false
	}
	if json.Unmarshal(m["role"], &role) != nil || role != "assistant" {
		return nil, false
	}
	if c := bytes.TrimSpace(m["content"]); len(c) == 0 || c[0] != '[' {
		return nil, false
	}
	if _, present := m["stop_reason"]; !present {
		return nil, false
	}
	return m, true
}

func isMessage(body []byte) bool {
	_, ok := messageEnvelope(body)
	return ok
}

// toolVerdict classifies the forced-tool reply's content blocks. Each block is an object with a
// string type; only the fields of the types the probe understands are checked, so an unknown block
// type with odd fields is tolerated. A "text" block needs a string text; a "tool_use" block needs a
// string id, a string name and an object input. ToolPassed: a valid tool_use block names the probe
// tool. ToolNotSupported: every block is well formed and none does. ToolInconclusive: the content
// is malformed (a non-object block, a missing or non-string type, or a text or tool_use block
// whose fields are missing, null or of the wrong type).
func toolVerdict(body []byte) string {
	m, ok := messageEnvelope(body)
	if !ok {
		return ToolInconclusive
	}
	var raws []json.RawMessage
	if json.Unmarshal(m["content"], &raws) != nil {
		return ToolInconclusive
	}
	passed := false
	for _, raw := range raws {
		if r := bytes.TrimSpace(raw); len(r) == 0 || r[0] != '{' {
			return ToolInconclusive
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &head) != nil || head.Type == "" {
			return ToolInconclusive
		}
		switch head.Type {
		case "text":
			var b struct {
				Text *string `json:"text"`
			}
			if json.Unmarshal(raw, &b) != nil || b.Text == nil {
				return ToolInconclusive
			}
		case "tool_use":
			var b struct {
				ID    *string         `json:"id"`
				Name  *string         `json:"name"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(raw, &b) != nil || b.ID == nil || b.Name == nil {
				return ToolInconclusive
			}
			if in := bytes.TrimSpace(b.Input); len(in) == 0 || in[0] != '{' {
				return ToolInconclusive
			}
			if *b.Name == probeToolName {
				passed = true
			}
		}
	}
	if passed {
		return ToolPassed
	}
	return ToolNotSupported
}

// transport returns a one-use transport for host. Its DialContext ignores the address the
// transport asks for and resolves host itself, refuses the whole host if ANY answer is refused,
// and dials only the checked addresses (pinned: no second lookup a rebinding answer could land
// in). Proxy is nil, so proxy environment variables are ignored and every connection is direct;
// SNI and certificate checks use host.
func (p *Prober) transport(host string) *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			_, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			port, err := strconv.ParseUint(portStr, 10, 16)
			if err != nil {
				return nil, err
			}
			ips, err := p.resolveChecked(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				c, err := p.dialPinned(ctx, netip.AddrPortFrom(ip, uint16(port)))
				if err == nil {
					return c, nil
				}
				lastErr = err
				if ctx.Err() != nil {
					break
				}
			}
			return nil, lastErr
		},
		TLSClientConfig: &tls.Config{
			ServerName: host,
			RootCAs:    p.cfg.RootCAs,
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout:    10 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		DisableCompression:     true,
		DisableKeepAlives:      true,
	}
}

// resolveChecked resolves host and returns its addresses only if every one passes the policy.
func (p *Prober) resolveChecked(ctx context.Context, host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{a}
	} else {
		lookup := p.resolve
		if lookup == nil {
			lookup = func(ctx context.Context, h string) ([]netip.Addr, error) {
				return net.DefaultResolver.LookupNetIP(ctx, "ip", h)
			}
		}
		if addrs, err = lookup(ctx, host); err != nil {
			return nil, err
		}
	}
	if len(addrs) == 0 {
		return nil, errors.New("the host did not resolve")
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if addrRefused(a, p.cfg.AllowLoopback) {
			return nil, errAddressRefused
		}
		out = append(out, a.WithZone("").Unmap())
	}
	return out, nil
}

// dialPinned connects to one checked address. The policy is applied once more at the socket.
func (p *Prober) dialPinned(ctx context.Context, ap netip.AddrPort) (net.Conn, error) {
	if addrRefused(ap.Addr(), p.cfg.AllowLoopback) {
		return nil, errAddressRefused
	}
	if p.dial != nil {
		return p.dial(ctx, ap)
	}
	d := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			got, err := netip.ParseAddrPort(address)
			if err != nil || addrRefused(got.Addr(), p.cfg.AllowLoopback) {
				return errAddressRefused
			}
			return nil
		},
	}
	return d.DialContext(ctx, "tcp", ap.String())
}

// responsesBody keeps the Responses probe non-streaming and forces the same diagnostic tool.
func responsesBody(model string, tool bool) []byte {
	m := map[string]any{"model": model, "input": "ping", "max_output_tokens": 16, "stream": false}
	if tool {
		m["input"] = "Call the uzi_probe tool with ok set to true."
		m["max_output_tokens"] = 64
		m["tools"] = []any{map[string]any{"type": "function", "name": probeToolName, "description": "Connectivity probe.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]string{"type": "boolean"}}, "required": []string{"ok"}, "additionalProperties": false}, "strict": true}}
		m["tool_choice"] = map[string]string{"type": "function", "name": probeToolName}
	}
	b, _ := json.Marshal(m)
	return b
}

func responseOutput(body []byte) ([]json.RawMessage, bool) {
	var m struct {
		Object string
		Output json.RawMessage
	}
	if json.Unmarshal(body, &m) != nil || m.Object != "response" {
		return nil, false
	}
	if b := bytes.TrimSpace(m.Output); len(b) == 0 || b[0] != '[' {
		return nil, false
	}
	var output []json.RawMessage
	if json.Unmarshal(m.Output, &output) != nil {
		return nil, false
	}
	return output, true
}
func isResponse(body []byte) bool { _, ok := responseOutput(body); return ok }

func responseToolVerdict(body []byte) string {
	output, ok := responseOutput(body)
	if !ok {
		return ToolInconclusive
	}
	passed := false
	// The response byte limit bounds this scan; malformed siblings invalidate the reply.
	for _, raw := range output {
		var item struct {
			Type      string
			Name      string
			Arguments *string
		}
		if json.Unmarshal(raw, &item) != nil || item.Type == "" {
			return ToolInconclusive
		}
		if item.Type != "function_call" {
			continue
		}
		if item.Name == "" || item.Arguments == nil {
			return ToolInconclusive
		}
		var args struct {
			OK *bool `json:"ok"`
		}
		if json.Unmarshal([]byte(*item.Arguments), &args) != nil {
			return ToolInconclusive
		}
		if item.Name == probeToolName && args.OK != nil && *args.OK {
			passed = true
		}
	}
	if passed {
		return ToolPassed
	}
	return ToolNotSupported
}
