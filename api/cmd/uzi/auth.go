package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newAuthCmd — `uzi auth` groups credential-plumbing verbs.
func newAuthCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage the stored CLI credential",
	}

	// `uzi auth token` — read a static token from stdin (piped) or a prompt (TTY)
	// and store it in credentials.toml for the default context. There is
	// deliberately no --token flag: a credential must never land on argv, readable
	// via ps / /proc/<pid>/cmdline (PRD #64). --with-token forces the stdin read
	// even on a TTY (piping into an interactive shell).
	token := &cobra.Command{
		Use:   "token",
		Short: "Store a static CLI token read from stdin (or a prompt on a TTY)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if env.Store == nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available to store the token")
			}
			withToken, _ := cmd.Flags().GetBool("with-token")
			tok, err := readToken(env, withToken)
			if err != nil {
				return err
			}
			// D4: `auth token` writes to the ACTIVE context, and an unknown
			// --context name is CREATED here (it does NOT call resolveSettings, so
			// D9 does not gate it). The name is validated at the write path because
			// it is stored raw and later appears verbatim in `context list --json`.
			name, _, err := resolveContextName(env, gf)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitAuth, "%v", err)
			}
			if err := termsafe.Validate("context name", name); err != nil {
				return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
			}
			creds, err := env.Store.LoadCredentials()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if creds.Contexts == nil {
				creds.Contexts = map[string]uzicli.Credential{}
			}
			c := creds.Contexts[name]
			c.Token = tok
			creds.Contexts[name] = c
			if err := env.Store.SaveCredentials(creds); err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if !gf.quiet {
				// name is termsafe.Validate'd above, so it carries no control bytes.
				_, _ = fmt.Fprintf(env.Stdout, "Stored token %s for context %s.\n", maskToken(tok), name)
			}
			return nil
		},
	}
	token.Flags().Bool("with-token", false, "read the token from stdin even on a TTY")

	// `uzi auth status` — report whether a credential is stored locally, and the
	// resolved URL. Never prints the token value — only a short prefix.
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether a credential is stored for the current context",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if all, _ := cmd.Flags().GetBool("all"); all {
				return runAuthStatusAll(env, gf)
			}
			// Report the ACTIVE context (D4), not the literal "default".
			name, _, err := resolveContextName(env, gf)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitAuth, "%v", err)
			}
			s, err := resolveSettings(env, gf)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{
					"context":      name,
					"url":          s.URL,
					"has_token":    s.Token != "",
					"token_prefix": tokenPrefix(s.Token),
				})
			}
			return p.Table(nil, [][]string{
				{"CONTEXT", name},
				{"URL", strDefault(s.URL, "(unset)")},
				{"TOKEN", tokenStatus(s.Token)},
			})
		},
	}
	status.Flags().Bool("all", false, "report every stored context, not just the active one")

	cmd.AddCommand(token, status)
	return cmd
}

// authStatusRow is one row of `uzi auth status --all`. It carries only the token
// PREFIX, never the value — the same guarantee `context list` makes.
type authStatusRow struct {
	Context     string `json:"context"`
	URL         string `json:"url"`
	HasToken    bool   `json:"has_token"`
	TokenPrefix string `json:"token_prefix"`
	Current     bool   `json:"current"`
}

// runAuthStatusAll lists every stored context — the union of config.toml and
// credentials.toml names, sorted — with each context's OWN stored URL (blank when
// it inherits the default per D3) and whether a token is stored, marking the
// active one. It never prints a token value. This overlaps `context list` in
// spirit; the two are kept consistent but remain separate commands.
func runAuthStatusAll(env Env, gf *globalFlags) error {
	if env.Store == nil {
		return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available")
	}
	ctxs, err := gatherContexts(env, gf)
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		out := make([]authStatusRow, 0, len(ctxs))
		for _, d := range ctxs {
			out = append(out, authStatusRow{
				Context:     d.Name,
				URL:         d.OwnURL,
				HasToken:    d.HasToken,
				TokenPrefix: d.TokenPrefix,
				Current:     d.Current,
			})
		}
		return p.JSON(out)
	}
	rows := make([][]string, 0, len(ctxs))
	for _, d := range ctxs {
		marker := ""
		if d.Current {
			marker = "*"
		}
		rows = append(rows, []string{d.Name, d.OwnURL, ctxTokenCell(d), marker})
	}
	return p.Table([]string{"CONTEXT", "URL", "TOKEN", "CURRENT"}, rows)
}

// readToken reads a token from stdin. On a TTY without --with-token it first
// prompts on stderr; otherwise it reads a piped value. Only the first line is
// taken and surrounding whitespace is trimmed.
func readToken(env Env, withToken bool) (string, error) {
	if env.StdinTTY && !withToken {
		_, _ = fmt.Fprint(env.Stderr, "Paste your uzi token: ")
	}
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", uzicli.Exitf(uzicli.ExitGeneric, "reading token from stdin: %v", err)
	}
	tok := strings.TrimSpace(line)
	if tok == "" {
		return "", uzicli.Exitf(uzicli.ExitUsage, "no token provided on stdin")
	}
	return tok, nil
}

// maskToken renders a stored token as its short prefix so a confirmation line
// never echoes the secret in full.
func maskToken(tok string) string {
	return tokenPrefix(tok) + "…"
}

// tokenPrefix returns at most the first 8 characters of a token (its `uzc_`/`uza_`
// family plus a few bytes), enough to recognise it without revealing it.
func tokenPrefix(tok string) string {
	const n = 8
	if len(tok) <= n {
		return tok
	}
	return tok[:n]
}

// tokenStatus renders the human TOKEN cell for `auth status`.
func tokenStatus(tok string) string {
	if tok == "" {
		return "(none)"
	}
	return maskToken(tok) + " (stored)"
}

func strDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// newWhoamiCmd — `uzi whoami`. Reports the identity and effective scope of the
// current credential (GET /api/auth/me): is_admin is false for a uzc_ token,
// true only for a uza_ token. Wired to the Client interface.
func newWhoamiCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the identity and scope of the current credential",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			u, err := c.Whoami(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(u)
			}
			return p.Table(
				[]string{"ID", "EMAIL", "ADMIN"},
				[][]string{{u.ID, u.Email, fmt.Sprintf("%t", u.IsAdmin)}},
			)
		},
	}
}
