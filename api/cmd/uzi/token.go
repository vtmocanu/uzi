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
			// The live eligibility, keyed by secret id (PRD #111 D23). Best-effort:
			// this read is a SECOND request and a failure of it must not fail the
			// listing — a user asking which tokens they hold still gets the answer, with
			// the eligibility column reading "?" rather than a lie. Before D23 this
			// endpoint was cookie-only and the CLI could not read it at all, which left
			// `uzi token pool x --on` unable to tell a caller that x can never be
			// picked: R7's silent no-op, surviving on the CLI half.
			eligibility := map[string]string{}
			if meters, merr := c.SelfRateLimits(cmd.Context()); merr == nil {
				for _, m := range meters {
					eligibility[m.SecretID] = m.AutoStatus
				}
			}

			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(secrets)
			}
			rows := make([][]string, 0, len(secrets))
			for _, s := range secrets {
				// POOL is the OPT-IN; ELIGIBLE is the live answer, and they are different
				// facts on purpose. A token can be opted in and still unpickable — its
				// gauge may never have polled, or its reading may have aged out — which is
				// exactly the state a user needs to see rather than infer.
				//
				// The status string is RENDERED, never re-derived (D21): it is
				// autoselect.Classify's answer, the same function the selector gates on,
				// so this column cannot promise something the selector will not do.
				//
				// The label goes through cellText: it is user-authored and
				// uzicli.Printer.Table does not sanitize what it is handed. This comment
				// used to add "validateSecretLabel permits unicode.Cf", which PRD #111 M2
				// made false — and which sat 124 lines from another comment in this same
				// file that was corrected while this one was missed. The conclusion is
				// unchanged: cellText folds newlines and tabs, which sanitizeTTY does not,
				// and rows written before the validator landed are never re-validated.
				rows = append(rows, []string{
					s.ID, cellText(s.Label), boolStr(s.IsDefault), boolStr(s.AutoEligible),
					eligibilityCell(s.AutoEligible, eligibility[s.ID]),
					s.CreatedAt.Format("2006-01-02"),
				})
			}
			return p.Table([]string{"ID", "LABEL", "DEFAULT", "POOL", "ELIGIBLE", "CREATED"}, rows)
		},
	}

	// pool takes a LABEL, the name a human knows, and resolves it to an id against the
	// caller's own token list.
	//
	// 🔴 THIS IS NOT THE SAME SHAPE AS `uzi worker set-token`, and an earlier version
	// of this comment said it was. set-token sends the LABEL and lets Postgres resolve
	// it through the kind-filtered GetUserSecretIDByLabel, using SQL `lower()` — the
	// same function 00077's unique index is built on. This resolves CLIENT-side with
	// Go's strings.ToLower and sends an id.
	//
	// Two consequences, both accepted here and neither hypothetical:
	//   - Go's and Postgres's case folding are not the same function. Measured:
	//     strings.ToLower("İstanbul") == strings.ToLower("istanbul") in Go. Postgres
	//     `lower('İstanbul')` under a UTF-8 collation is documented to yield a 9-char
	//     string distinct to the index — so both labels can coexist as separate rows
	//     while this command folds them together and pools whichever the list returns
	//     first. Exotic, and self-evident in `uzi token list`, which shows both rows.
	//   - findSecretByLabel has NO kind filter, while GetUserSecretIDByLabel does. It
	//     is vacuous today (anthropic_token is the only kind) and would matter the
	//     moment a second one lands.
	//
	// The reason it is client-side anyway: the toggle route is keyed on the secret id
	// (D13's narrow route), and giving it a label-accepting variant would widen a
	// credential-adjacent write surface to save one round trip. Routing this through
	// the server later would fix both bullets at once and is the cleaner end state.
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

// eligibilityCell renders one token's live auto-selection status for `uzi token
// list` (PRD #111 D23).
//
// Three cases, and the distinctions are the point:
//   - NOT pooled: "-", because the POOL column beside it already says so and
//     repeating "not in pool" would be noise on every row a user has not opted in.
//   - pooled with a status: the SERVER's word, rendered verbatim. The vocabulary is
//     autoselect.Status and this function deliberately does not interpret it — a
//     status this binary has never heard of prints as itself rather than being
//     mapped to something wrong or dropped.
//   - pooled with NO status: "?", meaning "the meters read failed or did not
//     mention this token". NOT "-" and not blank: a pooled token whose eligibility
//     is unknown must not look like one that is fine, which is the silent no-op the
//     column exists to remove.
func eligibilityCell(pooled bool, status string) string {
	if !pooled {
		return "-"
	}
	if status == "" {
		return "?"
	}
	return status
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
