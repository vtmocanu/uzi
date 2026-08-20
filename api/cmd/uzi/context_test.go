package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// listCtx is a fresh two-context store: a "default" context with a URL and a
// uzc_ token, and a URL-less "admin" context that has only a uza_ token (so its
// URL cell must be blank — it inherits default's URL per D3).
func listCtx(t *testing.T) Env {
	t.Helper()
	cfg := &uzicli.Config{
		Current: "default",
		Contexts: map[string]uzicli.Context{
			"default": {URL: "https://default.example"},
			// admin has NO URL of its own.
		},
	}
	creds := &uzicli.Credentials{
		Contexts: map[string]uzicli.Credential{
			"default": {Token: "uzc_defaultsecret"},
			"admin":   {Token: "uza_adminsecret"},
		},
	}
	return seedStore(t, cfg, creds)
}

// `context list` shows the union of config + credentials contexts, marks the
// current one, leaves a URL-less context's URL cell blank, and never leaks a
// token value.
func TestContextList(t *testing.T) {
	env := listCtx(t)
	out, _, code := runCLI(t, env, "context", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "default") {
		t.Errorf("list missing default context:\n%s", out)
	}
	// admin exists only in credentials — it must still appear (union).
	if !strings.Contains(out, "admin") {
		t.Errorf("list missing token-only admin context:\n%s", out)
	}
	if strings.Contains(out, "uzc_defaultsecret") || strings.Contains(out, "uza_adminsecret") {
		t.Errorf("list leaked a token value:\n%s", out)
	}
	// The current marker sits on the default row.
	defaultLine := ""
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "default") {
			defaultLine = ln
		}
	}
	if defaultLine == "" {
		t.Fatalf("no default row in:\n%s", out)
	}
	if !strings.Contains(defaultLine, "*") {
		t.Errorf("current marker not on default row: %q", defaultLine)
	}
	// admin's URL cell is blank (it does not print default's URL).
	for _, ln := range strings.Split(out, "\n") {
		if strings.HasPrefix(ln, "admin") && strings.Contains(ln, "https://default.example") {
			t.Errorf("admin row leaked the inherited default URL: %q", ln)
		}
	}
}

// `context list --json` emits the documented per-context shape, current bool
// and token prefix only (never the value).
func TestContextListJSON(t *testing.T) {
	env := listCtx(t)
	out, _, code := runCLI(t, env, "context", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var rows []struct {
		Name        string `json:"name"`
		URL         string `json:"url"`
		HasToken    bool   `json:"has_token"`
		TokenPrefix string `json:"token_prefix"`
		Current     bool   `json:"current"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	byName := map[string]int{}
	for i, r := range rows {
		byName[r.Name] = i
	}
	di, ok := byName["default"]
	if !ok {
		t.Fatalf("no default row:\n%s", out)
	}
	if !rows[di].Current {
		t.Errorf("default row current=false, want true")
	}
	if rows[di].URL != "https://default.example" {
		t.Errorf("default url = %q", rows[di].URL)
	}
	ai, ok := byName["admin"]
	if !ok {
		t.Fatalf("no admin row:\n%s", out)
	}
	if rows[ai].URL != "" {
		t.Errorf("admin url = %q, want blank (inherits default)", rows[ai].URL)
	}
	if !rows[ai].HasToken || rows[ai].Current {
		t.Errorf("admin row = %+v, want has_token=true current=false", rows[ai])
	}
	// The prefix is a short recognisable fragment, never the whole token.
	if strings.Contains(out, "uza_adminsecret") {
		t.Errorf("json leaked the full admin token:\n%s", out)
	}
	if rows[ai].TokenPrefix == "" || strings.HasPrefix("uza_adminsecret", rows[ai].TokenPrefix) == false {
		t.Errorf("admin token_prefix = %q, want a prefix of the token", rows[ai].TokenPrefix)
	}
}

// `context current` prints the sticky Current when set.
func TestContextCurrentSet(t *testing.T) {
	cfg := &uzicli.Config{
		Current:  "admin",
		Contexts: map[string]uzicli.Context{"default": {URL: "https://d.example"}, "admin": {}},
	}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"admin": {Token: "uza_x"}}}
	env := seedStore(t, cfg, creds)
	out, _, code := runCLI(t, env, "context", "current")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "admin") {
		t.Errorf("current did not print sticky admin:\n%s", out)
	}
}

// `context current` prints "default" when Current is unset, and honours --json.
func TestContextCurrentUnsetJSON(t *testing.T) {
	env := storeEnv(t, "")
	out, _, code := runCLI(t, env, "context", "current", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got struct {
		Current string `json:"current"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if got.Current != "default" {
		t.Errorf("current = %q, want default", got.Current)
	}
}

// `context use <name>` sets the sticky Current when the context exists.
func TestContextUseSetsCurrent(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: "https://d.example"}}}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{
		"default": {Token: "uzc_d"},
		"admin":   {Token: "uza_a"},
	}}
	env := seedStore(t, cfg, creds)
	_, _, code := runCLI(t, env, "context", "use", "admin")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	got, err := env.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != "admin" {
		t.Errorf("Current = %q, want admin", got.Current)
	}
}

