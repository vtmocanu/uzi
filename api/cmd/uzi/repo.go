package main

import (
	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRepoCmd — `uzi repo`. list is wired to the Client.
func newRepoCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "List repositories",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List repositories",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repos, err := c.ListRepos(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(repos)
			}
			rows := make([][]string, 0, len(repos))
			for _, r := range repos {
				rows = append(rows, []string{r.ID, r.PathWithNamespace, boolStr(r.Enabled)})
			}
			return p.Table([]string{"ID", "PATH", "ENABLED"}, rows)
		},
	}

	cmd.AddCommand(list)
	return cmd
}
