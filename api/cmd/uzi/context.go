package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newContextCmd — `uzi context` groups the named-context management verbs
// (PRD #427 M2). Contexts are pure client-side credential *selection*: a
// context picks which already-stored token/URL a command uses; it never grants
// capability (D6). All four verbs are local-only and never call the API.
func newContextCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Manage named credential contexts (multi-token profiles)",
		Long: "Named contexts let you store several uzi tokens (e.g. a uzc_ owner token and a\n" +
			"uza_ admin token) and switch between them. A context selects which stored\n" +
			"credential a command sends; it never changes your authority, which is the\n" +
			"token's server-enforced scope.",
	}
	cmd.AddCommand(
		newContextListCmd(env, gf),
		newContextCurrentCmd(env, gf),
		newContextUseCmd(env, gf),
		newContextSetCmd(env, gf),
		newContextRmCmd(env, gf),
	)
	return cmd
}

// newContextSetCmd — `uzi context set <name> --url <url>` (D7). The ONLY way to
// create a URL-only context: both `auth token` and `login` require a token, so a
// context that stores just an endpoint (e.g. to pre-seed a URL a later login
// inherits) has no other author. Create-or-update via load-modify-save, so any
// other future Context field on an existing entry is preserved.
func newContextSetCmd(env Env, gf *globalFlags) *cobra.Command {
	var urlFlag string
	c := &cobra.Command{
		Use:   "set <name> --url <url>",
		Short: "Create or update a URL-only context (no token)",
		Long: "Store an API base URL under a named context without a token — the only way to\n" +
			"create a URL-only context, since storing a token is `uzi auth token` or `uzi login`.\n" +
			"An existing context keeps its stored token; only its URL is updated.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if env.Store == nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available")
			}
			name := args[0]
			if err := termsafe.Validate("context name", name); err != nil {
				return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
			}
			if strings.TrimSpace(urlFlag) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--url is required")
			}
			// Validate the URL at the write path too (symmetric with the name):
			// it is stored raw and later emitted verbatim in the `--json` channel,
			// which does not strip control/bidi bytes.
			if err := termsafe.Validate("url", urlFlag); err != nil {
				return uzicli.Exitf(uzicli.ExitUsage, "%v", err)
			}
			cfg, err := env.Store.LoadConfig()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if cfg.Contexts == nil {
				cfg.Contexts = map[string]uzicli.Context{}
			}
			// Load-modify-save preserves any other fields on an existing entry.
			ctx := cfg.Contexts[name]
			ctx.URL = urlFlag
			cfg.Contexts[name] = ctx
			if err := env.Store.SaveConfig(cfg); err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if !gf.quiet {
				// name is validated; urlFlag is user-supplied — the Printer's hardened
				// table strips any control bytes from it.
				_ = env.printer(gf).Table(nil, [][]string{{fmt.Sprintf("Set context %s to %s.", name, urlFlag)}})
			}
			return nil
		},
	}
	c.Flags().StringVar(&urlFlag, "url", "", "the API base URL to store for this context (required)")
	return c
}

// contextRow is the per-context JSON shape emitted by `uzi context list --json`.
// It carries only the token PREFIX, never the value.
type contextRow struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	HasToken    bool   `json:"has_token"`
	TokenPrefix string `json:"token_prefix"`
	Current     bool   `json:"current"`
}

// ctxDisplay is one context's render-ready summary. It is built once by
// gatherContexts so the two invariants both `context list` and `auth status
// --all` must uphold — a token is shown only as a PREFIX (never the value), and
// the URL cell is the context's OWN stored URL (blank when it inherits the
// default per D3) — live in a single place and cannot drift between the two
// commands. It deliberately carries no raw token.
type ctxDisplay struct {
	Name        string
	OwnURL      string // blank ⇒ inherits the default context's URL (D3)
	HasToken    bool
	TokenPrefix string
	Current     bool
}