// `context use default` succeeds even when no default entry is materialized:
// "default" is the implicit always-resolving context (the PRD's opening example
// runs `context use default` before the first `auth token`).
func TestContextUseDefaultWithoutEntry(t *testing.T) {
	cfg := &uzicli.Config{
		Current:  "admin",
		Contexts: map[string]uzicli.Context{"admin": {URL: "https://a.example"}},
	}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"admin": {Token: "uza_a"}}}
	env := seedStore(t, cfg, creds)
	_, _, code := runCLI(t, env, "context", "use", "default")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (default is always valid)", code)
	}
	got, err := env.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != "default" {
		t.Errorf("Current = %q, want default", got.Current)
	}
}

// `context use` on an unknown context is a usage error that names how to create
// the context.
func TestContextUseUnknown(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: "https://d.example"}}}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"default": {Token: "uzc_d"}}}
	env := seedStore(t, cfg, creds)
	_, errOut, code := runCLI(t, env, "context", "use", "ghost")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errOut, "unknown context") {
		t.Errorf("error missing 'unknown context':\n%s", errOut)
	}
	if !strings.Contains(errOut, "auth token") && !strings.Contains(errOut, "login") {
		t.Errorf("error does not name how to create the context:\n%s", errOut)
	}
	// Current must be unchanged.
	got, err := env.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != "" {
		t.Errorf("Current = %q, want unchanged/empty", got.Current)
	}
}

// `context rm <name>` removes the context from both files.
func TestContextRmRemovesBoth(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{
		"default": {URL: "https://d.example"},
		"admin":   {URL: "https://a.example"},
	}}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{
		"default": {Token: "uzc_d"},
		"admin":   {Token: "uza_a"},
	}}
	env := seedStore(t, cfg, creds)
	_, _, code := runCLI(t, env, "context", "rm", "admin")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	gotCfg, err := env.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gotCfg.Contexts["admin"]; ok {
		t.Errorf("admin still in config after rm")
	}
	gotCreds, err := env.Store.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gotCreds.Contexts["admin"]; ok {
		t.Errorf("admin still in credentials after rm")
	}
}

// `context rm` of the current context resets Current to "default".
func TestContextRmResetsCurrent(t *testing.T) {
	cfg := &uzicli.Config{
		Current: "admin",
		Contexts: map[string]uzicli.Context{
			"default": {URL: "https://d.example"},
			"admin":   {URL: "https://a.example"},
		},
	}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"admin": {Token: "uza_a"}}}
	env := seedStore(t, cfg, creds)
	_, _, code := runCLI(t, env, "context", "rm", "admin")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	got, err := env.Store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.Current != "default" {
		t.Errorf("Current = %q, want default after removing the current context", got.Current)
	}
}

// `context rm` of a name in neither file succeeds and reports "no such context".
func TestContextRmUnknown(t *testing.T) {
	cfg := &uzicli.Config{Contexts: map[string]uzicli.Context{"default": {URL: "https://d.example"}}}
	creds := &uzicli.Credentials{Contexts: map[string]uzicli.Credential{"default": {Token: "uzc_d"}}}
	env := seedStore(t, cfg, creds)
	out, _, code := runCLI(t, env, "context", "rm", "ghost")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (idempotent)", code)
	}
	if !strings.Contains(strings.ToLower(out), "no such context") {
		t.Errorf("rm of unknown context did not report it clearly:\n%s", out)
	}
}

// The `context` verb group is wired into the root command tree.
func TestContextCommandWired(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))
	ctx := findCmd(root, "context")
	if ctx == nil {
		t.Fatal("context command not wired into newRootCmd")
	}
	for _, sub := range []string{"list", "current", "use", "rm"} {
		if findCmd(ctx, sub) == nil {
			t.Errorf("context missing subcommand %q", sub)
		}
	}
}
