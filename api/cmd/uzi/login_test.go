package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// stubLoginHooks replaces the browser-open and inter-poll-wait package vars so a
// login test never spawns a real browser and never sleeps, and returns a restore
// func. openedURL captures what the login flow tried to open.
func stubLoginHooks(t *testing.T, open func(string) error) {
	t.Helper()
	oldOpen, oldWait := openBrowser, authWait
	openBrowser = open
	// Fire immediately: the poll loop's inter-poll wait is a no-op under test.
	authWait = func(time.Duration) <-chan time.Time {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	t.Cleanup(func() { openBrowser, authWait = oldOpen, oldWait })
}

// loginEnv builds a login-capable Env: a real (temp) config store and the LIVE
// HTTP client, so login exercises the real start/poll requests against httptest.
func loginEnv(t *testing.T) (Env, *uzicli.Store, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	store := uzicli.NewStore(t.TempDir())
	var out, errb bytes.Buffer
	env := Env{
		Stdout:    &out,
		Stderr:    &errb,
		Stdin:     strings.NewReader(""),
		StdoutTTY: false,
		NewClient: func(s uzicli.Settings) uzicli.Client { return uzicli.NewHTTPClient(s) },
		Store:     store,
	}
	return env, store, &out, &errb
}

// The happy path: start → one pending poll → 200 {token,user}. The token is stored
// 0600 (never printed), the resolved URL is persisted, and the user_code + consent
// URL are shown (the URL always, so a headless box can complete the login).
func TestLoginFlow(t *testing.T) {
	var openedURL string
	stubLoginHooks(t, func(u string) error { openedURL = u; return nil })

	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/cli/start":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"request_id":"req-1","user_code":"WXYZ-7788","expires_in":300,"interval":5}`)
		case "/api/auth/cli/poll":
			if atomic.AddInt32(&polls, 1) == 1 {
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, `{"status":"pending"}`)
				return
			}
			_, _ = io.WriteString(w, `{"token":"uzc_minted_secret","user":{"id":"u1","email":"vlad@x.io"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	env, store, out, errb := loginEnv(t)
	t.Setenv("UZI_URL", srv.URL)
	t.Setenv("UZI_TOKEN", "")

	code := Main(env, []string{"login"})
	if code != uzicli.ExitOK {
		t.Fatalf("login exit = %d (stderr: %s)", code, errb.String())
	}

	// The token is stored, and NEVER printed on either stream.
	if strings.Contains(out.String(), "uzc_minted_secret") || strings.Contains(errb.String(), "uzc_minted_secret") {
		t.Fatal("login leaked the token to stdout/stderr")
	}
	got, err := store.Resolve("default")
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "uzc_minted_secret" {
		t.Fatalf("stored token = %q, want the minted token", got.Token)
	}
	if got.URL != srv.URL {
		t.Fatalf("stored url = %q, want %q", got.URL, srv.URL)
	}
	// The user_code and consent URL are the interactive contract; both must show, and
	// the URL must ALSO be printed (not only handed to the browser) for headless use.
	if !strings.Contains(errb.String(), "WXYZ-7788") {
		t.Errorf("user_code not shown:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), "/cli-auth?request=req-1") {
		t.Errorf("consent URL not printed (headless fallback):\n%s", errb.String())
	}
	if !strings.Contains(openedURL, "/cli-auth?request=req-1") {
		t.Errorf("browser opened %q, want the consent URL", openedURL)
	}
	if !strings.Contains(out.String(), "vlad@x.io") {
		t.Errorf("success line missing the identity:\n%s", out.String())
	}
}

// A hostile server returns a megabyte-long user_code and email. Both are server-supplied
// and printed outside the Printer, so they must go through the LOCAL cellText (200-char
// cap), not the unbounded uzicli.CellText — otherwise the giant strings flood the terminal
// (#220). The sharp assertion is that a 1000-rune run of A's never appears in full.
func TestLoginBoundsServerStrings(t *testing.T) {
	stubLoginHooks(t, func(string) error { return nil })

	giant := strings.Repeat("A", 1<<20)
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/cli/start":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"request_id":"req-1","user_code":"`+giant+`","expires_in":300,"interval":5}`)
		case "/api/auth/cli/poll":
			if atomic.AddInt32(&polls, 1) == 1 {
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, `{"status":"pending"}`)
				return
			}
			_, _ = io.WriteString(w, `{"token":"uzc_x","user":{"id":"u1","email":"`+giant+`@x.io"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	env, _, out, errb := loginEnv(t)
	t.Setenv("UZI_URL", srv.URL)
	t.Setenv("UZI_TOKEN", "")

	code := Main(env, []string{"login"})
	if code != uzicli.ExitOK {
		t.Fatalf("login exit = %d (stderr: %s)", code, errb.String())
	}

	// stderr carries the user_code line (plus the consent URL and instructions), so bound
	// it generously — the point is that the megabyte code did NOT flood it.
	if n := len(errb.String()); n > 8192 {
		t.Errorf("stderr is %d bytes; the server user_code reached the terminal unbounded", n)
	}
	if strings.Contains(errb.String(), strings.Repeat("A", 1000)) {
		t.Errorf("the giant user_code reached the terminal in full:\n%s", errb.String())
	}
	// Non-vacuous: the code line still rendered.
	if !strings.Contains(errb.String(), "and enter this one-time code") {
		t.Errorf("user_code line not shown:\n%s", errb.String())
	}

	// stdout carries the "Logged in as ..." line; the giant email must be bounded there too.
	if strings.Contains(out.String(), strings.Repeat("A", 1000)) {
		t.Errorf("the giant email reached stdout in full:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Logged in as") {
		t.Errorf("success line missing:\n%s", out.String())
	}
}

// Headless / no-browser: the opener fails, but login still completes because the URL
// is printed for the human to open manually. The token still lands.
func TestLoginHeadlessBrowserFailure(t *testing.T) {
	stubLoginHooks(t, func(string) error { return io.ErrUnexpectedEOF }) // opener unavailable

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/cli/start":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"request_id":"req-2","user_code":"AAAA-BBBB","expires_in":300,"interval":5}`)
		case "/api/auth/cli/poll":
			_, _ = io.WriteString(w, `{"token":"uzc_headless","user":{"id":"u1","email":"h@x.io"}}`)
		}
	}))
	defer srv.Close()

	env, store, _, errb := loginEnv(t)
	t.Setenv("UZI_URL", srv.URL)
	t.Setenv("UZI_TOKEN", "")

	if code := Main(env, []string{"login"}); code != uzicli.ExitOK {
		t.Fatalf("headless login exit = %d (stderr: %s)", code, errb.String())
	}
	if got, _ := store.Resolve("default"); got.Token != "uzc_headless" {
		t.Fatalf("headless login did not store the token (got %q)", got.Token)
	}
	if !strings.Contains(errb.String(), "/cli-auth?request=req-2") {
		t.Errorf("consent URL must print even when the browser cannot open:\n%s", errb.String())
	}
}

