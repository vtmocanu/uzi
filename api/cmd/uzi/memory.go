package main

import (
	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// newMemoryCmd — `uzi memory`. list and rm are wired. There is no `save`: writing
// memory is the agent's in-run save_memory tool (PRD #90), not a human CLI action —
// the CLI is the owner's VISIBILITY + PURGE surface (S1), the one control over a
// poisoned entry that can outlive the repo injection that planted it.
func newMemoryCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "List and remove your agents' cross-run memory",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List your agent memory across all repos",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			memories, err := c.ListMemory(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(memories)
			}
			rows := make([][]string, 0, len(memories))
			for _, m := range memories {
				rows = append(rows, []string{m.ID, m.RepoName, m.Title, m.Basis, m.CreatedAt.Format("2006-01-02 15:04")})
			}
			return p.Table([]string{"ID", "REPO", "TITLE", "BASIS", "CREATED"}, rows)
		},
	}

	rm := &cobra.Command{
		Use:   "rm <memory-id>",
		Short: "Remove a memory entry",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if err := c.DeleteMemory(cmd.Context(), args[0]); err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"id": args[0], "deleted": true})
			}
			if !gf.quiet {
				p.Printf("memory %s removed\n", args[0])
			}
			return nil
		},
	}

	cmd.AddCommand(list, rm)
	return cmd
}
