package main

import (
	"fmt"
	"sort"

	"github.com/spf13/cobra"

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
		newContextRmCmd(env, gf),
	)
	return cmd
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
			cfg, err := env.Store.LoadConfig()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			creds, err := env.Store.LoadCredentials()
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}
			active, _, err := resolveContextName(env, gf)
			if err != nil {
				return uzicli.Exitf(uzicli.ExitGeneric, "%v", err)
			}

			// Union of the two maps, sorted for deterministic output.
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

			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				out := make([]contextRow, 0, len(sorted))
				for _, n := range sorted {
					tok := creds.Contexts[n].Token
					out = append(out, contextRow{
						Name:        n,
						URL:         cfg.Contexts[n].URL,
						HasToken:    tok != "",
						TokenPrefix: tokenPrefix(tok),
						Current:     n == active,
					})
				}
				return p.JSON(out)
			}

			rows := make([][]string, 0, len(sorted))
			for _, n := range sorted {
				marker := ""
				if n == active {
					marker = "*"
				}
				rows = append(rows, []string{
					n,
					cfg.Contexts[n].URL, // blank ⇒ inherits default (D3)
					tokenStatus(creds.Contexts[n].Token),
					marker,
				})
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
