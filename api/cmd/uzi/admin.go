package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// newAdminCmd — `uzi admin`. Read-only by construction: the admin write
// endpoints are cookie-only, so there is nothing for the CLI to call. Needs a
// uza_ (admin_ro) token; a 403 from the scope gate maps to exit 3 with an
// actionable message (statusError in the client).
func newAdminCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Read-only admin views (requires an admin-scoped token)",
	}

	users := &cobra.Command{
		Use:   "users",
		Short: "List all users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			us, err := c.AdminListUsers(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(us)
			}
			rows := make([][]string, 0, len(us))
			for _, u := range us {
				rows = append(rows, []string{u.ID, u.Email, boolStr(u.IsAdmin)})
			}
			return p.Table([]string{"ID", "EMAIL", "ADMIN"}, rows)
		},
	}

	runs := &cobra.Command{
		Use:   "runs",
		Short: "List all runs across the factory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			rs, err := c.AdminListRuns(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(rs)
			}
			rows := make([][]string, 0, len(rs))
			for _, r := range rs {
				rows = append(rows, []string{r.ID, strOr(r.OwnerEmail, "-"), r.Kind, r.Status, runTitle(r.RunDTO)})
			}
			return p.Table([]string{"ID", "OWNER", "KIND", "STATUS", "TITLE"}, rows)
		},
	}

	workers := &cobra.Command{
		Use:   "workers",
		Short: "List all workers across the factory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			ws, err := c.AdminListWorkers(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(ws)
			}
			rows := make([][]string, 0, len(ws))
			for _, w := range ws {
				// 🔴 THE CROSS-TENANT ONE. This prints another user's worker name into an
				// ADMIN's terminal, and worker names are validated for length only — no
				// control-character check, no Cf check, and workers.name carries no CHECK.
				// So a crafted name is terminal control injection into someone else's
				// session, and an embedded newline forges a row in a table an admin reads
				// to make decisions. Strictly worse than the token-label case, where the
				// validator had already put ANSI out of reach.
				//
				// The render site is the trust boundary and does not depend on the
				// validator holding; hardening the validator is a separate change.
				//
				// 🔴 "THE CROSS-TENANT ONE" ABOVE MEANS "of the two worker-name cells",
				// NOT "of this file". `uzi admin cli-tokens` below renders `t.Name` raw
				// into the same kind of cross-tenant admin table, and CLI-token names get
				// only strings.TrimSpace — no control-character check either. It is left
				// deliberately: it is PRD #64's surface, this change does not make it
				// worse, and render-site fixes here are scoped to the surfaces this PRD
				// touches. It goes up as a follow-up with the worker-name validator.
				// Named here so the next reader sees a signpost rather than two
				// sanitized cells and a file that looks finished.
				rows = append(rows, []string{w.ID, w.OwnerEmail, cellText(w.Name), w.Status})
			}
			return p.Table([]string{"ID", "OWNER", "NAME", "STATUS"}, rows)
		},
	}

	usage := &cobra.Command{
		Use:   "usage",
		Short: "Show token/cost usage across the factory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			u, err := c.AdminUsage(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(u)
			}
			return renderAdminUsage(p, u)
		},
	}

	rateLimits := &cobra.Command{
		Use:   "rate-limits",
		Short: "Show Claude rate-limit meters per user",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			rls, err := c.AdminRateLimits(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(rls)
			}
			// One table row per (user, token) since #104 M5: the table gains a TOKEN
			// column, and a token-less user is a single row with an empty token cell and
			// a no_token status — so every user still appears exactly once when they hold
			// no token, and once per token when they hold several.
			rows := make([][]string, 0, len(rls))
			for _, rl := range rls {
				if len(rl.Tokens) == 0 {
					rows = append(rows, []string{
						rl.Email, vaultCell(rl.VaultLocked), "-", "no_token", "-", "-",
					})
					continue
				}
				for _, tk := range rl.Tokens {
					rows = append(rows, []string{
						rl.Email,
						vaultCell(rl.VaultLocked),
						tokenCell(tk.Label, tk.IsDefault),
						tk.Limits.Status,
						windowPct(tk.Limits.FiveHour),
						windowPct(tk.Limits.SevenDay),
					})
				}
			}
			return p.Table([]string{"EMAIL", "VAULT", "TOKEN", "STATUS", "5H%", "7D%"}, rows)
		},
	}

	cliTokens := &cobra.Command{
		Use:   "cli-tokens",
		Short: "List every CLI token in the factory (metadata only)",
		Long: "List every CLI token in the factory with its owner, so an operator can " +
			"answer who holds standing credentials to this instance and whether any are " +
			"stale or unexpected.\n\n" +
			"Never prints a token value or its hash: the value is not stored at all, and " +
			"the hash is excluded by the query's projection. USED is the coarse (<=1/min) " +
			"last-use stamp and, with the prefix, the only forensic signal there is -- " +
			"there is no per-request audit log. A blank EXPIRES is a never-expiring " +
			"user-scope token, which is the row most worth looking at.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			ts, err := c.AdminListCLITokens(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(ts)
			}
			rows := make([][]string, 0, len(ts))
			for _, t := range ts {
				rows = append(rows, []string{
					t.OwnerEmail,
					t.TokenPrefix,
					t.Name,
					t.Scope,
					tokenStateCell(t),
					tsCell(t.LastUsedAt),
					tsCell(t.ExpiresAt),
				})
			}
			return p.Table([]string{"OWNER", "PREFIX", "NAME", "SCOPE", "STATE", "USED", "EXPIRES"}, rows)
		},
	}

	cmd.AddCommand(users, runs, workers, usage, rateLimits, cliTokens)
	return cmd
}

