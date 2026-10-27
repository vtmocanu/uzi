package uzicli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPClientVaultStatus(t *testing.T) {
	for _, unlocked := range []bool{false, true} {
		t.Run(fmt.Sprint(unlocked), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.RequestURI() != "/api/vault/status" {
					t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "Bearer uzc_test" {
					t.Error("missing Bearer authorization")
				}
				_ = json.NewEncoder(w).Encode(VaultStatusResult{Unlocked: unlocked})
			}))
			defer srv.Close()
			got, err := newTestClient(srv).VaultStatus(context.Background())
			if err != nil || got.Unlocked != unlocked {
				t.Fatalf("status = %+v, %v", got, err)
			}
		})
	}
}

func TestHTTPClientVaultUnlock(t *testing.T) {
	const password = "private password with quotes \" and newline\n"
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		retry   string
		code    int
		message string
		wrong   bool
	}{
		{"204", 204, "", "", ExitOK, "", false},
		{"200", 200, "", "", ExitOK, "", false},
		{"401", 401, password, "", ExitAuth, "vault unlock authentication failed", false},
		{"403 wrong password", 403, "incorrect password: " + password, "", ExitAuth, "incorrect password", true},
		{"403 no vault", 403, "no vault: " + password, "", ExitAuth, "incorrect password", true},
		{"429", 429, password, "7", ExitUnreachable, "vault unlock rate limited; retry later (retry after 7s)", false},
		{"429 reflected retry", 429, password, password, ExitUnreachable, "vault unlock rate limited; retry later", false},
		{"500", 500, password, "", ExitUnreachable, "vault unlock failed", false},
		{"599", 599, password, "", ExitUnreachable, "vault unlock failed", false},
		{"unknown", 418, password, "", ExitGeneric, "vault unlock failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.RequestURI() != "/api/vault/unlock" {
					t.Errorf("request = %s %s", r.Method, r.URL.RequestURI())
				}
				if r.Header.Get("Authorization") != "Bearer uzc_test" || r.Header.Get("Content-Type") != "application/json" {
					t.Error("missing authorization or JSON content type")
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 || body["password"] != password {
					t.Errorf("unexpected request body: %v", err)
				}
				w.Header().Set("Retry-After", tc.retry)
				w.WriteHeader(tc.status)
				if tc.status != http.StatusNoContent {
					_ = json.NewEncoder(w).Encode(map[string]string{"error": tc.body, "reason": password})
				}
			}))
			defer srv.Close()
			err := newTestClient(srv).VaultUnlock(context.Background(), password)
			if calls != 1 {
				t.Fatalf("attempts = %d, want 1", calls)
			}
			if tc.code == ExitOK {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var ee *ExitError
			if !errors.As(err, &ee) || ee.Code != tc.code || err.Error() != tc.message {
				t.Fatalf("error = %v, want code %d message %q", err, tc.code, tc.message)
			}
			if errors.Is(err, ErrVaultIncorrectPassword) != tc.wrong {
				t.Error("incorrect password classification mismatch")
			}
			wantRetry := time.Duration(0)
			if tc.retry == "7" {
				wantRetry = 7 * time.Second
			}
			if ee.RetryAfter != wantRetry || ee.Reason != "" || ee.CurrentGateRevision != 0 {
				t.Fatalf("unexpected metadata: %+v", ee)
			}
			again := SafeVaultUnlockError(err)
			var againEE *ExitError
			if !errors.As(again, &againEE) || againEE.RetryAfter != wantRetry || errors.Is(again, ErrVaultRateLimited) != (tc.status == 429) || again.Error() != tc.message || errors.Is(again, ErrVaultIncorrectPassword) != tc.wrong {
				t.Fatalf("sanitization lost classification: %v", again)
			}
		})
	}
}

type vaultRoundTripper func(*http.Request) (*http.Response, error)

func (f vaultRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type vaultFailingBody struct {
	secret string
}

func (b vaultFailingBody) Read([]byte) (int, error) { return 0, errors.New(b.secret) }
func (b vaultFailingBody) Close() error             { return nil }

func TestHTTPClientVaultUnlockTransportSanitization(t *testing.T) {
	const password = "reflected private password"
	for _, readFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(readFailure), func(t *testing.T) {
			calls := 0
			c := &HTTPClient{
				BaseURL: "https://example.com",
				HTTP: &http.Client{Transport: vaultRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					if readFailure {
						return &http.Response{StatusCode: 204, Header: make(http.Header), Body: vaultFailingBody{secret: password}}, nil
					}
					return nil, errors.New(password)
				})},
			}
			err := c.VaultUnlock(context.Background(), password)
			if calls != 1 || err == nil || err.Error() != "vault unlock failed" || ExitCodeFor(err) != ExitUnreachable {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			if strings.Contains(fmt.Sprintf("%+v", errors.Unwrap(err)), password) {
				t.Error("wrapped error leaked password")
			}
		})
	}
}

