package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRepoCmd — `uzi repo`. list and remove are wired to the Client.
func newRepoCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "List and remove repositories",
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
				rows = append(rows, []string{r.ID, r.PathWithNamespace, boolStr(r.Enabled), boolStr(r.DockerAllowlisted)})
			}
			return p.Table([]string{"ID", "PATH", "ENABLED", "DOCKER"}, rows)
		},
	}

	// remove is destructive: it deletes the repo row and cascades its board/run
	// history. The server only removes a DISABLED repo (409 otherwise), so the id
	// must already be disabled. Requires --force or an interactive [y/N] confirm.
	remove := &cobra.Command{
		Use:   "remove <id>",
		Short: "Remove a single disabled repository (deletes its board and run history)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			force, _ := cmd.Flags().GetBool("force")
			if !force {
				ok, err := confirmRemoveRepo(env, id)
				if err != nil {
					return err
				}
				if !ok {
					if !gf.quiet {
						_, _ = fmt.Fprintln(env.Stderr, "aborted")
					}
					return nil
				}
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if err := c.DeleteRepo(cmd.Context(), id); err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"id": id, "removed": true})
			}
			if !gf.quiet {
				p.Printf("Removed repo %s\n", id)
			}
			return nil
		},
	}
	remove.Flags().BoolP("force", "f", false, "skip the confirmation prompt")

	cmd.AddCommand(list, remove)
	return cmd
}

// confirmRemoveRepo prompts on stderr and reads a single line from stdin, returning
// true only for an explicit y/yes. An empty line or EOF (the default) declines, so a
// piped-in "" or a bare Enter aborts — the safe default for a destructive action.
func confirmRemoveRepo(env Env, id string) (bool, error) {
	_, _ = fmt.Fprintf(env.Stderr, "Remove repo %s? This permanently deletes its board and run history. [y/N]: ", id)
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, uzicli.Exitf(uzicli.ExitGeneric, "reading confirmation from stdin: %v", err)
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}
