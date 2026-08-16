package uzicli

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The client side of the browser-brokered `uzi login` flow (PRD #64 M5/M8). The
// CLI mints a PKCE verifier locally, sends ONLY its S256 challenge to start, prints
// a user_code + a consent URL (best-effort opening a browser), and polls until the
// human approves in an already-authenticated tab. No loopback listener, no session
// JWT in a URL — so it works over SSH and in containers.

// verifierBytes is the entropy of the PKCE verifier: 32 bytes = 256 bits, RFC 7636's
// upper bound. The verifier is the real security boundary (the user_code is only a
// human-typed confirmation), so it is generated from crypto/rand and never logged.
const verifierBytes = 32

// GenerateVerifier returns a fresh PKCE verifier and its S256 challenge, both
// base64url (RawURLEncoding, no padding) — the exact encoding the server's
// s256Challenge compares against (handler/cli_auth_flow.go): challenge =
// base64url(sha256(<verifier string>)). The verifier is sent ONLY in the poll body
// (a POST, so it never lands in an access log).
func GenerateVerifier() (verifier, challenge string, err error) {
	buf := make([]byte, verifierBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// DefaultClientDesc derives the client_desc the CLI sends to start — hostname + OS —
// so the consent screen and the token list can tell one laptop from another. The
// server bounds it to 200 runes and substitutes "unknown client" when empty, so a
// missing hostname is harmless.
func DefaultClientDesc() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return fmt.Sprintf("uzi CLI on %s (%s)", host, runtime.GOOS)
}

// OpenBrowser best-effort opens url in the user's default browser. It is ONLY a
// convenience: `uzi login` always PRINTS the URL as well, so a headless box, an SSH
// session, or a missing opener never blocks a login — the human opens the URL
// themselves. An error here is informational, never fatal. url is built from the
// configured base URL plus a fixed path and a UUID query param (no shell), and each
// argument is passed as a distinct argv entry, so no shell interpolation occurs.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// CLIAuthStartResult is the server's reply to POST /api/auth/cli/start.
type CLIAuthStartResult struct {
	RequestID string
	UserCode  string
	// ExpiresIn is the request lifetime in seconds; the CLI stops polling past it.
	ExpiresIn int
	// Interval is the poll cadence the server asks the CLI to honour, in seconds.
	Interval int
}

// CLIAuthPollStatus is the outcome class of a single poll.
type CLIAuthPollStatus int

const (
	// CLIAuthPending: 202 — not yet approved, keep polling.
	CLIAuthPending CLIAuthPollStatus = iota
	// CLIAuthAuthorized: 200 — approved and minted; Token/User are set (once, ever).
	CLIAuthAuthorized
	// CLIAuthTerminal: 410 — expired, denied, or already consumed; stop, Reason set.
	CLIAuthTerminal
)

// CLIAuthPollResult carries one poll's outcome.
type CLIAuthPollResult struct {
	Status CLIAuthPollStatus
	// Reason is the terminal status string ("expired"|"denied"|"consumed"), set only
	// when Status is CLIAuthTerminal.
	Reason string
	// Token is the minted CLI token, set only when Status is CLIAuthAuthorized. It is
	// stored 0600 and never printed.
	Token string
	// User is the logged-in identity, set only when Status is CLIAuthAuthorized.
	User apitypes.UserDTO
}
