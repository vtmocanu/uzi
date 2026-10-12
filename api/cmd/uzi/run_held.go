package main

// run_held.go implements `uzi run held <run-id>` and `uzi run held expire <run-id> [--yes]`
// (issue #2545): the owner surface of held-work publication, where a failed run's committed work
// is published to refs/uzi-held/<run-id>/<generation> on the forge before custody is released.
// Every field is server data; the free-text and ref fields pass the terminal-safe renderers.

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunHeldCmd builds `uzi run held <run-id>` (list) with its `expire` subcommand.
func newRunHeldCmd(env Env, gf *globalFlags) *cobra.Command {
	held := &cobra.Command{
		Use:   "held <run-id>",
		Short: "List the held-work publications of a failed run",
		Long: "List the forge refs a failed run's committed work was published to " +
			"(refs/uzi-held/<run-id>/<generation>), with their state and when each is deleted.\n\n" +
			"A held ref is kept for the retention period after the run's custody was released against it, " +
			"then deleted. Fetch it before then with `git fetch origin <ref>`. To end the retention early, " +
			"use the expire subcommand (uzi run held expire).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			got, err := c.HeldPublications(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return renderHeldPublications(env, gf, args[0], got.Publications)
		},
	}
	held.AddCommand(newRunHeldExpireCmd(env, gf))
	return held
}

// newRunHeldExpireCmd builds `uzi run held expire <run-id> [--yes]`.
func newRunHeldExpireCmd(env Env, gf *globalFlags) *cobra.Command {
	expire := &cobra.Command{
		Use:   "expire <run-id> [--yes]",
		Short: "Delete a run's acknowledged held refs now instead of at their retention expiry",
		Long: "Make every ACKNOWLEDGED held-work ref of the run due for deletion now. The server's cleanup " +
			"deletes each ref shortly after; this command deletes nothing itself.\n\n" +
			"This is destructive. A held ref is the forge copy of the run's committed work; once it is " +
			"deleted the work can only be recovered from a clone you fetched it into. Fetch it first with " +
			"`git fetch origin <ref>` (see `uzi run held <run-id>`).\n\n" +
			"You must confirm interactively, or pass --yes for an unattended run. Without a terminal and " +
			"without --yes the command refuses and changes nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			runID := args[0]
			yes, _ := cmd.Flags().GetBool("yes")
			if !yes {
				if !env.StdinTTY {
					return uzicli.Exitf(uzicli.ExitUsage,
						"refusing to expire the held work of run %s without confirmation: pass --yes (stdin is not a "+
							"terminal, so an interactive confirmation cannot be shown)", sanitizeTTY(runID))
				}
				ok, err := confirmExpireHeld(env, runID)
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
			got, err := c.ExpireHeldPublications(cmd.Context(), runID)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(got)
			}
			if !gf.quiet {
				if got.Expired == 0 {
					p.Printf("run %s has no acknowledged held publication to expire\n", sanitizeTTY(runID))
				} else {
					p.Printf("%d held publication(s) of run %s are due for deletion; the server deletes each ref shortly\n",
						got.Expired, sanitizeTTY(runID))
				}
			}
			return nil
		},
	}
	expire.Flags().BoolP("yes", "y", false, "skip the interactive confirmation (required for a non-interactive run)")
	return expire
}

// confirmExpireHeld prompts on stderr and reads one line from stdin, true only for y/yes; an empty
// line or EOF declines, so a bare Enter aborts.
func confirmExpireHeld(env Env, runID string) (bool, error) {
	_, _ = fmt.Fprintf(env.Stderr,
		"Expire the held work of run %s now?\n"+
			"Its refs/uzi-held refs on the forge are deleted shortly after; fetch them first if you need the work. [y/N]: ",
		sanitizeTTY(runID))
	line, err := bufio.NewReader(env.Stdin).ReadString('\n')
	if err != nil && err != io.EOF {
		return false, uzicli.Exitf(uzicli.ExitGeneric, "reading confirmation from stdin: %v", err)
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// heldTimeCell renders an optional time as UTC RFC3339, "-" when absent.
func heldTimeCell(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// shortSha cuts a (sanitized) sha to 12 runes, so a hostile value cannot split into invalid UTF-8.
func shortSha(s string) string {
	r := []rune(cellText(s))
	if len(r) > 12 {
		r = r[:12]
	}
	return string(r)
}

// renderHeldPublications prints a run's held publications: --json the DTO array (never null), the
// human form one row per publication plus a fetch hint for the ones still on the forge.
func renderHeldPublications(env Env, gf *globalFlags, runID string, pubs []apitypes.HeldPublicationDTO) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		if pubs == nil {
			pubs = []apitypes.HeldPublicationDTO{}
		}
		return p.JSON(pubs)
	}
	if len(pubs) == 0 {
		if !gf.quiet {
			p.Printf("run %s has no held publications\n", sanitizeTTY(runID))
		}
		return nil
	}
	rows := make([][]string, 0, len(pubs))
	onForge := false
	for _, h := range pubs {
		if h.State == "acknowledged" || h.State == "created" || h.State == "delete_unknown" {
			onForge = true
		}
		rows = append(rows, []string{
			fmt.Sprintf("%d", h.Generation), cellText(h.State), cellText(h.Ref), shortSha(h.Tip), heldTimeCell(h.ExpiresAt),
		})
	}
	if err := p.Table([]string{"GEN", "STATE", "REF", "TIP", "EXPIRES"}, rows); err != nil {
		return err
	}
	for _, h := range pubs {
		if e := cellText(h.LastError); e != "" {
			p.Printf("gen %d: %s\n", h.Generation, e)
		}
		if r := cellText(h.RefusalReason); r != "" {
			p.Printf("gen %d: reason %s\n", h.Generation, r)
		}
	}
	if onForge && !gf.quiet {
		p.Printf("\nfetch a held ref with `git fetch origin <ref>`; end the retention early with `uzi run held expire %s`\n", sanitizeTTY(runID))
	}
	return nil
}

// heldReceiptLine renders a hold's held receipt: where its work was published and the commit it
// holds. "" for a hold that was not released against a held publication.
func heldReceiptLine(h apitypes.RecoveryCustodyHoldDTO) string {
	ref := cellText(h.HeldRef)
	if ref == "" {
		return ""
	}
	line := fmt.Sprintf("hold %s held: %s", cellText(h.ID), ref)
	if h.FinalReceipt != nil {
		if sha := shortSha(h.FinalReceipt.SourceSha); sha != "" {
			line += " @ " + sha
		}
	}
	return line
}
