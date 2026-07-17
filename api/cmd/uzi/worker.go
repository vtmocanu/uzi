package main

import (
	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// newWorkerCmd — `uzi worker`. list is wired; rm is a stub (M7). There is no
// `create`: minting a join token is a webui action (it returns a credential
// that reads decrypted secrets), so it stays cookie-only (Decision 18).
func newWorkerCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "List and remove workers",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List workers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			workers, err := c.ListWorkers(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(workers)
			}
			rows := make([][]string, 0, len(workers))
			for _, w := range workers {
				rows = append(rows, []string{w.ID, w.Name, w.Status})
			}
			return p.Table([]string{"ID", "NAME", "STATUS"}, rows)
		},
	}

	rm := &cobra.Command{
		Use:   "rm <worker-id>",
		Short: "Remove a worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if err := c.DeleteWorker(cmd.Context(), args[0]); err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"id": args[0], "deleted": true})
			}
			if !gf.quiet {
				p.Printf("worker %s removed\n", args[0])
			}
			return nil
		},
	}

	cmd.AddCommand(list, rm)
	return cmd
}
