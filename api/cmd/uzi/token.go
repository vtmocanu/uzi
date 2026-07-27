package main

import (
	"strings"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// newTokenCmd — `uzi token`. Only `list` and `pool` are wired, DELIBERATELY.
//
// Creating, renaming, set-defaulting and deleting a token are cookie-only web
// actions (PRD #104 D8): those routes are RequireAuth, not RequireUser, because
// minting or replacing a credential from a stolen CLI token is exactly the exposure
// D8 closes — the same reason `uzi worker` has no `create`. So the CLI surface is
// the read, plus the ONE write that yields nothing a stolen token did not already
// have.
//
// `pool` is that write (PRD #111 M2, D13). It re-points which of the caller's OWN
// tokens auto-selection may spend; it mints nothing and reveals nothing, which is
// why it got its own narrow RequireUser route rather than riding the cookie-only
// PATCH. Putting it on that PATCH would have meant moving rename, rotate and
// set-default within reach of a Bearer token as collateral damage.
func newTokenCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "List your Anthropic tokens and manage the auto-selection pool",
		Long: "List your named Anthropic tokens (label, default flag, pool opt-in,\n" +
			"timestamps), and opt one into or out of the auto-selection pool.\n\n" +
			"Adding, renaming, setting the default, and deleting a token are web-only,\n" +
			"because they mint or replace a credential and must not be reachable from a\n" +
			"CLI token. Use the web UI for those; use `uzi worker set-token` to point a\n" +
			"worker at one of the tokens this command lists.",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List your Anthropic tokens (labels, default flag, pool opt-in; never values)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			secrets, err := c.ListSecrets(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(secrets)
			}
			rows := make([][]string, 0, len(secrets))
			for _, s := range secrets {
				// POOL is the OPT-IN, not live eligibility. A pooled token can still be
				// unpickable right now — its gauge may never have polled, or its reading
				// may have aged out — and that live answer rides the rate-limit meters,
				// which are a web surface today (GET /me/rate-limits is cookie-only).
				// Naming the column POOL rather than ELIGIBLE keeps this from implying
				// it knows more than it does.
				//
				// The label goes through cellText: it is user-authored, validateSecretLabel
				// permits unicode.Cf (bidi overrides), and uzicli.Printer.Table does not
				// sanitize what it is handed.
				rows = append(rows, []string{
					s.ID, cellText(s.Label), boolStr(s.IsDefault), boolStr(s.AutoEligible),
					s.CreatedAt.Format("2006-01-02"),
				})
			}
			return p.Table([]string{"ID", "LABEL", "DEFAULT", "POOL", "CREATED"}, rows)
		},
	}

	// pool takes a LABEL, the name a human knows, and resolves it to an id against
	// the caller's own token list — the same shape `uzi worker set-token` uses. The
	// resolution is CLIENT-side and case-insensitive to match the unique index 00077
	// put on (user_id, kind, lower(label)), so `Console` and `console` name the same
	// token here exactly as they do everywhere else.
	var on, off bool
	pool := &cobra.Command{
		Use:   "pool <label>",
		Short: "Opt a token into (--on) or out of (--off) the auto-selection pool",
		Long: "Opt one of your Anthropic tokens into or out of the pool an `auto` worker\n" +
			"may spend from.\n\n" +
			"The pool is opt-in per token and empty by default, on purpose: a pool that\n" +
			"helped itself to every credential would spend the one you reserved for\n" +
			"something else. Opting a token IN does not guarantee it gets picked — the\n" +
			"selector also needs a fresh rate-limit reading for it, which the web\n" +
			"settings page shows per token.\n\n" +
			"Takes effect on the next claim: no restart, no new join token.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Exactly one of --on/--off, and one of them required. Defaulting either
			// way would make `uzi token pool console-key` a spend decision the user
			// never expressed — the same reasoning `worker set-token` uses to refuse a
			// bare invocation rather than guess between "clear" and "show me".
			switch {
			case on && off:
				return uzicli.Exitf(uzicli.ExitUsage, "pass either --on or --off, not both")
			case !on && !off:
				return uzicli.Exitf(uzicli.ExitUsage, "pass --on to add the token to the pool, or --off to remove it")
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			secrets, err := c.ListSecrets(cmd.Context())
			if err != nil {
				return err
			}
			target, ok := findSecretByLabel(secrets, args[0])
			if !ok {
				// A usage error, not a 404: the label never reached the server, so this
				// reports what actually happened — the caller holds no token by that name.
				return uzicli.Exitf(uzicli.ExitUsage, "no Anthropic token labelled %q; `uzi token list` shows yours", args[0])
			}
			out, err := c.SetTokenAutoEligible(cmd.Context(), target.ID, on)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(out)
			}
			if !gf.quiet {
				if on {
					p.Printf("token %q is now in the auto-selection pool\n", cellText(out.Label))
				} else {
					p.Printf("token %q is no longer in the auto-selection pool\n", cellText(out.Label))
				}
			}
			return nil
		},
	}
	pool.Flags().BoolVar(&on, "on", false, "add the token to the auto-selection pool")
	pool.Flags().BoolVar(&off, "off", false, "remove the token from the auto-selection pool")

	cmd.AddCommand(list, pool)
	return cmd
}

// findSecretByLabel resolves a user-facing label to the token it names,
// case-insensitively — matching the unique index 00077 put on
// (user_id, kind, lower(label)), so the CLI accepts the same spellings the server
// treats as one token.
func findSecretByLabel(secrets []apitypes.SecretDTO, label string) (apitypes.SecretDTO, bool) {
	want := strings.ToLower(strings.TrimSpace(label))
	for _, s := range secrets {
		if strings.ToLower(s.Label) == want {
			return s, true
		}
	}
	return apitypes.SecretDTO{}, false
}
