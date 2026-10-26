package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const vaultTestSecret = "vault-input-sentinel-keep-private"

func TestVaultUnlockInput(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"LF", "  pass phrase  \nignored", "  pass phrase  "},
		{"CRLF", "  pass phrase  \r\n", "  pass phrase  "},
		{"EOF", "  pass phrase  ", "  pass phrase  "},
		{"standalone CR", "pass\r", "pass\r"},
		{"spaces", "   \n", "   "},
		{"limit", strings.Repeat("x", 1<<20) + "\r\n", strings.Repeat("x", 1<<20)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &uzicli.FakeClient{}
			env := fakeEnv(f)
			env.Stdin = strings.NewReader(tc.input)
			out, stderr, code := runCLI(t, env, "--url", "https://example.com", "vault", "unlock", "--password-stdin")
			if code != 0 || out != "Vault unlocked.\n" || stderr != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
			}
			if f.VaultUnlockCalls != 1 || len(f.VaultUnlockPasswords) != 1 || f.VaultUnlockPasswords[0] != tc.want {
				t.Fatal("password was altered or attempt count differed")
			}
			if f.VaultStatusCalls != 0 {
				t.Fatal("unexpected status request")
			}
		})
	}
}

func TestVaultUnlockPrompt(t *testing.T) {
	f := &uzicli.FakeClient{}
	env := fakeEnv(f)
	env.StdinTTY = true
	reads := 0
	env.ReadVaultPassword = func(ctx context.Context, r io.Reader) ([]byte, error) {
		reads++
		if ctx.Err() != nil || r != env.Stdin {
			t.Fatal("wrong prompt inputs")
		}
		return []byte(vaultTestSecret), nil
	}
	out, stderr, code := runCLI(t, env, "--url", "https://example.com", "vault", "unlock")
	if code != 0 || out != "Vault unlocked.\n" || stderr != "Login password or OIDC vault passphrase: \n" || reads != 1 || f.VaultUnlockCalls != 1 || f.VaultUnlockPasswords[0] != vaultTestSecret {
		t.Fatalf("code=%d stdout=%q stderr=%q reads=%d calls=%d", code, out, stderr, reads, f.VaultUnlockCalls)
	}
}

type vaultInputReader func([]byte) (int, error)

func (r vaultInputReader) Read(p []byte) (int, error) { return r(p) }

