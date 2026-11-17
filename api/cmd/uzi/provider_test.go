package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const providerTestID = "11111111-1111-4111-8111-111111111111"

func ptrInt(n int) *int { return &n }

func providerFixture(name string) apitypes.ProviderDTO {
	return apitypes.ProviderDTO{
		ID: providerTestID, Name: name, Engine: "claude", Protocol: "anthropic_messages",
		BaseURL: "https://gw.example.com/anthropic", AuthStyle: "bearer", CurrentRevision: 1,
		ModelMap: apitypes.ModelMapDTO{
			Haiku:  apitypes.ModelEntryDTO{Model: "corp-haiku", ContextWindow: ptrInt(200000)},
			Sonnet: &apitypes.ModelEntryDTO{Model: "corp-sonnet"},
		},
	}
}

func enabledList(ps ...apitypes.ProviderDTO) apitypes.ProviderListDTO {
	return apitypes.ProviderListDTO{Enabled: true, Providers: ps, AllowedBaseURLs: []string{"https://gw.example.com/anthropic"}}
}

func TestProviderListTable(t *testing.T) {
	keyed := providerFixture("Corp")
	keyed.HasKey, keyed.KeyEnabled = true, true
	keyed.Probe = &apitypes.ProviderProbeDTO{AuthResult: "ok", ToolResult: "passed", ProbedAt: time.Now()}
	off := providerFixture("second")
	off.ID = "22222222-2222-4222-8222-222222222222"
	off.HasKey, off.KeyEnabled, off.Retired = true, false, true
	fc := &uzicli.FakeClient{ProviderList: enabledList(keyed, off)}
	out, errOut, code := runCLI(t, fakeEnv(fc), "provider", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"NAME", "URL", "AUTH", "MODELS", "KEY", "TEST", "RETIRED",
		"Corp", "https://gw.example.com/anthropic", "haiku=corp-haiku:200000", "sonnet=corp-sonnet", "ok/passed", "disabled", "true"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestProviderListFeatureOff(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderList: apitypes.ProviderListDTO{}}
	out, _, code := runCLI(t, fakeEnv(fc), "provider", "list")
	if code != uzicli.ExitOK || !strings.Contains(out, "not enabled") || !strings.Contains(out, "UZI_PROVIDER_ALLOWED_BASE_URLS") {
		t.Errorf("off state: code %d, out %q", code, out)
	}
	// A script gets the structured off state, with arrays rather than null.
	out, _, code = runCLI(t, fakeEnv(fc), "--json", "provider", "list")
	var got map[string]any
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("json off state: code %d, out %q", code, out)
	}
	if got["enabled"] != false {
		t.Errorf("enabled = %v", got["enabled"])
	}
	if _, ok := got["providers"].([]any); !ok {
		t.Errorf("providers = %v, want an array", got["providers"])
	}
	if _, ok := got["allowed_base_urls"].([]any); !ok {
		t.Errorf("allowed_base_urls = %v, want an array", got["allowed_base_urls"])
	}
}

func TestProviderListEmptyShowsAllowedURLs(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderList: enabledList()}
	out, _, code := runCLI(t, fakeEnv(fc), "provider", "list")
	if code != uzicli.ExitOK || !strings.Contains(out, "uzi provider create") || !strings.Contains(out, "https://gw.example.com/anthropic") {
		t.Errorf("empty state: code %d, out %q", code, out)
	}
}

func TestProviderListJSON(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderList: enabledList(providerFixture("corp"))}
	out, _, code := runCLI(t, fakeEnv(fc), "--json", "provider", "list")
	var got apitypes.ProviderListDTO
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil || !got.Enabled || len(got.Providers) != 1 || got.Providers[0].Name != "corp" {
		t.Errorf("json list: code %d, out %q", code, out)
	}
}

