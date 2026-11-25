package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #4324: `uzi admin provider list|get` (read-only) and the shared-definition rules of
// `uzi provider` (marker, ambiguity refusal, admin-managed refusal).

const (
	sharedTestID   = "33333333-3333-4333-8333-333333333333"
	sharedOtherID  = "44444444-4444-4444-8444-444444444444"
	hostileSegment = "evil\x1b[31mred\u202egnp\x07"
)

func perUserKey() *string { s := "per_user_key"; return &s }

func sharedAdminFixture(id, name string) apitypes.AdminSharedProviderDTO {
	return apitypes.AdminSharedProviderDTO{
		ID: id, Name: name, Engine: "claude", Protocol: "anthropic_messages", CredentialMode: *perUserKey(),
		BaseURL: "https://gw.example.com/anthropic", AuthStyle: "bearer", CurrentRevision: 3,
		SelectedUserCount: 7, AttachedKeyCount: 5,
		ModelMap: apitypes.ModelMapDTO{
			Haiku:  apitypes.ModelEntryDTO{Model: "corp-haiku", ContextWindow: ptrInt(200000)},
			Sonnet: &apitypes.ModelEntryDTO{Model: "corp-sonnet"},
		},
	}
}

func sharedProviderFixture(id, name string) apitypes.ProviderDTO {
	d := providerFixture(name)
	d.ID = id
	d.CredentialMode = perUserKey()
	return d
}

func TestAdminProviderListTable(t *testing.T) {
	retired := sharedAdminFixture(sharedOtherID, "old-gw")
	retired.Retired = true
	fc := &uzicli.FakeClient{SharedProviders: []apitypes.AdminSharedProviderDTO{sharedAdminFixture(sharedTestID, "Corp Shared"), retired}}
	out, errOut, code := runCLI(t, fakeEnv(fc), "admin", "provider", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr %q", code, errOut)
	}
	header := strings.SplitN(out, "\n", 2)[0]
	if got, want := strings.Fields(header), []string{"NAME", "ENGINE", "URL", "AUTH", "RETIRED", "REVISION", "SELECTED", "KEYS"}; !equalStringSlices(got, want) {
		t.Fatalf("header = %v, want %v", got, want)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Corp Shared") {
			f := strings.Fields(line)
			// NAME is two words: Corp Shared claude <url> bearer false 3 7 5
			if len(f) != 9 || f[2] != "claude" || f[5] != "false" || f[6] != "3" || f[7] != "7" || f[8] != "5" {
				t.Errorf("Corp Shared row = %q", line)
			}
		}
		if strings.HasPrefix(line, "old-gw") && !strings.Contains(line, "true") {
			t.Errorf("retired row = %q, want RETIRED true", line)
		}
	}
}

// A Codex-engine shared definition renders its engine next to a Claude one in both list and get.
func TestAdminProviderRendersCodexEngine(t *testing.T) {
	claude := sharedAdminFixture(sharedTestID, "claudegw")
	codex := sharedAdminFixture(sharedOtherID, "codexgw")
	codex.Engine, codex.Protocol, codex.AuthStyle = "codex", "openai_responses", "bearer"
	fc := &uzicli.FakeClient{SharedProviders: []apitypes.AdminSharedProviderDTO{claude, codex}}

	out, errOut, code := runCLI(t, fakeEnv(fc), "admin", "provider", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("list: exit = %d, stderr %q", code, errOut)
	}
	engines := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 2 {
			engines[f[0]] = f[1]
		}
	}
	if engines["claudegw"] != "claude" || engines["codexgw"] != "codex" {
		t.Errorf("list engines = %v, want claudegw=claude codexgw=codex:\n%s", engines, out)
	}

	for name, want := range map[string]string{"claudegw": "claude", "codexgw": "codex"} {
		out, errOut, code := runCLI(t, fakeEnv(fc), "admin", "provider", "get", name)
		if code != uzicli.ExitOK {
			t.Fatalf("get %s: exit = %d, stderr %q", name, code, errOut)
		}
		got := ""
		for _, line := range strings.Split(out, "\n") {
			if f := strings.Fields(line); len(f) == 2 && f[0] == "engine" {
				got = f[1]
			}
		}
		if got != want {
			t.Errorf("get %s: engine = %q, want %q:\n%s", name, got, want, out)
		}
	}
}