func TestSafeVaultUnlockError(t *testing.T) {
	const secret = "private password"
	for _, tc := range []struct {
		name    string
		err     error
		code    int
		message string
		wrong   bool
		retry   time.Duration
	}{
		{"unknown", errors.New(secret), ExitGeneric, "vault unlock failed", false, 0},
		{"untrusted wording", errors.New("incorrect password"), ExitGeneric, "vault unlock failed", false, 0},
		{"untrusted auth", Exitf(ExitAuth, "incorrect password: %s", secret), ExitAuth, "vault unlock authentication failed", false, 0},
		{"sentinel", fmt.Errorf("%s: %w", secret, ErrVaultIncorrectPassword), ExitAuth, "incorrect password", true, 0},
		{"metadata", fmt.Errorf("%s: %w", secret, &ExitError{Code: ExitUnreachable, Err: errors.New(secret), RetryAfter: 7 * time.Second, Reason: secret, CurrentGateRevision: 42}), ExitUnreachable, "vault unlock failed", false, 7 * time.Second},
		{"trusted rate limit", fmt.Errorf("%s: %w", secret, &ExitError{Code: ExitUnreachable, Err: ErrVaultRateLimited, RetryAfter: 7 * time.Second, Reason: secret}), ExitUnreachable, "vault unlock rate limited; retry later (retry after 7s)", false, 7 * time.Second},
		{"invalid code", &ExitError{Code: 99, Err: errors.New(secret)}, ExitGeneric, "vault unlock failed", false, 0},
		{"zero code", &ExitError{Code: 0, Err: errors.New(secret)}, ExitGeneric, "vault unlock failed", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := SafeVaultUnlockError(SafeVaultUnlockError(tc.err))
			var ee *ExitError
			if !errors.As(err, &ee) || ee.Code != tc.code || ee.Error() != tc.message || ee.RetryAfter != tc.retry {
				t.Fatalf("error = %+v", err)
			}
			if ee.Reason != "" || ee.CurrentGateRevision != 0 || errors.Is(err, ErrVaultIncorrectPassword) != tc.wrong {
				t.Fatalf("unsafe metadata or classification: %+v", ee)
			}
			if strings.Contains(fmt.Sprintf("%+v", ee.Err), secret) {
				t.Error("secret retained in error chain")
			}
		})
	}
	if SafeVaultUnlockError(nil) != nil {
		t.Error("nil became an error")
	}
}

func TestFakeVault(t *testing.T) {
	ctx := context.Background()
	for _, locked := range []bool{false, true} {
		fixture := &FakeClient{VaultLocked: locked}
		status, err := fixture.VaultStatus(ctx)
		_, whoamiLocked, whoamiErr := fixture.WhoamiVault(ctx)
		if err != nil || whoamiErr != nil || status.Unlocked == whoamiLocked {
			t.Fatal("fake vault surfaces disagree")
		}
	}
	f := &FakeClient{VaultLocked: true}
	got, err := f.VaultStatus(ctx)
	if err != nil || got.Unlocked || f.VaultStatusCalls != 1 {
		t.Fatalf("initial status=%+v error=%v calls=%d", got, err, f.VaultStatusCalls)
	}
	if err := f.VaultUnlock(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	got, err = f.VaultStatus(ctx)
	if err != nil || !got.Unlocked || f.VaultLocked || f.VaultStatusCalls != 2 {
		t.Fatalf("unlocked status=%+v error=%v", got, err)
	}
	f.VaultUnlockErr = ErrVaultIncorrectPassword
	f.VaultLocked = true
	if err := f.VaultUnlock(ctx, "second"); !errors.Is(err, ErrVaultIncorrectPassword) || !f.VaultLocked {
		t.Fatalf("failed unlock changed state: %v", err)
	}
	if f.VaultUnlockCalls != 2 || strings.Join(f.VaultUnlockPasswords, ",") != "first,second" {
		t.Fatal("password attempts not recorded")
	}
	f.VaultStatusErr = io.EOF
	if _, err := f.VaultStatus(ctx); err != io.EOF {
		t.Fatalf("status error=%v", err)
	}
	f.VaultStatusErr, f.VaultUnlockErr, f.Err = nil, nil, io.ErrUnexpectedEOF
	if _, err := f.VaultStatus(ctx); err != io.ErrUnexpectedEOF {
		t.Fatalf("global status error=%v", err)
	}
	if err := f.VaultUnlock(ctx, "third"); err != io.ErrUnexpectedEOF || !f.VaultLocked {
		t.Fatalf("global unlock error=%v", err)
	}
	if f.VaultUnlockCalls != 3 || f.VaultUnlockPasswords[2] != "third" {
		t.Fatal("failed global attempt not recorded")
	}
}