func TestProviderCreateSendsMapAndHintsKey(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderResult: providerFixture("corp")}
	out, errOut, code := runCLI(t, fakeEnv(fc), "provider", "create", "--name", "corp",
		"--base-url", "https://gw.example.com/anthropic",
		"--model", "haiku=corp-haiku:200000", "--model", "sonnet=corp-sonnet", "--model", "opus=arn:aws:bedrock:model/x:0")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, errOut)
	}
	r := fc.LastProviderCreate
	if r == nil || r.Name != "corp" || r.AuthStyle != "bearer" || r.Engine != "claude" || r.Protocol != "anthropic_messages" || r.BaseURL != "https://gw.example.com/anthropic" {
		t.Fatalf("request = %+v", r)
	}
	if r.ModelMap.Haiku.Model != "corp-haiku" || r.ModelMap.Haiku.ContextWindow == nil || *r.ModelMap.Haiku.ContextWindow != 200000 {
		t.Errorf("haiku = %+v", r.ModelMap.Haiku)
	}
	if r.ModelMap.Sonnet == nil || r.ModelMap.Sonnet.Model != "corp-sonnet" || r.ModelMap.Sonnet.ContextWindow != nil {
		t.Errorf("sonnet = %+v", r.ModelMap.Sonnet)
	}
	// A trailing ":0" is part of a Bedrock-style model id, not a window.
	if r.ModelMap.Opus == nil || r.ModelMap.Opus.Model != "arn:aws:bedrock:model/x:0" || r.ModelMap.Opus.ContextWindow != nil {
		t.Errorf("opus = %+v", r.ModelMap.Opus)
	}
	if !strings.Contains(out, "web Settings") || !strings.Contains(out, "`uzi provider test corp`") {
		t.Errorf("create output lacks the key hint:\n%s", out)
	}
}

func TestProviderCreateRejectsBadFlags(t *testing.T) {
	base := []string{"provider", "create", "--name", "corp", "--base-url", "https://gw.example.com/anthropic"}
	cases := map[string][]string{
		"no haiku":         {"--model", "sonnet=x"},
		"no model":         {},
		"bad alias":        {"--model", "turbo=x"},
		"no equals":        {"--model", "haiku"},
		"duplicate":        {"--model", "haiku=a", "--model", "haiku=b"},
		"bad auth style":   {"--model", "haiku=a", "--auth-style", "basic"},
		"empty model name": {"--model", "haiku="},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{ProviderResult: providerFixture("corp")}
			_, _, code := runCLI(t, fakeEnv(fc), append(append([]string{}, base...), extra...)...)
			if code != uzicli.ExitUsage {
				t.Errorf("exit = %d, want usage (%d)", code, uzicli.ExitUsage)
			}
			if fc.LastProviderCreate != nil {
				t.Errorf("a request was sent: %+v", fc.LastProviderCreate)
			}
		})
	}
	// name and base URL are required.
	fc := &uzicli.FakeClient{}
	if _, _, code := runCLI(t, fakeEnv(fc), "provider", "create", "--base-url", "https://x", "--model", "haiku=a"); code != uzicli.ExitUsage {
		t.Errorf("missing --name exit = %d", code)
	}
	if _, errOut, code := runCLI(t, fakeEnv(fc), "provider", "create", "--name", "n", "--model", "haiku=a"); code != uzicli.ExitUsage || !strings.Contains(errOut, "uzi provider list --json") {
		t.Errorf("missing --base-url exit = %d, stderr %q", code, errOut)
	}
}

func TestProviderCreateHasNoKeyFlag(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "provider", "create", "--name", "n", "--base-url", "https://x", "--model", "haiku=a", "--key", "sk-secret")
	if code == uzicli.ExitOK || fc.LastProviderCreate != nil {
		t.Errorf("a --key flag must not exist (code %d, sent %+v)", code, fc.LastProviderCreate)
	}
	provider := findCmd(newRootCmd(fakeEnv(fc)), "provider")
	for _, verb := range []string{"use", "delete", "key"} {
		if findCmd(provider, verb) != nil {
			t.Errorf("provider %s must not exist", verb)
		}
	}
}

func TestProviderCreateSurfacesReason(t *testing.T) {
	fc := &uzicli.FakeClient{Err: &uzicli.ExitError{
		Code: uzicli.ExitUsage, Err: errors.New("the base URL is not one your operator allowed"), Reason: "provider_base_url_not_allowed",
	}}
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "create", "--name", "n", "--base-url", "https://x", "--model", "haiku=a")
	if code != uzicli.ExitUsage || !strings.Contains(errOut, "not one your operator allowed") || !strings.Contains(errOut, "provider_base_url_not_allowed") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
}

func TestProviderEdit(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderList: enabledList(providerFixture("Corp")), ProviderResult: providerFixture("renamed")}
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "edit", "corp", "--name", "renamed", "--model", "haiku=h2")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, errOut)
	}
	p := fc.LastProviderPatch
	if fc.LastProviderID != providerTestID || p == nil || p.Name == nil || *p.Name != "renamed" || p.ModelMap == nil || p.ModelMap.Haiku.Model != "h2" {
		t.Errorf("id %q patch %+v", fc.LastProviderID, p)
	}

	// Rename only leaves the model map alone.
	fc = &uzicli.FakeClient{ProviderList: enabledList(providerFixture("Corp")), ProviderResult: providerFixture("x")}
	if _, _, code := runCLI(t, fakeEnv(fc), "provider", "edit", providerTestID, "--name", "x"); code != uzicli.ExitOK || fc.LastProviderPatch.ModelMap != nil {
		t.Errorf("rename only: code %d patch %+v", code, fc.LastProviderPatch)
	}
}

