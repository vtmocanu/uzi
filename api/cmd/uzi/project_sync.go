package main

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newProjectSyncCmd — `uzi project-sync`: the CLI half of the GitHub Projects v2
// sync surface (PRD #576 M7). Only the READ (`status`) and the fix-loop re-seed
// (`resync`) are exposed here; the mutating link setup — Adopt, Provision, safe
// column auto-create, disable — stays web-only (D4), so the CLI never mints a link
// or a project, it only observes and re-seeds an already-linked board.
//
// Both subcommands take <repo> as a positional (path-with-namespace, e.g.
// "org/repo", or a raw repo UUID) and resolve it to the repo's id server-side path
// via resolveProjectSyncRepo. The endpoints sit in the RequireUser group (M7 moved
// them up), so a uzc_ Bearer token is accepted — a cookie-only mount would 401 it.
func newProjectSyncCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project-sync",
		Short: "Inspect and re-seed a repo's GitHub Projects v2 sync",
	}

	status := &cobra.Command{
		Use:   "status <repo>",
		Short: "Show a repo's GitHub project sync status (health, last sync, item count)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repoID, err := resolveProjectSyncRepo(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			st, err := c.GetProjectSyncStatus(cmd.Context(), repoID)
			p := env.printer(gf)
			if err != nil {
				// A 404 is the not-linked case (a foreign/unknown repo OR a repo with
				// no link row both 404 owner-scoped): normal output, never an error.
				if uzicli.ExitCodeFor(err) == uzicli.ExitNotFound {
					if p.Format == uzicli.FormatJSON {
						return p.JSON(map[string]any{"repo": repoID, "linked": false})
					}
					p.Printf("repo %s is not linked to a GitHub project\n", repoID)
					return nil
				}
				return err
			}
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{
					"repo":              repoID,
					"linked":            true,
					"project_number":    st.ProjectNumber,
					"owned_by_uzi":      st.OwnedByUzi,
					"last_synced_at":    st.LastSyncedAt,
					"last_error":        st.LastError,
					"item_count":        st.ItemCount,
					"unmatched_columns": st.UnmatchedColumns,
					"no_done_option":    st.NoDoneOption,
				})
			}
			health := "ok"
			if st.LastError != nil {
				health = "error"
			}
			lastSynced := "never"
			if st.LastSyncedAt != nil {
				lastSynced = st.LastSyncedAt.Format(time.RFC3339)
			}
			unmatched := "—"
			if len(st.UnmatchedColumns) > 0 {
				unmatched = strings.Join(st.UnmatchedColumns, ", ")
			}
			rows := [][]string{
				{"LINKED", "yes"},
				{"PROJECT", "#" + strconv.FormatInt(st.ProjectNumber, 10)},
				{"OWNED_BY_UZI", boolStr(st.OwnedByUzi)},
				{"HEALTH", health},
			}
			if st.LastError != nil {
				rows = append(rows, []string{"LAST_ERROR", *st.LastError})
			}
			rows = append(rows,
				[]string{"LAST_SYNCED", lastSynced},
				[]string{"ITEMS", strconv.Itoa(st.ItemCount)},
				[]string{"UNMATCHED_COLUMNS", unmatched},
				[]string{"NO_DONE_OPTION", boolStr(st.NoDoneOption)},
			)
			return p.Table([]string{"FIELD", "VALUE"}, rows)
		},
	}

	resync := &cobra.Command{
		Use:   "resync <repo>",
		Short: "Re-seed an already-linked board, picking up newly-added Status columns",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			repoID, err := resolveProjectSyncRepo(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if err := c.ResyncProjectSync(cmd.Context(), repoID); err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"repo": repoID, "resync": "started"})
			}
			if !gf.quiet {
				p.Printf("resync started for %s\n", repoID)
			}
			return nil
		},
	}

	cmd.AddCommand(status, resync)
	return cmd
}

// resolveProjectSyncRepo turns the <repo> positional into a repo id. A raw UUID is
// used as-is (the API path takes the repo's id); otherwise the arg is matched as a
// path-with-namespace ("org/repo") against the caller's own repos via ListRepos,
// resolving only on EXACTLY one match. Zero or many matches is a usage error naming
// `uzi repo list` as the way to find the right id — the same shape as
// resolveHandoffRepo's origin-match, kept local so this command does not couple to
// handoff's origin-detection path.
func resolveProjectSyncRepo(ctx context.Context, c uzicli.Client, arg string) (string, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", uzicli.Exitf(uzicli.ExitUsage, "a <repo> (path-with-namespace or repo id) is required")
	}
	if _, err := uuid.Parse(arg); err == nil {
		return arg, nil
	}
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, r := range repos {
		if r.PathWithNamespace == arg {
			matches = append(matches, r.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"could not match %q to a uzi repo; pass the repo id instead (see 'uzi repo list')", arg)
	default:
		return "", uzicli.Exitf(uzicli.ExitUsage,
			"%q is ambiguous — it matches %d uzi repos; pass the repo id to pick one (see 'uzi repo list')", arg, len(matches))
	}
}