// renderAdminUsage prints the factory lifetime totals plus the per-user breakdown.
func renderAdminUsage(p *uzicli.Printer, u apitypes.AdminUsageDTO) error {
	lt := u.Factory.Lifetime
	p.Printf("factory (lifetime): input=%d cache_read=%d cache_creation=%d output=%d cost=$%.2f (runs=%d)\n",
		lt.InputTokens, lt.CacheReadTokens, lt.CacheCreationTokens, lt.OutputTokens, lt.CostUSD, u.Factory.RunCount)
	if len(u.Users) == 0 {
		return nil
	}
	p.Println()
	rows := make([][]string, 0, len(u.Users))
	for _, row := range u.Users {
		rows = append(rows, []string{
			row.Email,
			fmt.Sprintf("%d", row.Usage.InputTokens),
			fmt.Sprintf("%d", row.Usage.OutputTokens),
			fmt.Sprintf("$%.2f", row.Usage.CostUSD),
			fmt.Sprintf("%d", row.RunCount),
		})
	}
	return p.Table([]string{"EMAIL", "INPUT", "OUTPUT", "COST", "RUNS"}, rows)
}

func vaultCell(locked bool) string {
	if locked {
		return "locked"
	}
	return "unlocked"
}

// tokenCell renders a token's label for the admin table, marking the default so an
// operator can see at a glance which credential a user's unbound workers spend
// against (#104 M5). The label is a name, never the credential.
//
// 🔴 THE ONLY PATH WHERE A CRAFTED LABEL REACHES SOMEONE OTHER THAN ITS AUTHOR, which
// is why it goes through cellText even though every other admin cell is
// server-controlled. Everywhere else a hostile label can only spoof its own owner's
// terminal; here it renders in an ADMIN's, listing every user's credentials.
//
// PRD #111 M2's validateSecretLabel now rejects Cf on write, which narrows this
// without closing it: rows written before that landed were never re-validated, and
// nothing re-validates on read. The auditor enumerated every write path that sets
// user_secrets.label — the two body-driven ones validate, and UpsertDefaultUserSecret
// and the seed pass compiled-in constants — so HISTORY is precisely the remaining
// route, and a render-site defense is what covers history and future drift alike.
func tokenCell(label string, isDefault bool) string {
	if isDefault {
		return cellText(label) + " (default)"
	}
	return cellText(label)
}

// windowPct renders a rate-limit window's percentage, or "-" when the window is
// absent (status other than "ok", or Anthropic reported none).
func windowPct(w *apitypes.RateLimitWindow) string {
	if w == nil {
		return "-"
	}
	return fmt.Sprintf("%d", w.Pct)
}

// tokenStateCell folds the two independent ways a token stops working into one
// column. They are genuinely different states and an operator reading an incident
// needs to tell them apart: "revoked" is a deliberate act with a trail, "expired" is
// the server-set 90-day bound arriving. Revoked wins when both are true, because it
// is the one someone DID.
func tokenStateCell(t apitypes.AdminCLITokenDTO) string {
	switch {
	case t.Revoked:
		return "revoked"
	case t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()):
		return "expired"
	default:
		return "active"
	}
}

// tsCell renders a nullable timestamp. "-" means the column is genuinely empty, not
// unknown: never used, or (for expires_at) never expires.
func tsCell(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