func TestVaultUnlockRejectedInput(t *testing.T) {
	for _, name := range []string{"TTY flag", "nonTTY missing flag", "empty", "empty LF", "empty CRLF", "oversize", "TTY oversize", "read error", "prompt error", "cancel before", "cancel after", "cancel stdin"} {
		t.Run(name, func(t *testing.T) {
			f := &uzicli.FakeClient{}
			env := fakeEnv(f)
			env.Stdin = strings.NewReader(vaultTestSecret + "\n")
			args := []string{"--url", "https://example.com", "vault", "unlock", "--password-stdin"}
			wantCode := uzicli.ExitUsage
			reads := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			env.ReadVaultPassword = func(context.Context, io.Reader) ([]byte, error) { reads++; return []byte(vaultTestSecret), nil }
			switch name {
			case "TTY flag":
				env.StdinTTY = true
			case "nonTTY missing flag":
				args = args[:len(args)-1]
			case "empty":
				env.Stdin = strings.NewReader("")
			case "empty LF":
				env.Stdin = strings.NewReader("\n")
			case "empty CRLF":
				env.Stdin = strings.NewReader("\r\n")
			case "oversize":
				env.Stdin = strings.NewReader(strings.Repeat("x", (1<<20)+1))
			case "TTY oversize":
				env.StdinTTY = true
				args = args[:len(args)-1]
				env.ReadVaultPassword = func(context.Context, io.Reader) ([]byte, error) {
					reads++
					return make([]byte, maxVaultPasswordBytes+1), nil
				}
			case "read error":
				wantCode = uzicli.ExitGeneric
				env.Stdin = vaultInputReader(func(p []byte) (int, error) { return copy(p, vaultTestSecret), errors.New(vaultTestSecret) })
			case "prompt error":
				wantCode = uzicli.ExitGeneric
				env.StdinTTY = true
				args = args[:len(args)-1]
				env.ReadVaultPassword = func(context.Context, io.Reader) ([]byte, error) {
					return []byte(vaultTestSecret), errors.New(vaultTestSecret)
				}
			case "cancel before", "cancel after":
				wantCode = uzicli.ExitGeneric
				env.StdinTTY = true
				args = args[:len(args)-1]
				if name == "cancel before" {
					cancel()
				}
				env.ReadVaultPassword = func(context.Context, io.Reader) ([]byte, error) {
					reads++
					cancel()
					return []byte(vaultTestSecret), nil
				}
			case "cancel stdin":
				wantCode = uzicli.ExitGeneric
				env.Stdin = vaultInputReader(func(p []byte) (int, error) { cancel(); return copy(p, vaultTestSecret+"\n"), nil })
			}
			var out, stderr bytes.Buffer
			env.Stdout = &out
			env.Stderr = &stderr
			t.Setenv("UZI_URL", "")
			t.Setenv("UZI_TOKEN", "")
			// Main executes with Cobra's background context; inject the cancelled context
			// through the reader seam for cancellation during a command. Pre-cancelled
			// execution uses the same root command with an explicit context.
			var code int
			if strings.HasPrefix(name, "cancel") {
				root := newRootCmd(env)
				root.SetArgs(args)
				err := root.ExecuteContext(ctx)
				code = uzicli.ExitCodeFor(err)
				if err == nil {
					t.Fatal("cancelled command succeeded")
				}
			} else {
				code = Main(env, args)
			}
			if code != wantCode || f.VaultUnlockCalls != 0 || f.VaultStatusCalls != 0 || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%q stderr=%q calls=%d", code, out.String(), stderr.String(), f.VaultUnlockCalls)
			}
			if strings.Contains(stderr.String(), vaultTestSecret) {
				t.Fatal("input reflected")
			}
			if (name == "TTY flag" || name == "cancel before") && reads != 0 {
				t.Fatal("rejected command read password")
			}
		})
	}
}

func TestVaultStatusAndOutput(t *testing.T) {
	for _, tc := range []struct {
		name     string
		unlocked bool
		args     []string
		want     string
	}{
		{"locked", false, []string{"vault", "status"}, "locked\n"},
		{"unlocked", true, []string{"vault", "status"}, "unlocked\n"},
		{"JSON locked", false, []string{"--json", "vault", "status"}, "{\"unlocked\":false}\n"},
		{"JSON unlocked", true, []string{"--json", "vault", "status"}, "{\"unlocked\":true}\n"},
		{"quiet unlock", false, []string{"--quiet", "vault", "unlock", "--password-stdin"}, ""},
		{"JSON unlock", false, []string{"--json", "vault", "unlock", "--password-stdin"}, "{\"unlocked\":true}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &uzicli.FakeClient{VaultLocked: !tc.unlocked}
			env := fakeEnv(f)
			env.Stdin = strings.NewReader(vaultTestSecret)
			out, stderr, code := runCLI(t, env, append([]string{"--url", "https://example.com"}, tc.args...)...)
			if strings.HasPrefix(tc.name, "JSON") {
				var compact bytes.Buffer
				if err := json.Compact(&compact, []byte(out)); err != nil {
					t.Fatal(err)
				}
				out = compact.String() + "\n"
			}
			if code != 0 || out != tc.want || stderr != "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
			}
			wantStatus, wantUnlock := 1, 0
			if tc.name == "quiet unlock" || tc.name == "JSON unlock" {
				wantStatus, wantUnlock = 0, 1
			}
			if f.VaultStatusCalls != wantStatus || f.VaultUnlockCalls != wantUnlock {
				t.Fatalf("status=%d unlock=%d", f.VaultStatusCalls, f.VaultUnlockCalls)
			}
		})
	}
}

