package main

import (
	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// newTokenCmd — `uzi token`. Only `list` is wired, DELIBERATELY.
//
// Creating, renaming, set-defaulting and deleting a token are cookie-only web
// actions (PRD #104 D8): those routes are RequireAuth, not RequireUser, because
// minting or replacing a credential from a stolen CLI token is exactly the exposure
// D8 closes — the same reason `uzi worker` has no `create`. So the CLI surface is
// the read only. A `list` is metadata (labels, ids, default flag; never a value),
// which is safe to reach from a CLI token, and is what an agent or a script needs to
// discover the label to pass to `uzi worker set-token`.
func newTokenCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "List your Anthropic tokens",
		Long: "List your named Anthropic tokens (label, default flag, timestamps).\n\n" +
			"Adding, renaming, setting the default, and deleting a token are web-only,\n" +
			"because they mint or replace a credential and must not be reachable from a\n" +
			"CLI token. Use the web UI for those; use `uzi worker set-token` to point a\n" +
			"worker at one of the tokens this command lists.",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List your Anthropic tokens (labels, default flag; never values)",
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
				rows = append(rows, []string{s.ID, s.Label, boolStr(s.IsDefault), s.CreatedAt.Format("2006-01-02")})
			}
			return p.Table([]string{"ID", "LABEL", "DEFAULT", "CREATED"}, rows)
		},
	}

	cmd.AddCommand(list)
	return cmd
}
