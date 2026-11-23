package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestResponsesProbe(t *testing.T) {
	good := `{"object":"response","output":[]}`
	tool := `{"object":"response","output":[{"type":"function_call","name":"uzi_probe","arguments":"{\"ok\":true}"}]}`
	cases := []struct {
		name       string
		auth, tool reply
		want       ProbeResult
	}{
		{"positive", reply{status: 200, body: good}, reply{status: 200, body: tool}, ProbeResult{AuthOK, ToolPassed}},
		{"badbody", reply{status: 200, body: "{}"}, reply{}, ProbeResult{AuthInconclusive, ToolNotRun}},
		{"401", reply{status: 401}, reply{}, ProbeResult{AuthRejected, ToolNotRun}},
		{"403", reply{status: 403}, reply{}, ProbeResult{AuthRejected, ToolNotRun}},
		{"500", reply{status: 500}, reply{}, ProbeResult{AuthInconclusive, ToolNotRun}},
		{"oversized", reply{status: 200, body: strings.Repeat("x", MaxProbeResponseBytes+1)}, reply{}, ProbeResult{AuthInconclusive, ToolNotRun}},
		{"timeout", reply{hang: true}, reply{}, ProbeResult{AuthInconclusive, ToolNotRun}},
		{"badtool", reply{status: 200, body: good}, reply{status: 200, body: "{}"}, ProbeResult{AuthOK, ToolInconclusive}},
		{"false", reply{status: 200, body: good}, reply{status: 200, body: strings.Replace(tool, "true", "false", 1)}, ProbeResult{AuthOK, ToolNotSupported}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newGateway(t, c.auth, c.tool)
			p, _ := testProber(t, g, true, "127.0.0.1")
			if c.auth.hang {
				p.stepTimeout = 50 * time.Millisecond
			}
			target := target()
			target.Protocol = ProtocolResponses
			got, err := p.Probe(context.Background(), target)
			if err != nil || got != c.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, c.want)
			}
			requests := g.requests()
			wantRequests := 1
			if c.want.Auth == AuthOK {
				wantRequests = 2
			}
			// The deadline includes TLS setup, so a stalled probe can expire before HTTP.
			// Either way it must never proceed to a tool request.
			if c.auth.hang && len(requests) > 1 || !c.auth.hang && len(requests) != wantRequests {
				t.Fatalf("requests=%d want=%d (timeout permits zero)", len(requests), wantRequests)
			}
			for i, r := range requests {
				if r.path != "/anthropic/responses" || r.header.Get("Authorization") != "Bearer "+probeKey || r.header.Get("anthropic-version") != "" {
					t.Fatalf("request shape: %+v", r)
				}
				var body map[string]json.RawMessage
				if json.Unmarshal(r.body, &body) != nil || body["input"] == nil || body["max_output_tokens"] == nil || string(body["stream"]) != "false" {
					t.Fatalf("body %s", r.body)
				}
				if i == 1 {
					var tools []struct {
						Type, Name string
						Strict     bool
						Parameters struct {
							Required   []string `json:"required"`
							Additional bool     `json:"additionalProperties"`
						}
					}
					var choice struct{ Type, Name string }
					if json.Unmarshal(body["tools"], &tools) != nil || len(tools) != 1 || tools[0].Type != "function" || tools[0].Name != "uzi_probe" || !tools[0].Strict || len(tools[0].Parameters.Required) != 1 || tools[0].Parameters.Required[0] != "ok" || tools[0].Parameters.Additional || json.Unmarshal(body["tool_choice"], &choice) != nil || choice.Type != "function" || choice.Name != "uzi_probe" {
						t.Fatalf("tool body %s", r.body)
					}
				}
			}
		})
	}
}
func TestResponsesUnsupportedBeforeNetwork(t *testing.T) {
	for _, pair := range [][2]string{{ProtocolResponses, AuthXAPIKey}, {"unknown", AuthBearer}, {ProtocolAnth, "unknown"}} {
		p, d := testProber(t, nil, true, "127.0.0.1")
		target := target()
		target.Protocol = pair[0]
		target.AuthStyle = pair[1]
		got, err := p.Probe(context.Background(), target)
		if err != nil || got.Auth != AuthInconclusive || len(d.recorded()) != 0 {
			t.Fatalf("%+v %v", got, err)
		}
	}
}
func TestResponsesRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, step := range []string{"auth", "tool"} {
			t.Run(http.StatusText(status)+step, func(t *testing.T) {
				receiver := newGateway(t, reply{status: 200, body: `{"object":"response","output":[]}`}, reply{})
				resp, err := receiver.srv.Client().Get(receiver.srv.URL + "/receiver-positive-control")
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				before := len(receiver.requests())
				if before != 1 {
					t.Fatalf("receiver positive control count=%d", before)
				}
				redir := reply{status: status, location: receiver.srv.URL}
				auth, tool := redir, reply{}
				want := ProbeResult{AuthInconclusive, ToolNotRun}
				if step == "tool" {
					auth = reply{status: 200, body: `{"object":"response","output":[]}`}
					tool = redir
					want = ProbeResult{AuthOK, ToolInconclusive}
				}
				g := newGateway(t, auth, tool)
				p, _ := testProber(t, g, true, "127.0.0.1")
				target := target()
				target.Protocol = ProtocolResponses
				got, err := p.Probe(context.Background(), target)
				if err != nil || got != want || len(receiver.requests()) != before {
					t.Fatalf("%+v %v receiver=%d", got, err, len(receiver.requests()))
				}
			})
		}
	}
}

func TestResponsesV1Path(t *testing.T) {
	g := newGateway(t, reply{status: 200, body: `{"object":"response","output":[]}`}, reply{status: 200, body: `{"object":"response","output":[]}`})
	p, _ := testProber(t, g, true, "127.0.0.1")
	base := "https://example.com/v1"
	p.cfg.Allowed = func(u string) (string, bool) { return base, u == base }
	target := target()
	target.Protocol = ProtocolResponses
	target.BaseURL = base
	got, err := p.Probe(context.Background(), target)
	if err != nil || got.Auth != AuthOK {
		t.Fatalf("%+v %v", got, err)
	}
	if len(g.requests()) != 2 {
		t.Fatal("missing requests")
	}
	for _, r := range g.requests() {
		if r.path != "/v1/responses" {
			t.Fatalf("path=%s", r.path)
		}
	}
}