// gatherContexts returns every stored context as a render-ready row: the sorted
// UNION of the names in config.toml and credentials.toml, each with its own URL,
// whether a token is stored (masked to a prefix here, once), and whether it is
// the active context. Shared by `context list` and `auth status --all`.
func gatherContexts(env Env, gf *globalFlags) ([]ctxDisplay, error) {
	cfg, err := env.Store.LoadConfig()
	if err != nil {
		return nil, uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}
	creds, err := env.Store.LoadCredentials()
	if err != nil {
		return nil, uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
	}
	active, _, err := resolveContextName(env, gf)
	if err != nil {
		return nil, uzicli.Exitf(uzicli.ExitAuth, "%v", err)
	}
	names := map[string]struct{}{}
	for n := range cfg.Contexts {
		names[n] = struct{}{}
	}
	for n := range creds.Contexts {
		names[n] = struct{}{}
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	out := make([]ctxDisplay, 0, len(sorted))
	for _, n := range sorted {
		tok := creds.Contexts[n].Token
		out = append(out, ctxDisplay{
			Name:        n,
			OwnURL:      cfg.Contexts[n].URL,
			HasToken:    tok != "",
			TokenPrefix: tokenPrefix(tok),
			Current:     n == active,
		})
	}
	return out, nil
}

// ctxTokenCell renders the human TOKEN column from a masked summary (no raw
// token). It matches tokenStatus's output for the same token, from the prefix
// alone. (Named to avoid the admin.go tokenCell, which is a different helper.)
func ctxTokenCell(d ctxDisplay) string {
	if !d.HasToken {
		return "(none)"
	}
	return d.TokenPrefix + "… (stored)"
}

// newContextListCmd — `uzi context list`. One row per context, the UNION of the
// names in config.toml and credentials.toml (a context can exist in only one:
// a URL-only context from `context set`, or a token-only context). The URL cell
// shows the context's OWN stored URL and is blank when it has none — the blank
// is meaningful (it signals "inherits the default URL" per D3), so the table
// never implies every context stores its own URL. The TOKEN cell reports only
// whether a token is stored and a short prefix, never the value.
func newContextListCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List every stored context, its URL, whether it has a token, and which is current",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if env.Store == nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available")
			}
			ctxs, err := gatherContexts(env, gf)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				out := make([]contextRow, 0, len(ctxs))
				for _, d := range ctxs {
					out = append(out, contextRow{
						Name:        d.Name,
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
			return p.Table([]string{"NAME", "URL", "TOKEN", "CURRENT"}, rows)
		},
	}
}

// newContextCurrentCmd — `uzi context current`. Prints the STICKY context
// (config.Current), or "default" when unset. This is the sticky value only: a
// `--context`/`$UZI_CONTEXT` override selects a context per-invocation but does
// NOT change Current, so it is not reflected here.
func newContextCurrentCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "current",
		Short: "Print the sticky current context (set by `uzi context use`)",
		Long: "Print the sticky current context — the one `uzi context use` set, or \"default\"\n" +
			"if none. A `--context`/`-c` flag or $UZI_CONTEXT overrides the context for a\n" +
			"single invocation without changing this sticky value.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if env.Store == nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available")
			}
			cfg, err := env.Store.LoadConfig()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			current := cfg.Current
			if current == "" {
				current = "default"
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"current": current})
			}
			return p.Table(nil, [][]string{{current}})
		},
	}
}

// newContextUseCmd — `uzi context use <name>`. Sets config.Current (the sticky
// context) after validating (D5) that the context already exists in config or
// credentials; an unknown name is a usage error that names how to create it.
func newContextUseCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "use <name>",
		Short: "Set the sticky current context (must already exist)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if env.Store == nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available")
			}
			name := args[0]
			cfg, err := env.Store.LoadConfig()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			creds, err := env.Store.LoadCredentials()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			// "default" is the implicit context that always resolves (empty token
			// when nothing is stored), so switching stickiness back to it never
			// requires a materialized entry — this makes the PRD's own opening
			// example (`uzi context use default` before the first `auth token`)
			// work. Any OTHER name must already exist in config or credentials (D5).
			_, inCfg := cfg.Contexts[name]
			_, inCreds := creds.Contexts[name]
			if name != "default" && !inCfg && !inCreds {
				return uzicli.Exitf(uzicli.ExitUsage,
					"unknown context %q; create it first with `uzi auth token --context %s` or `uzi login --context %s`",
					name, name, name)
			}
			cfg.Current = name
			if err := env.Store.SaveConfig(cfg); err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if !gf.quiet {
				// name is user-supplied; route it through the Printer's hardened
				// table rather than a raw Fprintf.
				_ = env.printer(gf).Table(nil, [][]string{{fmt.Sprintf("Switched to context %s.", name)}})
			}
			return nil
		},
	}
}

// newContextRmCmd — `uzi context rm <name>`. Deletes the context from BOTH
// config and credentials and saves both. If the removed context was the sticky
// Current, Current is reset to "default" before saving. Removing "default"
// clears its stored entries but the implicit "default" still resolves later
// (empty token) — that is fine, not special-cased beyond the Current reset.
func newContextRmCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a context from config and credentials",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if env.Store == nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "no config directory available")
			}
			name := args[0]
			cfg, err := env.Store.LoadConfig()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			creds, err := env.Store.LoadCredentials()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			_, inCfg := cfg.Contexts[name]
			_, inCreds := creds.Contexts[name]
			if !inCfg && !inCreds {
				if !gf.quiet {
					_ = env.printer(gf).Table(nil, [][]string{{fmt.Sprintf("No such context %s.", name)}})
				}
				return nil
			}
			delete(cfg.Contexts, name)
			delete(creds.Contexts, name)
			if cfg.Current == name {
				cfg.Current = "default"
			}
			// Save credentials FIRST, so a partial failure drops the token rather
			// than orphaning it on disk: if SaveConfig then fails, the context still
			// resolves (config keeps the entry) but the secret is already gone.
			if err := env.Store.SaveCredentials(creds); err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if err := env.Store.SaveConfig(cfg); err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			if !gf.quiet {
				_ = env.printer(gf).Table(nil, [][]string{{fmt.Sprintf("Removed context %s.", name)}})
			}
			return nil
		},
	}
}
