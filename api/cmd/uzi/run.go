package main

import (
	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunCmd — `uzi run` and its verbs. list/get/review are wired to the Client;
// logs/create/approve/reject/cancel/follow-up are stubs (M7/M8).
func newRunCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "List, inspect, and drive runs",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			runs, err := c.ListRuns(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(runs)
			}
			rows := make([][]string, 0, len(runs))
			for _, r := range runs {
				rows = append(rows, []string{r.ID, r.Status, r.Title})
			}
			return p.Table([]string{"ID", "STATUS", "TITLE"}, rows)
		},
	}

	get := &cobra.Command{
		Use:   "get <run-id>",
		Short: "Show a run's status and details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			run, err := c.GetRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(run)
			}
			return p.Table(
				[]string{"ID", "STATUS", "TITLE"},
				[][]string{{run.ID, run.Status, run.Title}},
			)
		},
	}

	logs := &cobra.Command{
		Use:   "logs <run-id>",
		Short: "Print a run's message history (REST polling)",
		Args:  cobra.ExactArgs(1),
		RunE:  stubRunE("run logs"),
	}
	logs.Flags().Bool("follow", false, "keep polling for new messages")

	review := &cobra.Command{
		Use:   "review <run-id>",
		Short: "Show the judge's review for a run (read-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			rv, err := c.RunReview(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(rv)
			}
			return p.Table(
				[]string{"RUN", "VERDICT", "STATUS"},
				[][]string{{rv.RunID, rv.Verdict, rv.Status}},
			)
		},
	}

	create := &cobra.Command{
		Use:   "create",
		Short: "Start a run on a repo/issue",
		RunE:  stubRunE("run create"),
	}
	create.Flags().StringSlice("agents", nil, "restrict the run to these agents")

	approve := &cobra.Command{
		Use:   "approve <run-id>",
		Short: "Approve a run's plan gate",
		Args:  cobra.ExactArgs(1),
		RunE:  stubRunE("run approve"),
	}
	reject := &cobra.Command{
		Use:   "reject <run-id>",
		Short: "Reject a run's plan gate",
		Args:  cobra.ExactArgs(1),
		RunE:  stubRunE("run reject"),
	}
	cancel := &cobra.Command{
		Use:   "cancel <run-id>",
		Short: "Cancel a run",
		Args:  cobra.ExactArgs(1),
		RunE:  stubRunE("run cancel"),
	}
	followUp := &cobra.Command{
		Use:   "follow-up <run-id>",
		Short: "Send a follow-up message to a run",
		Args:  cobra.ExactArgs(1),
		RunE:  stubRunE("run follow-up"),
	}

	cmd.AddCommand(list, get, logs, review, create, approve, reject, cancel, followUp)
	return cmd
}
