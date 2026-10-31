package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newTokenCmd — `uzi token` provides metadata listing, credential testing and pool control.
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
		Short: "List and test your credentials, and manage the Anthropic pool",
		Long: "List your named credentials (label, default flag, pool opt-in,\n" +
			"timestamps), test one, or opt an Anthropic token into or out of the auto-selection pool.\n\n" +
			"Adding, renaming, setting the default, and deleting a token are web-only,\n" +
			"because they mint or replace a credential and must not be reachable from a\n" +
			"CLI token. Use the web UI for those; use `uzi worker set-token` to point a\n" +
			"worker at one of the tokens this command lists.",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List your Anthropic tokens (labels, enabled state, default flag, pool opt-in; never values)",
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
				// known. A disabled token is omitted from the meters read (PRD #1732), so
				// its auto_status is null too: a script reads `enabled` first, and treats
				// null as "unknown" only for an enabled token (docs/cli.md).
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
				// POOL reads the opt-in for the two poolable kinds, anthropic_token and
				// codex_auth (issue #3719), and "-" (not applicable) for openai_api_key,
				// which has no pool and so no flag a user could flip. ELIGIBLE stays
				// Anthropic-only: it is autoselect's live headroom answer, which the Codex
				// pool does not use, so a codex row reads "-" there (it is absent from the
				// anthropic-only eligibility map and is gated on isAnthropic below).
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
				if isAnthropic || s.Kind == kindCodexAuth {
					poolCell = boolStr(s.AutoEligible)
				}
				//
				// STATE is the credential's enablement (PRD #1732 D12). A disabled token keeps
				// its stored pool opt-in (D8), so POOL still reports the setting, but it is
				// never picked while disabled and the meters read omits it: ELIGIBLE reads
				// "-" rather than the "?" an unknown pooled token would get.
				eligible := eligibilityCell(s.AutoEligible, eligibility[s.ID])
				if !isAnthropic || secretDisabled(s) {
					eligible = "-"
				}
				rows = append(rows, []string{
					s.ID, tokenKindAlias(s.Kind), cellText(s.Label), boolStr(s.IsDefault),
					poolCell, eligible,
					codexStatusCell(s.CodexStatus),
					s.CreatedAt.Format("2006-01-02"),
					// Last, so the multi-word "disabled since <date>" never shifts the
					// columns a script reading this table by position already relies on.
					secretStateCell(s),
				})
			}
			return p.Table([]string{"ID", "KIND", "LABEL", "DEFAULT", "POOL", "ELIGIBLE", "STATUS", "CREATED", "STATE"}, rows)
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
	var poolKind string
	pool := &cobra.Command{
		Use:   "pool <label>",
		Short: "Opt a token or Codex login into (--on) or out of (--off) the auto-selection pool",
		Long: "Opt one of your Anthropic tokens or Codex logins into or out of the pool an\n" +
			"`auto` choice may spend from (an `auto` worker for Anthropic tokens; `uzi settings\n" +
			"codex-mode auto` for Codex logins).\n\n" +
			"The pool is opt-in per credential and empty by default, on purpose: a pool that\n" +
			"helped itself to every credential would spend the one you reserved for\n" +
			"something else. Opting a credential IN does not guarantee it gets picked — the\n" +
			"selector also needs a fresh rate-limit reading for it, which the web\n" +
			"settings page shows per credential.\n\n" +
			"A label may exist as both an Anthropic token and a Codex login: pass --kind\n" +
			"anthropic or --kind codex to say which. API keys (openai_api_key) cannot be pooled.\n\n" +
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
			target, err := resolvePoolTarget(secrets, poolKind, args[0])
			if err != nil {
				return err
			}
			// PRD #1732 D5: a disabled credential cannot join the pool. Refuse before the
			// write with the same advice the server's 409 gives ("credential is disabled;
			// enable it in Settings", workersvc.ErrCredentialDisabled); enabling is a
			// web-only action (D12). Opting a disabled credential OUT stays allowed.
			if on && secretDisabled(target) {
				return uzicli.Exitf(uzicli.ExitConflict,
					"%s %q is disabled; enable it in Settings before adding it to the pool", poolNoun(target.Kind), cellText(target.Label))
			}
			var out apitypes.SecretDTO
			if target.Kind == kindCodexAuth {
				out, err = c.SetCodexAutoEligible(cmd.Context(), target.ID, on)
			} else {
				out, err = c.SetTokenAutoEligible(cmd.Context(), target.ID, on)
			}
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(out)
			}
			if !gf.quiet {
				noun := poolNoun(target.Kind)
				if on {
					p.Printf("%s %q is now in the auto-selection pool\n", noun, cellText(out.Label))
				} else {
					p.Printf("%s %q is no longer in the auto-selection pool\n", noun, cellText(out.Label))
				}
			}
			return nil
		},
	}
	pool.Flags().BoolVar(&on, "on", false, "add the token to the auto-selection pool")
	pool.Flags().BoolVar(&off, "off", false, "remove the token from the auto-selection pool")
	pool.Flags().StringVar(&poolKind, "kind", "", "credential kind to pool: anthropic or codex (needed when a label exists in both)")

	var testKind string
	test := &cobra.Command{
		Use:   "test <label>",
		Short: "Test an enabled credential by label",
		Long:  "Test an enabled credential without revealing its value. Use --kind anthropic|codex|openai-key when a label occurs in more than one kind.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			kind := ""
			switch testKind {
			case "":
			case "anthropic":
				kind = kindAnthropicToken
			case "codex":
				kind = kindCodexAuth
			case "openai-key":
				kind = kindOpenAIAPIKey
			default:
				return uzicli.Exitf(uzicli.ExitUsage, "invalid kind %q; use anthropic, codex or openai-key", testKind)
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			secrets, err := c.ListSecrets(cmd.Context())
			if err != nil {
				return err
			}
			var target apitypes.SecretDTO
			matches := 0
			for _, s := range secrets {
				if kind != "" && s.Kind != kind {
					continue
				}
				if strings.EqualFold(s.Label, strings.TrimSpace(args[0])) {
					target = s
					matches++
				}
			}
			switch {
			case matches == 0:
				return uzicli.Exitf(uzicli.ExitUsage, "no credential labelled %q; `uzi token list` shows yours", args[0])
			case matches > 1:
				return uzicli.Exitf(uzicli.ExitUsage, "label %q is ambiguous; pass --kind anthropic, codex or openai-key", args[0])
			case secretDisabled(target):
				return uzicli.Exitf(uzicli.ExitConflict, "credential %q is disabled; enable it in Settings before testing", cellText(target.Label))
			}
			result, err := c.TestSecret(cmd.Context(), target.Kind, target.ID)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(struct {
					Kind    string `json:"kind"`
					Label   string `json:"label"`
					ID      string `json:"id"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Display string `json:"display"`
				}{target.Kind, target.Label, target.ID, result.Status, result.Reason, result.Display})
			}
			if !gf.quiet {
				p.Printf("%s %q: %s", tokenKindAlias(target.Kind), cellText(target.Label), cellText(result.Status))
				if result.Reason != "" {
					p.Printf(" (%s)", cellText(result.Reason))
				}
				if result.Display != "" {
					p.Printf(", %s", cellText(result.Display))
				}
				p.Printf("\n")
			}
			return nil
		},
	}
	test.Flags().StringVar(&testKind, "kind", "", "credential kind: anthropic, codex or openai-key")

	cmd.AddCommand(list, pool, test)
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
//   - pooled with a status: known server statuses use the same human labels as the
//     web UI. An unknown status prints verbatim so a newer server's answer remains
//     visible rather than being guessed or dropped.
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
	switch status {
	case "eligible":
		return "in pool"
	case "rejected":
		return "rejected by Anthropic"
	case "no_reading":
		return "never polled"
	case "unmeasured":
		return "no usage data"
	case "stale":
		return "stale reading"
	case "below_threshold":
		return "low headroom"
	default:
		return status
	}
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

// secretDisabled reports whether a credential is disabled (PRD #1732 D10). It keys on
// disabled_at rather than the enabled boolean on purpose: a server that predates the
// feature omits both fields, and a missing `enabled` decodes as false, which would read
// every credential as disabled. A missing disabled_at decodes as nil, i.e. enabled.
func secretDisabled(s apitypes.SecretDTO) bool {
	return s.DisabledAt != nil
}

// secretStateCell renders the STATE column for `uzi token list` (PRD #1732 D12): "enabled",
// or "disabled since <date>" (UTC, the same date format as CREATED).
func secretStateCell(s apitypes.SecretDTO) string {
	if !secretDisabled(s) {
		return "enabled"
	}
	return "disabled since " + s.DisabledAt.UTC().Format("2006-01-02")
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

// resolvePoolTarget picks the credential `uzi token pool <label>` toggles (issue #3719).
// Only anthropic_token and codex_auth are poolable. With kind ("anthropic" or "codex") the
// label is resolved within that kind alone, so a same-label credential of the other kind is
// never touched. Without it, a label held by exactly one poolable kind resolves to it, a
// label held by both is a usage error naming --kind, and an openai_api_key match is refused
// (it has no pool). Every refusal is a usage error raised before any write.
func resolvePoolTarget(secrets []apitypes.SecretDTO, kindFlag, label string) (apitypes.SecretDTO, error) {
	switch kindFlag {
	case "":
		anthropic, hasAnthropic := findSecretByLabel(secrets, kindAnthropicToken, label)
		codex, hasCodex := findSecretByLabel(secrets, kindCodexAuth, label)
		switch {
		case hasAnthropic && hasCodex:
			return apitypes.SecretDTO{}, uzicli.Exitf(uzicli.ExitUsage,
				"%q names both an Anthropic token and a Codex login; pass --kind anthropic or --kind codex", label)
		case hasAnthropic:
			return anthropic, nil
		case hasCodex:
			return codex, nil
		}
		if other, found := findSecretByLabel(secrets, "", label); found {
			return apitypes.SecretDTO{}, uzicli.Exitf(uzicli.ExitUsage,
				"token pool applies only to Anthropic tokens and Codex logins; %q is a %s credential",
				label, tokenKindAlias(other.Kind))
		}
		return apitypes.SecretDTO{}, uzicli.Exitf(uzicli.ExitUsage,
			"no Anthropic token or Codex login labelled %q; `uzi token list` shows yours", label)
	case "anthropic", "codex":
		kind, noun := kindAnthropicToken, "Anthropic token"
		if kindFlag == "codex" {
			kind, noun = kindCodexAuth, "Codex login"
		}
		if target, ok := findSecretByLabel(secrets, kind, label); ok {
			return target, nil
		}
		if other, found := findSecretByLabel(secrets, "", label); found {
			return apitypes.SecretDTO{}, uzicli.Exitf(uzicli.ExitUsage,
				"no %s labelled %q; that label is a %s credential", noun, label, tokenKindAlias(other.Kind))
		}
		return apitypes.SecretDTO{}, uzicli.Exitf(uzicli.ExitUsage,
			"no %s labelled %q; `uzi token list` shows yours", noun, label)
	default:
		return apitypes.SecretDTO{}, uzicli.Exitf(uzicli.ExitUsage, "invalid kind %q; use anthropic or codex", kindFlag)
	}
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

// poolNoun is the word `token pool` uses for a credential in its messages: a Codex alias is a
// login, an Anthropic credential a token.
func poolNoun(kind string) string {
	if kind == kindCodexAuth {
		return "login"
	}
	return "token"
}
