package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
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
	s, err := resolveSettings(env, gf)
	if err != nil {
		return err
	}
	if strings.TrimSpace(s.URL) == "" {
		return uzicli.Exitf(uzicli.ExitUsage, "no uzi API URL configured: pass --url or set $UZI_URL")
	}
	c := env.NewClient(s)
	ctx := cmd.Context()

	verifier, challenge, err := uzicli.GenerateVerifier()
	if err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}
	start, err := c.StartCLIAuth(ctx, challenge, uzicli.DefaultClientDesc())
	if err != nil {
		return err
	}

	consentURL := strings.TrimRight(s.URL, "/") + "/cli-auth?request=" + url.QueryEscape(start.RequestID)
	// Instructions go to stderr so stdout stays clean for --json; the user_code and
	// URL are essential, so they print even with --quiet. The token is NEVER printed.
	fmt.Fprintf(env.Stderr, "\nTo authorize this login, open:\n\n    %s\n\n", consentURL)
	fmt.Fprintf(env.Stderr, "and enter this one-time code when asked:\n\n    %s\n\n", start.UserCode)
	if err := openBrowser(consentURL); err != nil {
		fmt.Fprintln(env.Stderr, "(could not open a browser automatically — open the URL above)")
	}
	if !gf.quiet {
		fmt.Fprintln(env.Stderr, "Waiting for approval...")
	}

	res, err := pollUntilDone(ctx, c, start, verifier)
	if err != nil {
		return err
	}
	return finishLogin(env, gf, s.URL, res)
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
// (credentials.toml, 0600) for the default context, then reports the identity. The
// token is stored, never printed.
func finishLogin(env Env, gf *globalFlags, resolvedURL string, res uzicli.CLIAuthPollResult) error {
	// Persist the endpoint first (non-secret): a login is an explicit "configure this
	// context" action, so subsequent commands work without --url / $UZI_URL.
	cfg, err := env.Store.LoadConfig()
	if err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}
	if cfg.Contexts == nil {
		cfg.Contexts = map[string]uzicli.Context{}
	}
	cc := cfg.Contexts["default"]
	cc.URL = resolvedURL
	cfg.Contexts["default"] = cc
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
	cr := creds.Contexts["default"]
	cr.Token = res.Token
	creds.Contexts["default"] = cr
	if err := env.Store.SaveCredentials(creds); err != nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}

	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(res.User)
	}
	if !gf.quiet {
		fmt.Fprintf(env.Stdout, "Logged in as %s. Token stored for context default.\n", res.User.Email)
	}
	return nil
}

// newLogoutCmd — `uzi logout`. Local-only: removes the stored credential for the
// default context. Revoking server-side is a webui action (Decision 16), so this
// only clears the local file — it never calls the API.
func newLogoutCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the locally stored CLI credential (does not revoke it server-side)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if env.Store == nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available")
			}
			creds, err := env.Store.LoadCredentials()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if cur, ok := creds.Contexts["default"]; !ok || cur.Token == "" {
				if !gf.quiet {
					fmt.Fprintln(env.Stdout, "No stored credential for context default.")
				}
				return nil
			}
			delete(creds.Contexts, "default")
			if err := env.Store.SaveCredentials(creds); err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if !gf.quiet {
				fmt.Fprintln(env.Stdout, "Removed the stored credential for context default (not revoked server-side).")
			}
			return nil
		},
	}
}
