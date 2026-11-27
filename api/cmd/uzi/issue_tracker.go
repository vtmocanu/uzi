package main

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newIssueTrackerCmd builds `uzi issue-tracker` (PRD #4173 M6): a read-only view of the
// caller's issue tracker connections. Connecting and linking stay web-only.
func newIssueTrackerCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "issue-tracker",
		Short: "Show issue tracker connections",
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List your issue tracker connections",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			conns, err := c.ListIssueTrackerConnections(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				if conns == nil {
					conns = []uzicli.IssueTrackerConnection{}
				}
				return p.JSON(conns)
			}
			rows := make([][]string, 0, len(conns))
			for _, cn := range conns {
				rows = append(rows, []string{
					cn.Kind, cn.BaseURL, issueTrackerAccount(cn),
					cn.CreatedAt.UTC().Format(time.RFC3339), issueTrackerTime(cn.LastVerifiedAt),
				})
			}
			return p.Table([]string{"KIND", "BASE URL", "ACCOUNT", "CREATED", "LAST VERIFIED"}, rows)
		},
	}
	cmd.AddCommand(list)
	return cmd
}

// issueTrackerAccount is the ACCOUNT cell: the login, falling back to the account id.
func issueTrackerAccount(cn uzicli.IssueTrackerConnection) string {
	if cn.AccountLogin != "" {
		return cn.AccountLogin
	}
	return cn.AccountID
}

func issueTrackerTime(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// newRepoIssueTrackerStatesCmd builds `uzi repo issue-tracker-states <repo>`: the linked
// repo's State mapping and its project's States. <repo> is a path-with-namespace or a repo
// UUID; a UUID goes straight to the route, because ListRepos lists enabled repos only and a
// repo is linked before its first enable.
func newRepoIssueTrackerStatesCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "issue-tracker-states <repo>",
		Short: "Show a linked repo's issue tracker State mapping and project States",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repoID, err := resolveProjectSyncRepoHint(cmd.Context(), c, args[0],
				"; pass the repo id instead (a repo not yet enabled is not listed by 'uzi repo list'; its id is shown in the Map States panel on the web Repos page)")
			if err != nil {
				return err
			}
			res, err := c.GetIssueTrackerStates(cmd.Context(), repoID)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				if res.States == nil {
					res.States = []uzicli.IssueTrackerState{}
				}
				return p.JSON(res)
			}
			if m := res.Mapping; m == nil {
				p.Println("no State mapping saved")
			} else {
				p.Printf("OPEN state:    %s\n", cellText(m.OpenState))
				p.Printf("CLOSED state:  %s\n", cellText(m.ClosedState))
				rows := make([][]string, 0, len(m.Columns))
				for _, col := range m.Columns {
					rows = append(rows, []string{col.LabelName, col.State})
				}
				if err := p.Table([]string{"COLUMN", "STATE"}, rows); err != nil {
					return err
				}
				p.Println("Columns not listed are unmapped: a move into one is refused until it is mapped.")
			}
			p.Println()
			rows := make([][]string, 0, len(res.States))
			for _, s := range res.States {
				rows = append(rows, []string{s.Name, boolStr(s.Closable), boolStr(s.Openable)})
			}
			return p.Table([]string{"STATE", "CLOSABLE", "OPENABLE"}, rows)
		},
	}
}