func TestVaultRejectedArgumentsDoNotReflectSecret(t *testing.T) {
	for _, args := range [][]string{
		{"vault", "unlock", vaultTestSecret},
		{"--password", vaultTestSecret, "vault", "unlock"},
		{"--password=" + vaultTestSecret, "vault", "unlock"},
		{"vault", "status", vaultTestSecret},
		{"vault", vaultTestSecret},
		{"vault", "unlock", "--password=" + vaultTestSecret},
		{"vault", "unlock", "--password-stdin=" + vaultTestSecret},
		{"--quiet", "vault", "unlock", "--password-stdin=" + vaultTestSecret},
		{"--json", "vault", vaultTestSecret},
	} {
		t.Run(strings.Join(args[:1], " "), func(t *testing.T) {
			f := &uzicli.FakeClient{}
			out, stderr, code := runCLI(t, fakeEnv(f), append([]string{"--url", "https://example.com"}, args...)...)
			if code != uzicli.ExitUsage || f.VaultUnlockCalls != 0 || f.VaultStatusCalls != 0 {
				t.Fatalf("code=%d calls=%d/%d", code, f.VaultUnlockCalls, f.VaultStatusCalls)
			}
			if strings.Contains(out+stderr, vaultTestSecret) {
				t.Fatal("rejected argument leaked into output")
			}
		})
	}
}

type vaultCommandTransport func(*http.Request) (*http.Response, error)

func (f vaultCommandTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVaultUnlockSafeErrors(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status, code int
		want         string
	}{
		{"401", 401, uzicli.ExitAuth, "authentication failed"},
		{"403 missing vault", 403, uzicli.ExitAuth, "incorrect password"},
		{"429", 429, uzicli.ExitUnreachable, "retry after 7s"},
		{"500", 500, uzicli.ExitUnreachable, "vault unlock failed"},
		{"transport", 0, uzicli.ExitUnreachable, "vault unlock failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Contains(body, []byte(vaultTestSecret)) {
					t.Error("missing stdin password")
				}
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "{\"error\":\""+vaultTestSecret+"\",\"reason\":\""+vaultTestSecret+"\"}")
			}))
			defer srv.Close()
			c := &uzicli.HTTPClient{BaseURL: srv.URL, HTTP: srv.Client()}
			if tc.status == 0 {
				c.HTTP = &http.Client{Transport: vaultCommandTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New(vaultTestSecret) })}
			}
			env := fakeEnv(c)
			env.Stdin = strings.NewReader(vaultTestSecret + "\n")
			var logs bytes.Buffer
			old := log.Writer()
			log.SetOutput(&logs)
			defer log.SetOutput(old)
			out, stderr, code := runCLI(t, env, "--url", srv.URL, "vault", "unlock", "--password-stdin")
			if code != tc.code || out != "" || !strings.Contains(stderr, tc.want) || calls != 1 {
				t.Fatalf("code=%d stdout=%q stderr=%q calls=%d", code, out, stderr, calls)
			}
			if strings.Contains(out+stderr+logs.String(), vaultTestSecret) {
				t.Fatal("password reflected in output or logs")
			}
		})
	}
}

func TestVaultStatusFailure(t *testing.T) {
	f := &uzicli.FakeClient{VaultStatusErr: uzicli.Exitf(uzicli.ExitAuth, "authentication required")}
	out, stderr, code := runCLI(t, fakeEnv(f), "--url", "https://example.com", "vault", "status")
	if code != uzicli.ExitAuth || out != "" || !strings.Contains(stderr, "authentication required") || f.VaultStatusCalls != 1 || f.VaultUnlockCalls != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
	}
}

// Keep the duration contract explicit at the command's second sanitization boundary.
func TestVaultUnlockDoubleSanitization(t *testing.T) {
	f := &uzicli.FakeClient{VaultUnlockErr: uzicli.SafeVaultUnlockError(&uzicli.ExitError{Code: uzicli.ExitUnreachable, Err: uzicli.ErrVaultRateLimited, RetryAfter: 7 * time.Second, Reason: vaultTestSecret})}
	env := fakeEnv(f)
	env.Stdin = strings.NewReader(vaultTestSecret)
	out, stderr, code := runCLI(t, env, "--url", "https://example.com", "vault", "unlock", "--password-stdin")
	if code != uzicli.ExitUnreachable || out != "" || !strings.Contains(stderr, "retry after 7s") || strings.Contains(stderr, vaultTestSecret) || f.VaultUnlockCalls != 1 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out, stderr)
	}
}