func TestProviderEditRefusals(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderList: enabledList(providerFixture("corp"))}
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "edit", "corp", "--base-url", "https://elsewhere.example.com")
	if code != uzicli.ExitUsage || !strings.Contains(errOut, "web-only") || fc.LastProviderPatch != nil {
		t.Errorf("--base-url: code %d stderr %q", code, errOut)
	}
	_, errOut, code = runCLI(t, fakeEnv(fc), "provider", "edit", "corp", "--auth-style", "x_api_key")
	if code != uzicli.ExitUsage || !strings.Contains(errOut, "web-only") {
		t.Errorf("--auth-style: code %d stderr %q", code, errOut)
	}
	if _, _, code := runCLI(t, fakeEnv(fc), "provider", "edit", "corp"); code != uzicli.ExitUsage {
		t.Errorf("no change requested: exit %d", code)
	}
	if _, _, code := runCLI(t, fakeEnv(fc), "provider", "edit", "corp", "--model", "sonnet=x"); code != uzicli.ExitUsage {
		t.Errorf("map without haiku: exit %d", code)
	}
}

func TestProviderTestPrintsBothResults(t *testing.T) {
	fc := &uzicli.FakeClient{
		ProviderList:  enabledList(providerFixture("corp")),
		ProviderProbe: &apitypes.ProviderProbeDTO{AuthResult: "ok", ToolResult: "not_supported", ProbedAt: time.Now()},
	}
	out, errOut, code := runCLI(t, fakeEnv(fc), "provider", "test", "CORP")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, errOut)
	}
	if fc.LastProviderID != providerTestID || fc.ProviderTestCalls != 1 {
		t.Errorf("tested %q x%d", fc.LastProviderID, fc.ProviderTestCalls)
	}
	for _, want := range []string{"auth:  ok", "tools: not_supported", "Neither result certifies full compatibility"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	fc.ProviderProbe = &apitypes.ProviderProbeDTO{AuthResult: "rejected", ToolResult: "not_run"}
	out, _, _ = runCLI(t, fakeEnv(fc), "provider", "test", "corp")
	if !strings.Contains(out, "auth:  rejected") || !strings.Contains(out, "rejected the key") {
		t.Errorf("rejected output:\n%s", out)
	}

	out, _, code = runCLI(t, fakeEnv(fc), "--json", "provider", "test", "corp")
	var got struct {
		Name  string `json:"name"`
		Probe struct {
			AuthResult string `json:"auth_result"`
		} `json:"probe"`
	}
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil || got.Name != "corp" || got.Probe.AuthResult != "rejected" {
		t.Errorf("json: code %d out %q", code, out)
	}
}

func TestProviderTestRateLimitedReason(t *testing.T) {
	// The shared fake error would fail the list too, so only TestProvider fails here.
	fc := &testFailingProbe{
		FakeClient: &uzicli.FakeClient{ProviderList: enabledList(providerFixture("corp"))},
		err:        &uzicli.ExitError{Code: uzicli.ExitUnreachable, Err: errors.New("too many provider tests; try again later"), Reason: "provider_probe_rate_limited"},
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "test", "corp")
	if code != uzicli.ExitUnreachable || !strings.Contains(errOut, "provider_probe_rate_limited") {
		t.Errorf("code %d stderr %q", code, errOut)
	}
}

// testFailingProbe fails only TestProvider so name resolution (a list) still succeeds.
type testFailingProbe struct {
	*uzicli.FakeClient
	err error
}

func (f *testFailingProbe) TestProvider(_ context.Context, _ string) (*apitypes.ProviderProbeDTO, error) {
	return nil, f.err
}

func TestProviderRetire(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderList: enabledList(providerFixture("corp")), ProviderResult: providerFixture("corp")}
	out, errOut, code := runCLI(t, fakeEnv(fc), "provider", "retire", "Corp")
	if code != uzicli.ExitOK || fc.ProviderRetireCalls != 1 || fc.LastProviderID != providerTestID || !strings.Contains(out, "Retired") {
		t.Errorf("code %d calls %d id %q out %q err %q", code, fc.ProviderRetireCalls, fc.LastProviderID, out, errOut)
	}
}