// A terminal poll status (410 denied/expired/consumed) stops with a clear message
// and a non-zero exit, and stores nothing.
func TestLoginDeniedStops(t *testing.T) {
	stubLoginHooks(t, func(string) error { return nil })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/cli/start":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"request_id":"req-3","user_code":"CCCC-DDDD","expires_in":300,"interval":5}`)
		case "/api/auth/cli/poll":
			w.WriteHeader(http.StatusGone)
			_, _ = io.WriteString(w, `{"status":"denied"}`)
		}
	}))
	defer srv.Close()

	env, store, _, errb := loginEnv(t)
	t.Setenv("UZI_URL", srv.URL)
	t.Setenv("UZI_TOKEN", "")

	code := Main(env, []string{"login"})
	if code == uzicli.ExitOK {
		t.Fatal("a denied login must exit non-zero")
	}
	if !strings.Contains(errb.String(), "denied") {
		t.Errorf("denied login message = %q, want it to say denied", errb.String())
	}
	if got, _ := store.Resolve("default"); got.Token != "" {
		t.Fatalf("a denied login must store no token (got %q)", got.Token)
	}
}

// login with no configured URL is a usage error (there is no server to reach), not a
// crash — it must not attempt a request with an empty base URL.
func TestLoginNoURL(t *testing.T) {
	stubLoginHooks(t, func(string) error { return nil })
	env, _, _, _ := loginEnv(t)
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	if code := Main(env, []string{"login"}); code != uzicli.ExitUsage {
		t.Fatalf("login with no URL exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
}

// D4: `login --context admin` stores the minted token AND the resolved URL under
// a BRAND-NEW `admin` context — it is CREATED, not rejected by D9 (login resolves
// its URL without the existence gate). The default context is untouched.
func TestLoginNamedContextCreated(t *testing.T) {
	stubLoginHooks(t, func(string) error { return nil })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/cli/start":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"request_id":"req-9","user_code":"EEEE-FFFF","expires_in":300,"interval":5}`)
		case "/api/auth/cli/poll":
			_, _ = io.WriteString(w, `{"token":"uza_named_secret","user":{"id":"u2","email":"admin@x.io"}}`)
		}
	}))
	defer srv.Close()

	env, store, out, errb := loginEnv(t)
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	// A store with no `admin` context at all — a read command under -c admin would
	// D9-error, but login must CREATE it.
	code := Main(env, []string{"login", "-c", "admin", "--url", srv.URL})
	if code != uzicli.ExitOK {
		t.Fatalf("named login exit = %d (stderr: %s)", code, errb.String())
	}
	got, err := store.Resolve("admin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "uza_named_secret" {
		t.Fatalf("admin token = %q, want the minted token", got.Token)
	}
	if got.URL != srv.URL {
		t.Fatalf("admin url = %q, want %q", got.URL, srv.URL)
	}
	// The default context was not written.
	if d, _ := store.Resolve("default"); d.Token != "" || d.URL != "" {
		t.Errorf("default context was written by a named login: %+v", d)
	}
	if !strings.Contains(out.String(), "admin@x.io") {
		t.Errorf("success line missing identity:\n%s", out.String())
	}
}

