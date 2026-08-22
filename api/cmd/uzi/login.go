package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// openBrowser launches the consent URL in the user's browser. It is a package var
// so tests can stub it (a real browser must never spawn under `go test`). The login
// flow calls it best-effort and never fails on its error — the URL is always
// printed too (headless / SSH).
var openBrowser = uzicli.OpenBrowser

// authWait returns the channel that fires after the inter-poll interval. A package
// var so tests drive the poll loop with no real delay; runtime honours the
// server-returned interval via time.After.
var authWait = func(d time.Duration) <-chan time.Time { return time.After(d) }

// newLoginCmd — `uzi login`. Browser-brokered, poll-based token acquisition (M5
// server endpoints + M8 client): mint a PKCE verifier locally, send only its
// challenge to start, print the user_code + a consent URL (best-effort open a
// browser), then poll until the human approves in an already-authenticated tab. No
// loopback listener, so it works over SSH and in containers.
func newLoginCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Log in via the browser and store a CLI token",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogin(cmd, env, gf)
		},
	}
}

func runLogin(cmd *cobra.Command, env Env, gf *globalFlags) error {
	if env.Store == nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available to store the token")
	}
	// D4: login WRITES to the active/named context, and D4 lets an unknown
	// --context name be CREATED. resolveSettings would reject a brand-new name
	// under D9, so login resolves only the URL (via resolveURL, no existence
	// gate) — it mints a fresh token from the server and never reads a stored one.
	name, _, err := resolveContextName(env, gf)
	if err != nil {
		return uzicli.Exitf(uzicli.ExitAuth, "%v", err)
	}
	// Validate the target name before contacting the server: it is stored raw and
	// later appears verbatim in `context list --json`.
	if err := termsafe.Validate("context name", name); err != nil {
		return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
	}
	resolvedURL, err := resolveURL(env, gf, name)
	if err != nil {
		return err
	}
	if strings.TrimSpace(resolvedURL) == "" {
		return uzicli.Exitf(uzicli.ExitUsage, "no uzi API URL configured: pass --url or set $UZI_URL")
	}
	c := env.NewClient(uzicli.Settings{URL: resolvedURL})
	ctx := cmd.Context()

	verifier, challenge, err := uzicli.GenerateVerifier()
	if err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}
	start, err := c.StartCLIAuth(ctx, challenge, uzicli.DefaultClientDesc())
	if err != nil {
		return err
	}

	consentURL := strings.TrimRight(resolvedURL, "/") + "/cli-auth?request=" + url.QueryEscape(start.RequestID)
	// Instructions go to stderr so stdout stays clean for --json; the user_code and
	// URL are essential, so they print even with --quiet. The token is NEVER printed.
	_, _ = fmt.Fprintf(env.Stderr, "\nTo authorize this login, open:\n\n    %s\n\n", consentURL)
	// user_code is server-supplied and printed outside the Printer (#180). The LOCAL
	// cellText, not uzicli.CellText: it adds compactText's 200-char cap on top of the same
	// strip, so a hostile server cannot flood the terminal here (#220). consentURL
	// beside it needs nothing: its server-supplied half is url.QueryEscape'd, which
	// percent-encodes every control byte.
	_, _ = fmt.Fprintf(env.Stderr, "and enter this one-time code when asked:\n\n    %s\n\n", cellText(start.UserCode))
	if err := openBrowser(consentURL); err != nil {
		_, _ = fmt.Fprintln(env.Stderr, "(could not open a browser automatically — open the URL above)")
	}
	if !gf.quiet {
		_, _ = fmt.Fprintln(env.Stderr, "Waiting for approval...")
	}

	res, err := pollUntilDone(ctx, c, start, verifier)
	if err != nil {
		return err
	}
	return finishLogin(env, gf, name, resolvedURL, res)
}

