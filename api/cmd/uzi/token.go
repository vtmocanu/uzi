package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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
				// 🔴 THE JSON BRANCH CARRIES THE ELIGIBILITY TOO, AND FOR A WHILE IT DID
				// NOT (PRD #111 D23-JSON). The map above was computed and then discarded
				// here, so the human table gained an ELIGIBLE column while a script still
				// saw only the opt-in flag and could not learn that a pooled token will
				// never be picked — R7's silent no-op, surviving on exactly the surface
				// this CLI exists to serve. `SelfRateLimits` has one CLI call site, this
				// one, so there was no other command a script could reach it through. Two
				// tells it was an oversight rather than a decision: a discarded second HTTP
				// request on every --json invocation, and no comment explaining the split.
				//
				// The ?-vs-- distinction eligibilityCell is careful about needs an
				// equivalent here, and `null` is it: a pooled token whose eligibility is
				// UNKNOWN (the meters read failed, or did not mention it) must not look
				// like one that is fine. null, "" and absent must not collapse — so the
				// field is always present, and it is null exactly when the answer is not
				// known.
				out := make([]tokenListItem, 0, len(secrets))
				for _, s := range secrets {
					item := tokenListItem{SecretDTO: s}
					if status, ok := eligibility[s.ID]; ok {
						item.AutoStatus = &status
					}
					out = append(out, item)
				}
				return p.JSON(out)
			}
			rows := make([][]string, 0, len(secrets))
			for _, s := range secrets {
				// KIND distinguishes the credential kinds `ListSecrets` now returns
				// together (PRD #1147 M1): anthropic_token, codex_auth, openai_api_key.
				// STATUS renders the codex link state for a codex row, and "-" for an
				// anthropic row that carries none.
				//
				// POOL is the OPT-IN; ELIGIBLE is the live answer, and they are different
				// facts on purpose. A token can be opted in and still unpickable — its
				// gauge may never have polled, or its reading may have aged out — which is
				// exactly the state a user needs to see rather than infer.
				//
				// Both POOL and ELIGIBLE are Anthropic-only concepts: there is no Codex
				// auto-selection pool server-side, so a codex row's POOL/ELIGIBLE must read
				// "-" (not applicable), not "false"/"no" (which would imply an opt-in a user
				// could flip). ELIGIBLE already reads "-" for a codex row because it is
				// absent from the anthropic-only eligibility map and carries
				// AutoEligible=false; POOL is forced to "-" here for the same reason rather
				// than rendering a misleading boolean (PRD #1147 M3).
				//
				// The status string is RENDERED, never re-derived (D21): it is
				// autoselect.Classify's answer, the same function the selector gates on,
				// so this column cannot promise something the selector will not do.
				//
				// The label goes through cellText because it is user-authored. This comment
				// has now had BOTH of its stated premises go false in turn, which is worth
				// more than either correction: it first added "validateSecretLabel permits
				// unicode.Cf" (PRD #111 M2 made that false), and then "uzicli.Printer.Table
				// does not sanitize what it is handed" (#180 made THAT false — Table now
				// runs CellText over every cell). The conclusion has survived both, and the
				// reasons are the durable part: cellText additionally caps length, it names
				// this value as untrusted where the boundary cannot, and it keeps holding if
				// this row ever stops being rendered through Printer.Table.
				isAnthropic := s.Kind == kindAnthropicToken
				poolCell := "-"
				if isAnthropic {
					poolCell = boolStr(s.AutoEligible)
				}
				rows = append(rows, []string{
					s.ID, tokenKindAlias(s.Kind), cellText(s.Label), boolStr(s.IsDefault),
					poolCell, eligibilityCell(s.AutoEligible, eligibility[s.ID]),
					codexStatusCell(s.CodexStatus),
					s.CreatedAt.Format("2006-01-02"),
				})
			}
			return p.Table([]string{"ID", "KIND", "LABEL", "DEFAULT", "POOL", "ELIGIBLE", "STATUS", "CREATED"}, rows)
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
	//   - findSecretByLabel once had NO kind filter, while GetUserSecretIDByLabel
	//     does. That was vacuous while anthropic_token was the only kind; PRD #1147
	//     M1 landed codex_auth/openai_api_key, so this now passes kindAnthropicToken
	//     to findSecretByLabel and rejects a non-anthropic match with a usage error
	//     (M3). The client-side/server-side case-folding split above still stands.
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
			// KIND-FILTERED to anthropic_token (PRD #1147 M3). Before this milestone
			// findSecretByLabel had no kind filter and anthropic_token was the only kind,
			// so the resolution was vacuously anthropic-only; now that ListSecrets returns
			// codex_auth/openai_api_key rows too, an unfiltered match could cross-resolve a
			// codex secret sharing a label with an anthropic one, or land on a codex row
			// and try to set auto-eligible on it — but there is no Codex auto-selection
			// pool server-side, so pool stays Anthropic-only.
			target, ok := findSecretByLabel(secrets, kindAnthropicToken, args[0])
			if !ok {
				// If a NON-anthropic secret carries this label, name its kind so the caller
				// understands the refusal (the pool is Anthropic-only) rather than being told
				// no such token exists when one plainly does under `uzi token list`.
				if other, found := findSecretByLabel(secrets, "", args[0]); found {
					return uzicli.Exitf(uzicli.ExitUsage,
						"token pool applies only to Anthropic tokens; %q is a %s credential",
						args[0], tokenKindAlias(other.Kind))
				}
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

// tokenListItem is `uzi token list --json`'s element: the server's SecretDTO plus
// the live eligibility (PRD #111 D23-JSON).
//
// A CLI-LOCAL composed struct, not a field on SecretDTO, and that is D23's own
// ruling rather than a convenience. Putting auto_status on SecretDTO was considered
// and rejected: it would add a THIRD query feeding autoselect.Classify, widening the
// differential surface D21 exists to keep narrow, for a worse result — the status
// without the meters beside it is less useful than the meters. It would also make
// GET /api/me/secrets ship a field the handler cannot populate from its own query,
// i.e. a uniformly empty string, which is the confident-wrong-answer shape this PRD
// keeps refusing.
//
// Embedded, so the JSON is SecretDTO's keys plus one. A wrapper object would break
// every script that reads `.[].label` today, which is not a change this follow-up is
// entitled to make.
//
// The pointer is the contract: null means "not known" (the meters read failed, or
// the server did not mention this token), never "not eligible". A token that is
// genuinely un-pooled gets the server's own word for it, `not_pooled` — the JSON
// consumer wants the answer, where the table can lean on the POOL column beside it.
type tokenListItem struct {
	apitypes.SecretDTO
	AutoStatus *string `json:"auto_status"`
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

// Secret kind values as stored server-side and carried in SecretDTO.Kind (PRD
// #1147 M1). ListSecrets returns all three together, so the CLI names them rather
// than assuming anthropic_token is the only kind.
const (
	kindAnthropicToken = "anthropic_token" //nolint:gosec // G101: this is a secret KIND discriminator (the server's enum value), not a credential value; SecretDTO carries no secret material at all.
	kindCodexAuth      = "codex_auth"
	kindOpenAIAPIKey   = "openai_api_key" //nolint:gosec // G101: a secret KIND discriminator, not a credential value; SecretDTO carries no secret material at all.
)

// tokenKindAlias maps a SecretDTO.Kind to the short label the KIND column shows
// (PRD #1147 M3). The mapping is:
//
//	anthropic_token -> "anthropic"
//	codex_auth      -> "codex"
//	openai_api_key  -> "openai-key"
//
// An unrecognised kind prints as ITSELF rather than being dropped or mismapped —
// the same discipline eligibilityCell applies to an unknown status, so a kind this
// binary has never heard of is still legible instead of silently blank.
func tokenKindAlias(kind string) string {
	switch kind {
	case kindAnthropicToken:
		return "anthropic"
	case kindCodexAuth:
		return "codex"
	case kindOpenAIAPIKey:
		return "openai-key"
	default:
		return kind
	}
}

// codexStatusCell renders the STATUS column for `uzi token list` (PRD #1147 M3):
// the codex link state (staging/linked/failed/static) for a codex row, or "-" for
// an anthropic row, which carries no such state and leaves CodexStatus empty. The
// value is rendered verbatim, never re-derived, so a status this binary has not
// heard of prints as itself.
func codexStatusCell(status string) string {
	if status == "" {
		return "-"
	}
	return status
}

// findSecretByLabel resolves a user-facing label to the token it names,
// case-insensitively — matching the unique index 00077 put on
// (user_id, kind, lower(label)), so the CLI accepts the same spellings the server
// treats as one token.
//
// kind filters the search to a single SecretDTO.Kind; an empty kind matches any.
// The filter mirrors GetUserSecretIDByLabel's server-side kind scoping (PRD #1147
// M3): now that ListSecrets returns multiple kinds, a label can collide across
// kinds, so the pool command passes kindAnthropicToken to avoid cross-resolving a
// codex secret.
func findSecretByLabel(secrets []apitypes.SecretDTO, kind, label string) (apitypes.SecretDTO, bool) {
	want := strings.ToLower(strings.TrimSpace(label))
	for _, s := range secrets {
		if kind != "" && s.Kind != kind {
			continue
		}
		if strings.ToLower(s.Label) == want {
			return s, true
		}
	}
	return apitypes.SecretDTO{}, false
}
