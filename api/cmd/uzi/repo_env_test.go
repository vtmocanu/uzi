package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/shellquote"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func repoEnvFake() *uzicli.FakeClient {
	plain := "https://api.example.test"
	evil := "x\x1b[31mred"
	ts := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	return &uzicli.FakeClient{RepoEnvResult: apitypes.RunEnvListDTO{
		Variables: []apitypes.RunEnvVarDTO{
			{Name: "API_URL", Description: "base url", Value: &plain, State: "enabled", UpdatedAt: ts},
			{Name: "TOKEN", Secret: true, State: "disabled", UpdatedAt: ts},
			{Name: "LOCKED", ValueWithheld: true, State: "enabled", UpdatedAt: ts},
			{Name: "EVIL", Value: &evil, Description: "line1\nFAKE_ROW\tplain", State: "enabled", UpdatedAt: ts},
		},
		Counts: apitypes.RunEnvCountsDTO{Total: 4, Limit: 50},
	}}
}

func TestRepoEnvListTable(t *testing.T) {
	fc := repoEnvFake()
	out, _, code := runCLI(t, fakeEnv(fc), "repo", "env", "list", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if fc.LastListRepoEnvRepoID != "r1" {
		t.Errorf("repo id = %q", fc.LastListRepoEnvRepoID)
	}
	for _, want := range []string{"NAME", "KIND", "STATE", "VALUE", "DESCRIPTION", "UPDATED",
		"API_URL", "plain", "https://api.example.test", "base url",
		"TOKEN", "secret", "(secret)", "disabled", "(withheld)", "2026-03-04"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("output carries a raw escape byte: %q", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAKE_ROW") {
			t.Errorf("a newline in a description forged a table row: %q", out)
		}
	}
}

func TestRepoEnvListJSON(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(repoEnvFake()), "repo", "env", "list", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.RunEnvListDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(got.Variables) != 4 || got.Variables[1].Value != nil || got.Counts.Limit != 50 {
		t.Errorf("decoded = %+v", got)
	}
}

func TestRepoEnvListRequiresRepo(t *testing.T) {
	_, _, code := runCLI(t, fakeEnv(repoEnvFake()), "repo", "env", "list")
	if code == uzicli.ExitOK {
		t.Fatal("want usage failure without a repo")
	}
}

func TestRepoEnvListVaultLockedNote(t *testing.T) {
	const note = "Your vault is locked or its state could not be checked: plain values are withheld. If it is locked, run:"
	locked := repoEnvFake()
	locked.RepoEnvResult.VaultLocked = true
	out, errOut, code := runCLI(t, fakeEnv(locked), "repo", "env", "list", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(errOut, note) || !strings.Contains(errOut, "uzi vault unlock") || strings.Contains(out, note) {
		t.Errorf("locked: stderr=%q stdout has note=%v", errOut, strings.Contains(out, note))
	}
	if _, errOut, _ := runCLI(t, fakeEnv(repoEnvFake()), "repo", "env", "list", "r1"); strings.Contains(errOut, note) {
		t.Errorf("unlocked list printed the note: %q", errOut)
	}
	jout, jerr, jcode := runCLI(t, fakeEnv(locked), "repo", "env", "list", "r1", "--json")
	if jcode != uzicli.ExitOK {
		t.Fatalf("--json exit = %d, want 0", jcode)
	}
	if strings.Contains(jerr, note) {
		t.Errorf("--json printed the note: %q", jerr)
	}
	var got apitypes.RunEnvListDTO
	if err := json.Unmarshal([]byte(jout), &got); err != nil {
		t.Fatalf("--json stdout is not the list DTO: %v\n%s", err, jout)
	}
	if !got.VaultLocked {
		t.Errorf("--json vault_locked = false, want true: %s", jout)
	}
}

func TestRepoEnvListVaultLockedNoteCarriesFlags(t *testing.T) {
	locked := repoEnvFake()
	locked.RepoEnvResult.VaultLocked = true
	_, errOut, code := runCLI(t, fakeEnv(locked), "repo", "env", "list", "r1",
		"--context", "my ctx", "--url", "https://uzi.example.com")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr=%q", code, errOut)
	}
	const want = "uzi vault unlock --context 'my ctx' --url https://uzi.example.com"
	// The command must be a whole line of its own: no sentence punctuation may follow it,
	// or a copied --url gains a trailing dot.
	found := false
	for _, line := range strings.Split(errOut, "\n") {
		if line == "  "+want {
			found = true
		}
	}
	if !found {
		t.Errorf("note has no line exactly %q: %q", "  "+want, errOut)
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":                    "''",
		"prod":                "prod",
		"https://u.example/x": "https://u.example/x",
		"my ctx":              "'my ctx'",
		"o'brien":             `'o'\''brien'`,
		"$HOME":               "'$HOME'",
		"=foo":                "'=foo'",
		"a=b":                 "a=b",
	} {
		if got := shellquote.Quote(in); got != want {
			t.Errorf("shellquote.Quote(%q) = %s; want %s", in, got, want)
		}
	}
}