// D9 for READ commands is unaffected: an explicit unknown context is still a usage
// error before any client is built (login is the write-path exception, not this).
func TestReadCommandUnknownContextStillD9(t *testing.T) {
	env := storeEnv(t, "")
	_, errOut, code := runCLI(t, env, "auth", "status", "-c", "ghost")
	if code != uzicli.ExitUsage {
		t.Fatalf("read under unknown context exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errOut, "unknown context") {
		t.Errorf("error missing 'unknown context':\n%s", errOut)
	}
}

// logout removes the stored credential (local-only; it never calls the server).
func TestLogoutRemovesCredential(t *testing.T) {
	store := uzicli.NewStore(t.TempDir())
	if err := store.SaveCredentials(&uzicli.Credentials{Contexts: map[string]uzicli.Credential{
		"default": {Token: "uzc_stored"},
	}}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Store: store,
		NewClient: func(uzicli.Settings) uzicli.Client { return &uzicli.FakeClient{} }}
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	if code := Main(env, []string{"logout"}); code != uzicli.ExitOK {
		t.Fatalf("logout exit = %d", code)
	}
	if got, _ := store.Resolve("default"); got.Token != "" {
		t.Fatalf("logout left a token behind: %q", got.Token)
	}
}

// D4/D8: `logout -c admin` clears ONLY admin's token. Admin's config entry (its
// URL) is left intact so a re-login works, and the default context is untouched.
func TestLogoutNamedContextD8(t *testing.T) {
	store := uzicli.NewStore(t.TempDir())
	if err := store.SaveConfig(&uzicli.Config{Contexts: map[string]uzicli.Context{
		"default": {URL: "https://default.example"},
		"admin":   {URL: "https://admin.example"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCredentials(&uzicli.Credentials{Contexts: map[string]uzicli.Credential{
		"default": {Token: "uzc_default"},
		"admin":   {Token: "uza_admin"},
	}}); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	env := Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Store: store,
		NewClient: func(uzicli.Settings) uzicli.Client { return &uzicli.FakeClient{} }}
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	if code := Main(env, []string{"logout", "-c", "admin"}); code != uzicli.ExitOK {
		t.Fatalf("logout -c admin exit = %d (stderr: %s)", code, errb.String())
	}

	// admin's token is gone...
	if got, _ := store.Resolve("admin"); got.Token != "" {
		t.Errorf("admin token survived logout: %q", got.Token)
	}
	// ...but admin's config URL remains (re-login-ready).
	cfg, err := store.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Contexts["admin"].URL != "https://admin.example" {
		t.Errorf("admin config URL = %q, want it preserved (D8)", cfg.Contexts["admin"].URL)
	}
	// The default context is untouched.
	if got, _ := store.Resolve("default"); got.Token != "uzc_default" {
		t.Errorf("default token = %q, want it untouched", got.Token)
	}
}