func TestAdminProviderListJSONAndEmpty(t *testing.T) {
	fc := &uzicli.FakeClient{SharedProviders: []apitypes.AdminSharedProviderDTO{sharedAdminFixture(sharedTestID, "corp")}}
	out, _, code := runCLI(t, fakeEnv(fc), "--json", "admin", "provider", "list")
	var got []apitypes.AdminSharedProviderDTO
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil || len(got) != 1 || got[0].SelectedUserCount != 7 || got[0].AttachedKeyCount != 5 {
		t.Fatalf("json list: code %d, out %q", code, out)
	}
	out, _, code = runCLI(t, fakeEnv(&uzicli.FakeClient{}), "--json", "admin", "provider", "list")
	if code != uzicli.ExitOK || strings.TrimSpace(out) != "[]" {
		t.Errorf("empty json list: code %d, out %q", code, out)
	}
}

func TestAdminProviderGetByIDAndName(t *testing.T) {
	fc := &uzicli.FakeClient{SharedProviders: []apitypes.AdminSharedProviderDTO{sharedAdminFixture(sharedTestID, "Corp Shared")}}
	wantGets := 0
	for _, arg := range []string{sharedTestID, "corp shared"} {
		out, errOut, code := runCLI(t, fakeEnv(fc), "admin", "provider", "get", arg)
		if code != uzicli.ExitOK {
			t.Fatalf("get %q: exit = %d, stderr %q", arg, code, errOut)
		}
		for _, want := range []string{sharedTestID, "Corp Shared", "https://gw.example.com/anthropic", "bearer", "selected_user_count",
			"attached_key_count", "haiku=corp-haiku:200000", "sonnet=corp-sonnet"} {
			if !strings.Contains(out, want) {
				t.Errorf("get %q missing %q:\n%s", arg, want, out)
			}
		}
		if fc.LastProviderID != sharedTestID {
			t.Errorf("get %q fetched id %q", arg, fc.LastProviderID)
		}
		wantGets++
		if fc.SharedProviderGets != wantGets {
			t.Errorf("get %q: SharedProviderGets = %d, want %d", arg, fc.SharedProviderGets, wantGets)
		}
	}
	out, _, code := runCLI(t, fakeEnv(fc), "--json", "admin", "provider", "get", sharedTestID)
	var got apitypes.AdminSharedProviderDTO
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil || got.ID != sharedTestID {
		t.Errorf("json get: code %d, out %q", code, out)
	}
}

func TestAdminProviderGetPersonalIDIsNotFound(t *testing.T) {
	fc := &uzicli.FakeClient{SharedProviders: []apitypes.AdminSharedProviderDTO{sharedAdminFixture(sharedTestID, "corp")}}
	_, errOut, code := runCLI(t, fakeEnv(fc), "admin", "provider", "get", providerTestID)
	if code != uzicli.ExitNotFound || !strings.Contains(errOut, "personal providers are not shown here") || !strings.Contains(errOut, "uzi admin provider list") {
		t.Errorf("code %d, stderr %q", code, errOut)
	}
	if fc.SharedProviderGets != 0 {
		t.Errorf("SharedProviderGets = %d, want 0", fc.SharedProviderGets)
	}
}

