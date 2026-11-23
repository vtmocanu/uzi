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

func TestProviderWarnsContextWindowOnClaudeNames(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderResult: providerFixture("corp")}
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "create", "--name", "corp",
		"--base-url", "https://gw.example.com/anthropic",
		"--model", "haiku=corp-haiku:200000", "--model", "sonnet=us.anthropic.Claude-sonnet-4-5:50000", "--model", "opus=claude-opus")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, errOut)
	}
	const wantTail = "the Claude CLI may ignore the context window (configured or uzi's 200000 default) for model names it recognizes as Anthropic models, such as claude-sonnet-4-5 (measured at Claude Code 2.1.325)"
	if !strings.Contains(errOut, "warning: --model sonnet: "+wantTail) {
		t.Errorf("create stderr lacks the sonnet warning: %q", errOut)
	}
	// A claude-named model with no window warns too.
	if !strings.Contains(errOut, "warning: --model opus: "+wantTail) {
		t.Errorf("create stderr lacks the windowless opus warning: %q", errOut)
	}
	if strings.Contains(errOut, "--model haiku") {
		t.Errorf("warned for a non-claude name: %q", errOut)
	}
	if strings.Contains(errOut, "us.anthropic.Claude") || strings.Contains(errOut, "claude-opus") {
		t.Errorf("echoed an untrusted model name: %q", errOut)
	}
	fc = &uzicli.FakeClient{ProviderList: enabledList(providerFixture("corp")), ProviderResult: providerFixture("corp")}
	_, errOut, code = runCLI(t, fakeEnv(fc), "provider", "edit", providerTestID, "--model", "haiku=claude-haiku-4-5:100000")
	if code != uzicli.ExitOK || !strings.Contains(errOut, "warning: --model haiku: the Claude CLI may ignore the context window") {
		t.Errorf("edit: code %d stderr %q", code, errOut)
	}
	_, errOut, _ = runCLI(t, fakeEnv(fc), "provider", "edit", providerTestID, "--model", "haiku=corp-haiku:100000")
	if strings.Contains(errOut, "warning") {
		t.Errorf("warned for a non-claude name: %q", errOut)
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
	for _, verb := range []string{"delete", "key"} {
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

func TestParseModelFlagsContextWindowSuffix(t *testing.T) {
	mm, err := parseModelFlags([]string{"haiku=corp:model", "sonnet=bedrock-v1:0", "opus=big:200000"})
	if err != nil {
		t.Fatal(err)
	}
	if mm.Haiku.Model != "corp:model" || mm.Haiku.ContextWindow != nil {
		t.Fatalf("haiku = %+v", mm.Haiku)
	}
	if mm.Sonnet.Model != "bedrock-v1:0" || mm.Sonnet.ContextWindow != nil {
		t.Fatalf("sonnet = %+v", mm.Sonnet)
	}
	if mm.Opus.Model != "big" || mm.Opus.ContextWindow == nil || *mm.Opus.ContextWindow != 200000 {
		t.Fatalf("opus = %+v", mm.Opus)
	}
	_, err = parseModelFlags([]string{"haiku=corp:99999999999999999999999"})
	var ee *uzicli.ExitError
	if !errors.As(err, &ee) || ee.Code != uzicli.ExitUsage {
		t.Fatalf("overflowing window: err = %v", err)
	}
}

func TestProviderUseSelectsByNameAndID(t *testing.T) {
	keyed := providerFixture("Corp GW")
	keyed.HasKey, keyed.KeyEnabled = true, true
	fc := &uzicli.FakeClient{ProviderList: enabledList(keyed)}
	out, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", "corp gw")
	if code != uzicli.ExitOK || fc.ProviderSourceCalls != 1 || fc.LastProviderSource == nil || *fc.LastProviderSource != providerTestID {
		t.Fatalf("by name: code %d calls %d sent %v stderr %q", code, fc.ProviderSourceCalls, fc.LastProviderSource, errOut)
	}
	if !strings.Contains(out, `use provider "Corp GW"`) {
		t.Errorf("output = %q", out)
	}
	if _, _, code := runCLI(t, fakeEnv(fc), "provider", "use", providerTestID); code != uzicli.ExitOK || fc.ProviderSourceCalls != 2 {
		t.Errorf("by id: code %d calls %d", code, fc.ProviderSourceCalls)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "--json", "provider", "use", "corp gw")
	var got struct {
		ID   *string `json:"claude_provider_id"`
		Name *string `json:"name"`
	}
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil || got.ID == nil || *got.ID != providerTestID || got.Name == nil || *got.Name != "Corp GW" {
		t.Errorf("json: code %d out %q", code, out)
	}
}

func TestProviderUseNoneClears(t *testing.T) {
	fc := &uzicli.FakeClient{}
	out, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", "--none")
	if code != uzicli.ExitOK || !fc.ProviderSourceSet || fc.LastProviderSource != nil || fc.ProviderSourceCalls != 1 {
		t.Fatalf("code %d set=%v sent=%v stderr %q", code, fc.ProviderSourceSet, fc.LastProviderSource, errOut)
	}
	if !strings.Contains(out, "Anthropic directly") {
		t.Errorf("output = %q", out)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "--json", "provider", "use", "--none")
	if code != uzicli.ExitOK || !strings.Contains(out, `"claude_provider_id": null`) && !strings.Contains(out, `"claude_provider_id":null`) {
		t.Errorf("json: code %d out %q", code, out)
	}
}

func TestProviderUseArgumentErrors(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderList: enabledList(providerFixture("corp"))}
	for _, args := range [][]string{{"provider", "use"}, {"provider", "use", "corp", "--none"}, {"provider", "use", "a", "b"}} {
		if _, _, code := runCLI(t, fakeEnv(fc), args...); code != uzicli.ExitUsage || fc.ProviderSourceCalls != 0 {
			t.Errorf("%v: code %d calls %d, want usage and no write", args, code, fc.ProviderSourceCalls)
		}
	}
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", "nope")
	if code != uzicli.ExitNotFound || fc.ProviderSourceCalls != 0 || !strings.Contains(errOut, "uzi provider list") {
		t.Errorf("unknown: code %d stderr %q", code, errOut)
	}
}

// A provider the list already shows is unusable is refused before any write, naming the fix.
func TestProviderUseRefusesUnusableBeforeWrite(t *testing.T) {
	retired := providerFixture("old")
	retired.HasKey, retired.KeyEnabled, retired.Retired = true, true, true
	keyless := providerFixture("bare")
	keyless.ID = "22222222-2222-4222-8222-222222222222"
	disabled := providerFixture("off")
	disabled.ID = "33333333-3333-4333-8333-333333333333"
	disabled.HasKey = true
	fc := &uzicli.FakeClient{ProviderList: enabledList(retired, keyless, disabled)}
	for name, want := range map[string]string{"old": "retired", "bare": "no key", "off": "key disabled"} {
		_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", name)
		if code != uzicli.ExitConflict || !strings.Contains(errOut, want) {
			t.Errorf("%s: code %d stderr %q, want conflict naming %q", name, code, errOut, want)
		}
	}
	// A fixable refusal names the retry command with the provider's id (safe to paste).
	if _, errOut, _ := runCLI(t, fakeEnv(fc), "provider", "use", "bare"); !strings.Contains(errOut, "uzi provider use "+keyless.ID) {
		t.Errorf("keyless refusal lacks the retry command: %q", errOut)
	}
	if fc.ProviderSourceCalls != 0 {
		t.Errorf("an unusable provider reached the server %d times", fc.ProviderSourceCalls)
	}
}

// The server's typed refusal (for example a base URL no longer on the allowlist, which the list
// cannot show) keeps its exit code, shows its cause and reason, and names the next step.
func TestProviderUseServerRefusalMapped(t *testing.T) {
	keyed := providerFixture("corp")
	keyed.HasKey, keyed.KeyEnabled = true, true
	fc := &uzicli.FakeClient{ProviderList: enabledList(keyed)}
	fw := &testFailingSource{FakeClient: fc, err: &uzicli.ExitError{Code: uzicli.ExitConflict,
		Err:    errors.New("the provider cannot be the Claude source: its base URL is not on the operator allowlist"),
		Reason: "provider_source_unusable"}}
	_, errOut, code := runCLI(t, fakeEnv(fw), "provider", "use", "corp")
	for _, want := range []string{"not on the operator allowlist", "provider_source_unusable", "pick another provider"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr missing %q: %q", want, errOut)
		}
	}
	if code != uzicli.ExitConflict {
		t.Errorf("code = %d, want conflict", code)
	}
	// An unrelated typed error passes through with its reason appended.
	fw.err = &uzicli.ExitError{Code: uzicli.ExitNotFound, Err: errors.New("provider not found"), Reason: "provider_not_found"}
	_, errOut, code = runCLI(t, fakeEnv(fw), "provider", "use", "corp")
	if code != uzicli.ExitNotFound || !strings.Contains(errOut, "provider_not_found") {
		t.Errorf("not found: code %d stderr %q", code, errOut)
	}
}

type testFailingSource struct {
	*uzicli.FakeClient
	err error
}

func (f *testFailingSource) SetClaudeProviderSource(_ context.Context, _ *string) (*string, error) {
	return nil, f.err
}

// The provider name is user-authored text: it must not reach the terminal raw from use.
func TestProviderUseNameNeverReachesTerminalRaw(t *testing.T) {
	hostile := providerFixture(hostileANSI + hostileBidi)
	hostile.HasKey, hostile.KeyEnabled = true, true
	fc := &uzicli.FakeClient{ProviderList: enabledList(hostile)}
	out, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", providerTestID)
	if code != uzicli.ExitOK {
		t.Fatalf("code %d stderr %q", code, errOut)
	}
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, '\u202e') {
		t.Errorf("raw control or bidi in output: %q", out)
	}
}
