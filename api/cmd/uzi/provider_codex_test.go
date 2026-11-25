package main

import (
	"context"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func codexProviderFixture() apitypes.ProviderDTO {
	d := providerFixture("code")
	d.Engine, d.Protocol = "codex", "openai_responses"
	d.HasKey, d.KeyEnabled = true, true
	d.ModelMap = apitypes.ModelMapDTO{Default: &apitypes.ModelEntryDTO{Model: "gateway:0"}}
	return d
}

func TestProviderCodexCreateAndRender(t *testing.T) {
	fc := &uzicli.FakeClient{ProviderResult: codexProviderFixture()}
	args := []string{"provider", "create", "--engine", "codex", "--name", "code", "--base-url", "https://gw.example.com/anthropic", "--model", "default=gateway:0"}
	for _, alias := range []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "gpt-6-luna"} {
		args = append(args, "--model", alias+"=claude-gateway:100000")
	}
	_, stderr, code := runCLI(t, fakeEnv(fc), args...)
	if code != uzicli.ExitOK {
		t.Fatalf("code %d: %s", code, stderr)
	}
	r := fc.LastProviderCreate
	if r == nil || r.Engine != "codex" || r.Protocol != "openai_responses" || r.AuthStyle != "bearer" || r.ModelMap.Default.Model != "gateway:0" || r.ModelMap.Default.ContextWindow != nil || r.ModelMap.Haiku.Model != "" {
		t.Fatalf("request: %+v", r)
	}
	if strings.Contains(stderr, "warning") {
		t.Fatal(stderr)
	}
	fc.ProviderList = enabledList(apitypes.ProviderDTO{Name: "code", Engine: "codex", ModelMap: r.ModelMap})
	out, _, code := runCLI(t, fakeEnv(fc), "provider", "list")
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"ENGINE", "codex", "default=gateway:0", "gpt-6-astra=claude-gateway:100000", "gpt-5.6-sol=", "gpt-6-sol=", "gpt-6.1-sol=", "gpt-6.2-sol=claude-gateway:100000", "gpt-6.2-astra=claude-gateway:100000", "gpt-6-luna="} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s: %s", want, out)
		}
	}
}

func TestModelsCellCodexSanitizesEveryEntry(t *testing.T) {
	entry := &apitypes.ModelEntryDTO{Model: "unsafe\nmodel"}
	mm := apitypes.ModelMapDTO{Default: entry, GPT6Astra: entry, GPT56Sol: entry, GPT6Sol: entry, GPT61Sol: entry, GPT62Sol: entry, GPT62Astra: entry, GPT6Luna: entry}
	got := modelsCell(mm)
	if strings.Contains(got, "\n") || strings.Count(got, "unsafe") != 8 {
		t.Fatalf("unsafe or missing cells: %q", got)
	}
}

func TestProviderCodexRejectsInvalidProfileBeforeWrite(t *testing.T) {
	for _, extra := range [][]string{
		{"--model", "default=x", "--auth-style", "x_api_key"},
		{"--model", "haiku=x"},
		{"--model", "default=x", "--model", "sonnet=y"},
		{"--model", "default=x:8191"},
		{"--model", "default=bad model"},
		{"--model", "default=x", "--engine", "unknown"},
	} {
		fc := &uzicli.FakeClient{}
		args := []string{"provider", "create", "--engine", "codex", "--name", "code", "--base-url", "https://gw.example.com/anthropic"}
		_, stderr, code := runCLI(t, fakeEnv(fc), append(args, extra...)...)
		if code != uzicli.ExitUsage || fc.LastProviderCreate != nil {
			t.Fatalf("%v: %d %s", extra, code, stderr)
		}
	}
}

func TestProviderCodexEditUsesStoredEngine(t *testing.T) {
	for _, models := range [][]string{nil, {"--model", "default=new:100000"}, {"--model", "haiku=wrong"}} {
		fc := &uzicli.FakeClient{ProviderList: enabledList(codexProviderFixture()), ProviderResult: codexProviderFixture()}
		args := append([]string{"provider", "edit", "code", "--name", "renamed"}, models...)
		_, stderr, code := runCLI(t, fakeEnv(fc), args...)
		if len(models) > 0 && models[1] == "haiku=wrong" {
			if code != uzicli.ExitUsage || fc.LastProviderPatch != nil {
				t.Fatalf("wrong map: %d %s", code, stderr)
			}
			continue
		}
		if code != 0 || fc.LastProviderPatch == nil {
			t.Fatalf("edit: %d %s", code, stderr)
		}
		if len(models) == 0 && fc.LastProviderPatch.ModelMap != nil {
			t.Fatal("rename changed map")
		}
		if len(models) > 0 && (fc.LastProviderPatch.ModelMap.Default.Model != "new" || *fc.LastProviderPatch.ModelMap.Default.ContextWindow != 100000) {
			t.Fatal("wrong replacement")
		}
	}
}