func TestAdminProviderRendersHostileStringsInert(t *testing.T) {
	d := sharedAdminFixture(sharedTestID, "pre"+hostileSegment+"post")
	d.BaseURL = "https://gw.example.com/" + hostileSegment
	d.ModelMap.Sonnet = &apitypes.ModelEntryDTO{Model: "m" + hostileSegment}
	fc := &uzicli.FakeClient{SharedProviders: []apitypes.AdminSharedProviderDTO{d}}
	for _, args := range [][]string{{"admin", "provider", "list"}, {"admin", "provider", "get", sharedTestID}} {
		out, errOut, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitOK {
			t.Fatalf("%v: exit %d, stderr %q", args, code, errOut)
		}
		for _, bad := range []string{"\x1b", "\u202e", "\x07"} {
			if strings.Contains(out, bad) {
				t.Errorf("%v output carries control %q:\n%q", args, bad, out)
			}
		}
		if !strings.Contains(out, "pre") || !strings.Contains(out, "post") {
			t.Errorf("%v output lost the name text:\n%s", args, out)
		}
	}
}

func TestProviderListMarksSharedAndSanitizes(t *testing.T) {
	own := providerFixture("mine")
	shared := sharedProviderFixture(sharedTestID, "team"+hostileSegment)
	fc := &uzicli.FakeClient{ProviderList: enabledList(own, shared)}
	out, errOut, code := runCLI(t, fakeEnv(fc), "provider", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "MANAGED BY") {
		t.Fatalf("no MANAGED BY column:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "team") && !strings.Contains(line, "admin") {
			t.Errorf("shared row not marked: %q", line)
		}
		if strings.HasPrefix(line, "mine") && (strings.Contains(line, "admin") || !strings.HasSuffix(strings.TrimSpace(line), "you")) {
			t.Errorf("owned row not marked you: %q", line)
		}
	}
	for _, bad := range []string{"\x1b", "\u202e", "\x07"} {
		if strings.Contains(out, bad) {
			t.Errorf("list carries control %q", bad)
		}
	}
}

// ambiguousList is an owned and a shared definition sharing one name, differing in case.
func ambiguousList() apitypes.ProviderListDTO {
	own := providerFixture("Corp")
	shared := sharedProviderFixture(sharedTestID, "corp")
	return enabledList(own, shared)
}

func TestProviderUseAndTestRefuseAmbiguousName(t *testing.T) {
	for _, args := range [][]string{{"provider", "use", "CORP"}, {"provider", "test", "corp"}} {
		fc := &uzicli.FakeClient{ProviderList: ambiguousList()}
		_, errOut, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitUsage {
			t.Errorf("%v: exit = %d, want %d (stderr %q)", args, code, uzicli.ExitUsage, errOut)
		}
		for _, want := range []string{providerTestID, sharedTestID, "pass an id"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("%v: stderr missing %q: %q", args, want, errOut)
			}
		}
		if fc.ProviderSourceCalls != 0 || fc.ProviderTestCalls != 0 {
			t.Errorf("%v sent a request: source %d, test %d", args, fc.ProviderSourceCalls, fc.ProviderTestCalls)
		}
	}
}

func TestProviderUseAndTestAcceptIDsAndSharedNames(t *testing.T) {
	// An id is exact even when the name is ambiguous.
	own := providerFixture("Corp")
	own.HasKey, own.KeyEnabled = true, true
	shared := sharedProviderFixture(sharedTestID, "corp")
	shared.HasKey, shared.KeyEnabled = true, true
	fc := &uzicli.FakeClient{ProviderList: enabledList(own, shared)}
	if _, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", sharedTestID); code != uzicli.ExitOK || fc.LastProviderSource == nil || *fc.LastProviderSource != sharedTestID {
		t.Fatalf("use by id: code %d, stderr %q, source %v", code, errOut, fc.LastProviderSource)
	}
	if _, errOut, code := runCLI(t, fakeEnv(fc), "provider", "test", sharedTestID); code != uzicli.ExitOK || fc.LastProviderID != sharedTestID {
		t.Fatalf("test by id: code %d, stderr %q, id %q", code, errOut, fc.LastProviderID)
	}
	// A name only a shared definition carries resolves to it.
	only := sharedProviderFixture(sharedOtherID, "team")
	only.HasKey, only.KeyEnabled = true, true
	fc = &uzicli.FakeClient{ProviderList: enabledList(own, only)}
	if _, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", "team"); code != uzicli.ExitOK || *fc.LastProviderSource != sharedOtherID {
		t.Fatalf("use shared by name: code %d, stderr %q", code, errOut)
	}
}