func TestProviderNameOrIDResolution(t *testing.T) {
	other := providerFixture("Other")
	other.ID = "33333333-3333-4333-8333-333333333333"
	fc := &uzicli.FakeClient{ProviderList: enabledList(providerFixture("Corp-GW"), other), ProviderResult: providerFixture("x")}

	// Case-insensitive name.
	if _, _, code := runCLI(t, fakeEnv(fc), "provider", "retire", "cOrP-gw"); code != uzicli.ExitOK || fc.LastProviderID != providerTestID {
		t.Errorf("name: code %d id %q", code, fc.LastProviderID)
	}
	// Exact id picks the matching row, not the first.
	if _, _, code := runCLI(t, fakeEnv(fc), "provider", "retire", other.ID); code != uzicli.ExitOK || fc.LastProviderID != other.ID {
		t.Errorf("id: code %d id %q", code, fc.LastProviderID)
	}
	// Unknown: not found, and nothing written.
	before := fc.ProviderRetireCalls
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "retire", "nope")
	if code != uzicli.ExitNotFound || fc.ProviderRetireCalls != before || !strings.Contains(errOut, "uzi provider list") {
		t.Errorf("unknown: code %d stderr %q", code, errOut)
	}
	// Feature off: a distinct, readable refusal.
	off := &uzicli.FakeClient{}
	_, errOut, code = runCLI(t, fakeEnv(off), "provider", "retire", "corp")
	if code != uzicli.ExitConflict || !strings.Contains(errOut, "not enabled") {
		t.Errorf("off: code %d stderr %q", code, errOut)
	}
}

func TestProviderRetiredReasonSurfaced(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderList: enabledList(providerFixture("corp"))}
	fcw := &testFailingRetire{FakeClient: fc, err: &uzicli.ExitError{Code: uzicli.ExitConflict, Err: errors.New("the provider is retired"), Reason: "provider_retired"}}
	_, errOut, code := runCLI(t, fakeEnv(fcw), "provider", "retire", "corp")
	if code != uzicli.ExitConflict || !strings.Contains(errOut, "provider_retired") {
		t.Errorf("code %d stderr %q", code, errOut)
	}
}

type testFailingRetire struct {
	*uzicli.FakeClient
	err error
}

func (f *testFailingRetire) RetireProvider(_ context.Context, _ string) (apitypes.ProviderDTO, error) {
	return apitypes.ProviderDTO{}, f.err
}

// A provider name is user-authored text: control sequences and bidi overrides must not reach
// the terminal raw from any verb's output.
func TestProviderUnsafeNameNeverReachesTerminalRaw(t *testing.T) {
	hostile := providerFixture(hostileANSI + hostileBidi)
	hostile.BaseURL = "https://gw.example.com/" + hostileANSI
	hostile.ModelMap.Haiku.Model = "m" + hostileANSI
	hostile.Probe = &apitypes.ProviderProbeDTO{AuthResult: "ok" + hostileANSI, ToolResult: hostileBidi}
	list := enabledList(hostile)
	list.AllowedBaseURLs = []string{"https://gw.example.com/" + hostileANSI}

	check := func(label, out string) {
		t.Helper()
		for _, bad := range []string{"\x1b", "\u202e"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s output carries raw %q: %q", label, bad, out)
			}
		}
	}
	fc := &uzicli.FakeClient{ProviderList: list, ProviderResult: hostile, ProviderProbe: hostile.Probe}
	out, errOut, _ := runCLI(t, fakeEnv(fc), "provider", "list")
	check("list", out+errOut)
	out, errOut, _ = runCLI(t, fakeEnv(fc), "provider", "test", providerTestID)
	check("test", out+errOut)
	out, errOut, _ = runCLI(t, fakeEnv(fc), "provider", "retire", providerTestID)
	check("retire", out+errOut)
	out, errOut, _ = runCLI(t, fakeEnv(fc), "provider", "create", "--name", "n", "--base-url", "https://x", "--model", "haiku=a")
	check("create", out+errOut)
	out, errOut, _ = runCLI(t, fakeEnv(fc), "provider", "edit", providerTestID, "--name", "z")
	check("edit", out+errOut)
	empty := &uzicli.FakeClient{ProviderList: apitypes.ProviderListDTO{Enabled: true, AllowedBaseURLs: list.AllowedBaseURLs}}
	out, errOut, _ = runCLI(t, fakeEnv(empty), "provider", "list")
	check("empty list", out+errOut)
	// An unknown name echoed in the not-found error is user-typed text too.
	_, errOut, _ = runCLI(t, fakeEnv(fc), "provider", "retire", hostileANSI)
	check("not found", errOut)
}