// pollUntilDone polls at the server-returned cadence until the request is approved
// (authorized), terminal (expired/denied/consumed), or the request lifetime lapses.
// It respects the server's interval (with a 1s floor so a misbehaving interval:0
// never busy-loops) and stops at expires_in.
func pollUntilDone(ctx context.Context, c uzicli.Client, start uzicli.CLIAuthStartResult, verifier string) (uzicli.CLIAuthPollResult, error) {
	interval := time.Duration(start.Interval) * time.Second
	if interval < time.Second {
		interval = time.Second
	}
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	for {
		res, err := c.PollCLIAuth(ctx, start.RequestID, verifier)
		if err != nil {
			return uzicli.CLIAuthPollResult{}, err
		}
		switch res.Status {
		case uzicli.CLIAuthAuthorized:
			return res, nil
		case uzicli.CLIAuthTerminal:
			return uzicli.CLIAuthPollResult{}, uzicli.Exitf(uzicli.ExitGeneric,
				"login %s — run `uzi login` again", terminalReason(res.Reason))
		case uzicli.CLIAuthPending:
			// keep polling
		}
		if start.ExpiresIn > 0 && !time.Now().Before(deadline) {
			return uzicli.CLIAuthPollResult{}, uzicli.Exitf(uzicli.ExitGeneric,
				"login timed out before approval — run `uzi login` again")
		}
		select {
		case <-ctx.Done():
			return uzicli.CLIAuthPollResult{}, uzicli.Exitf(uzicli.ExitGeneric, "login cancelled")
		case <-authWait(interval):
		}
	}
}

// terminalReason renders a 410 poll status as a human phrase.
func terminalReason(status string) string {
	switch status {
	case "denied":
		return "was denied"
	case "consumed":
		return "was already completed elsewhere"
	default:
		return "expired"
	}
}

// finishLogin persists the resolved URL (config.toml) and the minted token
// (credentials.toml, 0600) for the active/named context (D4), then reports the
// identity. The token is stored, never printed. `name` is already termsafe.Validate'd
// by runLogin, so it is safe to interpolate into the confirmation line.
func finishLogin(env Env, gf *globalFlags, name, resolvedURL string, res uzicli.CLIAuthPollResult) error {
	// Persist the endpoint first (non-secret): a login is an explicit "configure this
	// context" action, so subsequent commands work without --url / $UZI_URL.
	cfg, err := env.Store.LoadConfig()
	if err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]uzicli.Context{}
	}
	cc := cfg.Contexts[name]
	cc.URL = resolvedURL
	cfg.Contexts[name] = cc
	if err := env.Store.SaveConfig(cfg); err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}

	creds, err := env.Store.LoadCredentials()
	if err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}
	if creds.Contexts == nil {
		creds.Contexts = map[string]uzicli.Credential{}
	}
	cr := creds.Contexts[name]
	cr.Token = res.Token
	creds.Contexts[name] = cr
	if err := env.Store.SaveCredentials(creds); err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}

	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(res.User)
	}
	if !gf.quiet {
		// Server-supplied email, outside the Printer (#180); name is validated. The LOCAL
		// cellText (200-char cap) rather than the unbounded uzicli.CellText, so a hostile
		// server's giant email cannot flood stdout here (#220).
		_, _ = fmt.Fprintf(env.Stdout, "Logged in as %s. Token stored for context %s.\n", cellText(res.User.Email), name)
	}
	return nil
}

// newLogoutCmd — `uzi logout`. Local-only: removes the stored credential for the
// ACTIVE context (D4/D8). Revoking server-side is a webui action (Decision 16), so
// this only clears the local file — it never calls the API. D8: it touches ONLY
// credentials, deleting the token entry; the context's config entry (its URL) is
// left intact so a re-login works without reconfiguring the endpoint.
func newLogoutCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the locally stored CLI credential (does not revoke it server-side)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if env.Store == nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available")
			}
			name, _, err := resolveContextName(env, gf)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitAuth, "%v", err)
			}
			creds, err := env.Store.LoadCredentials()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if cur, ok := creds.Contexts[name]; !ok || cur.Token == "" {
				if !gf.quiet {
					// name may come from -c/$UZI_CONTEXT unvalidated; CellText strips
					// any control bytes before it reaches the terminal.
					_, _ = fmt.Fprintf(env.Stdout, "No stored credential for context %s.\n", uzicli.CellText(name))
				}
				return nil
			}
			// D8: drop only the token entry; the config URL is deliberately untouched.
			delete(creds.Contexts, name)
			if err := env.Store.SaveCredentials(creds); err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if !gf.quiet {
				_, _ = fmt.Fprintf(env.Stdout, "Removed the stored credential for context %s (not revoked server-side).\n", uzicli.CellText(name))
			}
			return nil
		},
	}
}