func TestProviderEditAndRetireRefuseSharedWithoutRequest(t *testing.T) {
	shared := sharedProviderFixture(sharedTestID, "team")
	for _, args := range [][]string{
		{"provider", "edit", "TEAM", "--name", "x"},
		{"provider", "edit", sharedTestID, "--model", "haiku=h"},
		{"provider", "retire", "team"},
		{"provider", "retire", sharedTestID},
	} {
		fc := &uzicli.FakeClient{ProviderList: enabledList(providerFixture("mine"), shared)}
		_, errOut, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitConflict || !strings.Contains(errOut, "managed by your admin") {
			t.Errorf("%v: code %d, stderr %q", args, code, errOut)
		}
		if fc.LastProviderPatch != nil || fc.ProviderRetireCalls != 0 {
			t.Errorf("%v sent a write request", args)
		}
	}
}

// Errors print through cellText's 200-rune cap, so a refusal must carry its fix step inside it
// even for a maximum-length (64) name and two full UUIDs.
func TestProviderRefusalsSurviveTheErrorCapWithLongName(t *testing.T) {
	long := strings.Repeat("n", 64)
	shared := sharedProviderFixture(sharedTestID, long)
	own := providerFixture(long)

	fc := &uzicli.FakeClient{ProviderList: enabledList(own, shared)}
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", long)
	if code != uzicli.ExitUsage || !strings.Contains(errOut, providerTestID) || !strings.Contains(errOut, sharedTestID) ||
		!strings.Contains(errOut, "(yours)") || !strings.Contains(errOut, "(admin)") {
		t.Errorf("ambiguity: code %d, stderr %q", code, errOut)
	}

	fc = &uzicli.FakeClient{ProviderList: enabledList(shared)}
	_, errOut, code = runCLI(t, fakeEnv(fc), "provider", "edit", long, "--name", "x")
	if code != uzicli.ExitConflict || !strings.Contains(errOut, "only an admin can change it") || !strings.Contains(errOut, "uzi provider use") {
		t.Errorf("edit refusal: code %d, stderr %q", code, errOut)
	}
}

// TestProviderRefusalsKeepEveryIDWithQuoteHeavyAndThreeWayNames: a 64-byte name of quotes (which a
// %q verb would double) and a three-way case-fold collision (own "s…", own "ſ…" which EqualFold
// matches but lower() keeps distinct, and a shared "s…") still print every id in full, untruncated.
func TestProviderRefusalsKeepEveryIDWithQuoteHeavyAndThreeWayNames(t *testing.T) {
	const thirdID = "22222222-2222-4222-8222-222222222222"
	quotes := strings.Repeat(`"`, 40) + strings.Repeat("n", 24)
	fc := &uzicli.FakeClient{ProviderList: enabledList(providerFixture(quotes), sharedProviderFixture(sharedTestID, quotes))}
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", quotes)
	if code != uzicli.ExitUsage || !strings.Contains(errOut, providerTestID+" (yours)") || !strings.Contains(errOut, sharedTestID+" (admin)") {
		t.Errorf("quote-heavy ambiguity: code %d, stderr %q", code, errOut)
	}
	fc = &uzicli.FakeClient{ProviderList: enabledList(sharedProviderFixture(sharedTestID, quotes))}
	_, errOut, code = runCLI(t, fakeEnv(fc), "provider", "edit", quotes, "--name", "x")
	if code != uzicli.ExitConflict || !strings.Contains(errOut, "only an admin can change it; you can select it with `uzi provider use`") {
		t.Errorf("quote-heavy edit refusal: code %d, stderr %q", code, errOut)
	}

	base := strings.Repeat("x", 60)
	ownS := providerFixture("s" + base)
	ownLongS := providerFixture("\u017f" + base)
	ownLongS.ID = thirdID
	fc = &uzicli.FakeClient{ProviderList: enabledList(ownS, ownLongS, sharedProviderFixture(sharedTestID, "s"+base))}
	_, errOut, code = runCLI(t, fakeEnv(fc), "provider", "use", "s"+base)
	if code != uzicli.ExitUsage {
		t.Fatalf("three-way: code %d, stderr %q", code, errOut)
	}
	for _, want := range []string{providerTestID + " (yours)", thirdID + " (yours)", sharedTestID + " (admin)", "3 providers are named"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("three-way stderr lacks %q: %q", want, errOut)
		}
	}
}

