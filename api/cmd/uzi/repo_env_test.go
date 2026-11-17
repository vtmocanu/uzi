package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
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
			{Name: "EVIL", Value: &evil, State: "enabled", UpdatedAt: ts},
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
		"TOKEN", "secret", "(secret)", "disabled", "(vault locked)", "2026-03-04"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("output carries a raw escape byte: %q", out)
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
