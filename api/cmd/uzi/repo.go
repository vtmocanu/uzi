package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/shellquote"
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
				rows = append(rows, []string{r.ID, r.PathWithNamespace, boolStr(r.Enabled), boolStr(r.DockerAllowlisted),
					crossCheckGuidanceCell(r.CodeCrossCheckGuidance), boolStr(r.CrossCheckRepoInstructionsEnabled), sanitizeTTY(strOr(r.AgentSourceDefault, "unset"))})
			}
			return p.Table([]string{"ID", "PATH", "ENABLED", "DOCKER", "CHECK_GUIDANCE", "CHECK_REPO_INSTRUCTIONS", "DEFAULT_AGENTS"}, rows)
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

	cmd.AddCommand(list, remove, newRepoEnvCmd(env, gf))
	return cmd
}

// newRepoEnvCmd — `uzi repo env`. Read-only by design (PRD #4416): variables are
// written in the web UI, where a secret's value is typed once and never shown again.
func newRepoEnvCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Inspect a repository's run variables",
	}
	list := &cobra.Command{
		Use:   "list <id>",
		Short: "List a repository's run variables (secret values are never shown)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			out, err := c.ListRepoEnv(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(out)
			}
			rows := make([][]string, 0, len(out.Variables))
			for _, v := range out.Variables {
				kind := "plain"
				if v.Secret {
					kind = "secret"
				}
				rows = append(rows, []string{
					uzicli.CellText(v.Name), kind, uzicli.CellText(v.State), runEnvValueCell(v),
					uzicli.CellText(v.Description), v.UpdatedAt.UTC().Format("2006-01-02 15:04:05Z"),
				})
			}
			if out.VaultLocked {
				_, _ = fmt.Fprintln(env.Stderr, vaultLockedNote(gf))
			}
			return p.Table([]string{"NAME", "KIND", "STATE", "VALUE", "DESCRIPTION", "UPDATED"}, rows)
		},
	}
	cmd.AddCommand(list)
	return cmd
}

// vaultLockedNote is the stderr hint printed when the list reports vault_locked (the
// vault is locked or its state could not be checked). The suggested unlock command
// carries the caller's --context/--url so it targets the same server.
func vaultLockedNote(gf *globalFlags) string {
	cmd := "uzi vault unlock"
	if gf.context != "" {
		cmd += " --context " + shellquote.Quote(gf.context)
	}
	if gf.url != "" {
		cmd += " --url " + shellquote.Quote(gf.url)
	}
	// The command sits alone on its own line with no trailing punctuation, so a copied
	// final argument (a --url or a quoted --context) stays exactly what was passed.
	return uzicli.CellText("Your vault is locked or its state could not be checked: plain values are withheld. If it is locked, run:") +
		"\n  " + uzicli.CellText(cmd)
}

// runEnvValueCell is the VALUE column: a secret never shows a value, a plain value
// the server withheld says so without guessing why (the vault may be locked, absent,
// or unable to open the value; the list's vault_locked is only a fail-closed hint,
// true when the vault is locked or its state could not be checked), otherwise the
// user-authored value, terminal-sanitized.
func runEnvValueCell(v apitypes.RunEnvVarDTO) string {
	switch {
	case v.Secret:
		return "(secret)"
	case v.ValueWithheld || v.Value == nil:
		return "(withheld)"
	default:
		return uzicli.CellText(*v.Value)
	}
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

// crossCheckGuidanceCell is the table cell for the owner's code cross-check
// guidance: only whether it is set and its byte length, never the text.
// `--json` carries the stored value; the CLI never writes guidance.
func crossCheckGuidanceCell(g *string) string {
	if g == nil || *g == "" {
		return "none"
	}
	return fmt.Sprintf("set (%d bytes)", len(*g))
}