func TestProviderSharedRefusalRetiredOmitsUseAdvice(t *testing.T) {
	shared := sharedProviderFixture(sharedTestID, "team")
	shared.Retired = true
	fc := &uzicli.FakeClient{ProviderList: enabledList(shared)}
	_, errOut, code := runCLI(t, fakeEnv(fc), "provider", "retire", "team")
	if code != uzicli.ExitConflict || !strings.Contains(errOut, "only an admin can change it") {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
	if strings.Contains(errOut, "uzi provider use") {
		t.Errorf("retired refusal suggests use: %q", errOut)
	}
}

func TestProviderRefusalsWithHostileNameCarryNoControlBytes(t *testing.T) {
	name := "x" + hostileSegment
	shared := sharedProviderFixture(sharedTestID, name)
	cases := map[string]apitypes.ProviderListDTO{
		"edit": enabledList(shared),
		"use":  enabledList(providerFixture(name), shared),
	}
	for verb, list := range cases {
		args := []string{"provider", verb, name}
		if verb == "edit" {
			args = append(args, "--name", "y")
		}
		_, errOut, code := runCLI(t, fakeEnv(&uzicli.FakeClient{ProviderList: list}), args...)
		if code == uzicli.ExitOK || errOut == "" {
			t.Fatalf("%s: code %d, stderr %q", verb, code, errOut)
		}
		for _, bad := range []string{"\x1b", "\u202e", "\x07"} {
			if strings.Contains(errOut, bad) {
				t.Errorf("%s stderr carries control %q: %q", verb, bad, errOut)
			}
		}
	}
}

func TestProviderUseConfirmationNamesSharedSource(t *testing.T) {
	shared := sharedProviderFixture(sharedTestID, "team")
	shared.HasKey, shared.KeyEnabled = true, true
	own := providerFixture("mine")
	own.HasKey, own.KeyEnabled = true, true
	for name, want := range map[string]bool{"team": true, "mine": false} {
		fc := &uzicli.FakeClient{ProviderList: enabledList(own, shared)}
		out, errOut, code := runCLI(t, fakeEnv(fc), "provider", "use", name)
		if code != uzicli.ExitOK {
			t.Fatalf("%s: code %d, stderr %q", name, code, errOut)
		}
		if got := strings.Contains(out, "(shared by your admin)"); got != want {
			t.Errorf("%s: confirmation %q, shared suffix = %v, want %v", name, out, got, want)
		}
	}
}

func TestProviderEditAndRetireResolveOwnedWhenNameIsShared(t *testing.T) {
	own := providerFixture("Corp")
	fc := &uzicli.FakeClient{ProviderList: enabledList(own, sharedProviderFixture(sharedTestID, "corp")), ProviderResult: own}
	if _, errOut, code := runCLI(t, fakeEnv(fc), "provider", "retire", "corp"); code != uzicli.ExitOK || fc.LastProviderID != providerTestID {
		t.Fatalf("retire: code %d, stderr %q, id %q", code, errOut, fc.LastProviderID)
	}
	if _, errOut, code := runCLI(t, fakeEnv(fc), "provider", "edit", "corp", "--name", "Renamed"); code != uzicli.ExitOK || fc.LastProviderID != providerTestID {
		t.Fatalf("edit: code %d, stderr %q, id %q", code, errOut, fc.LastProviderID)
	}
}