// A list call is client contact too: these guards must run before resolving a profile.
type providerNoContact struct {
	*uzicli.FakeClient
	t *testing.T
}

func (f *providerNoContact) ListProviders(context.Context) (apitypes.ProviderListDTO, error) {
	f.t.Fatal("unexpected client contact")
	return apitypes.ProviderListDTO{}, nil
}

func TestProviderUseRequiresExplicitValidClearEngine(t *testing.T) {
	for _, flags := range [][]string{{"--none"}, {"--none", "--engine", ""}, {"--none", "--engine", "invalid"}, {"code", "--engine", "invalid"}} {
		fc := &providerNoContact{FakeClient: &uzicli.FakeClient{}, t: t}
		_, stderr, code := runCLI(t, fakeEnv(fc), append([]string{"provider", "use"}, flags...)...)
		if code != uzicli.ExitUsage || fc.ProviderSourceCalls != 0 {
			t.Fatalf("%v: %d %s", flags, code, stderr)
		}
	}
}

func TestProviderUseRoutesOnlySelectedEngine(t *testing.T) {
	for _, engine := range []string{"claude", "codex"} {
		d := codexProviderFixture()
		if engine == "claude" {
			d = providerFixture("code")
			d.HasKey, d.KeyEnabled = true, true
		}
		fc := &uzicli.FakeClient{ProviderList: enabledList(d)}
		out, stderr, code := runCLI(t, fakeEnv(fc), "--json", "provider", "use", "code")
		if code != 0 || fc.ProviderSourceCalls != 1 || fc.LastProviderSourceEngine != engine || fc.LastProviderSource == nil || *fc.LastProviderSource != d.ID || !strings.Contains(out, engine+"_provider_id") {
			t.Fatalf("select %s: %d %s %s %+v", engine, code, out, stderr, fc)
		}
		_, _, code = runCLI(t, fakeEnv(fc), "provider", "use", "--none", "--engine", engine)
		if code != 0 || fc.ProviderSourceCalls != 2 || fc.LastProviderSourceEngine != engine || fc.LastProviderSource != nil {
			t.Fatal("clear routed incorrectly")
		}
		other := "claude"
		if engine == "claude" {
			other = "codex"
		}
		_, _, code = runCLI(t, fakeEnv(fc), "provider", "use", "code", "--engine", other)
		if code != uzicli.ExitUsage || fc.ProviderSourceCalls != 2 {
			t.Fatal("mismatched engine wrote source")
		}
	}
	for _, engine := range []string{"", "unknown"} {
		d := codexProviderFixture()
		d.Engine = engine
		fc := &uzicli.FakeClient{ProviderList: enabledList(d)}
		_, _, code := runCLI(t, fakeEnv(fc), "provider", "use", "code")
		if code != uzicli.ExitUsage || fc.ProviderSourceCalls != 0 {
			t.Fatal("invalid stored engine wrote source")
		}
	}
}

func TestProviderCodexNewSlotsEditRoundtrip(t *testing.T) {
	for _, slot := range []string{"gpt-6.2-sol", "gpt-6.2-astra"} {
		t.Run(slot, func(t *testing.T) {
			fc := &uzicli.FakeClient{ProviderList: enabledList(codexProviderFixture()), ProviderResult: codexProviderFixture()}
			_, stderr, code := runCLI(t, fakeEnv(fc), "provider", "edit", "code", "--model", "default=base", "--model", slot+"=edited:100000")
			if code != 0 || fc.LastProviderPatch == nil {
				t.Fatalf("edit: %d %s", code, stderr)
			}
			got := modelsCell(*fc.LastProviderPatch.ModelMap)
			if !strings.Contains(got, slot+"=edited:100000") {
				t.Fatal(got)
			}
			_, _, code = runCLI(t, fakeEnv(fc), "provider", "edit", "code", "--model", "default=base", "--model", slot+"=edited:8191")
			if code != uzicli.ExitUsage {
				t.Fatal("accepted invalid context")
			}
		})
	}
}
