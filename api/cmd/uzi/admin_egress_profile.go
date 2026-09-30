package main

import (
	"fmt"
	"slices"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/egressprofile"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newAdminEgressProfileCmd — `uzi admin egress-profile`. A container group of two
// READ-ONLY leaves over GET /api/admin/egress-profiles[/{name}] (PRD #1906 M1). Creating,
// editing and deleting a site list are cookie-only admin writes (the PRD #64 split), so
// there is deliberately no create/edit/delete verb here: the web Admin page is the editor.
//
// Every server string (name, description, host entries, warning text) is admin-authored
// and validated by the api, and is still sanitized here: the render site is the trust
// boundary and does not depend on the validator holding. `list` uses the 200-rune
// cellText cap (a summary row); `show` bounds each field at its own validated maximum
// instead (fullCell), so a legal 253-character host or 500-character description prints
// whole.
func newAdminEgressProfileCmd(env Env, gf *globalFlags) *cobra.Command {
	group := &cobra.Command{
		Use:   "egress-profile",
		Short: "Read-only egress profiles: the named site lists for official-sources research (writes are web-only)",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List every egress profile with its entry and override counts",
		Long: "List the egress profiles (named site lists) configured on this instance: the " +
			"name, how many host entries it has, how many of those are multi-publisher " +
			"hosts admitted by an explicit override, when it was last changed, and its " +
			"description.\n\n" +
			"Read-only. Creating and editing a list is done from the web Admin page " +
			"(cookie-only admin writes). Needs a uza_ (admin_ro) token.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			ps, err := c.AdminListEgressProfiles(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(ps)
			}
			rows := make([][]string, 0, len(ps))
			for _, e := range ps {
				rows = append(rows, []string{
					cellText(e.Name),
					fmt.Sprintf("%d", len(e.Hosts)),
					fmt.Sprintf("%d", len(e.MultiPublisherOverride)),
					updatedCell(e.UpdatedAt),
					dashOr(cellText(e.Description)),
				})
			}
			return p.Table([]string{"NAME", "HOSTS", "OVERRIDES", "UPDATED", "DESCRIPTION"}, rows)
		},
	}

	show := &cobra.Command{
		Use:   "show <name>",
		Short: "Show one egress profile's host entries, overrides and warnings",
		Long: "Print one egress profile: its description and timestamps, then every host " +
			"entry. An entry is an exact host or \"*.base\", which matches the proper " +
			"subdomains of base but not base itself. OVERRIDE is yes for a multi-publisher " +
			"host an admin admitted explicitly; each such entry also prints a warning.\n\n" +
			"Read-only; exit 4 when no profile has that name. Needs a uza_ (admin_ro) token.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			e, err := c.AdminGetEgressProfile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(e)
			}
			return renderEgressProfile(p, e)
		},
	}

	group.AddCommand(list, show)
	return group
}

// renderEgressProfile prints the human view of one profile: a FIELD/VALUE table, a
// HOST/OVERRIDE table, then one warning line per overridden entry.
func renderEgressProfile(p *uzicli.Printer, e apitypes.EgressProfileDTO) error {
	if err := p.Table([]string{"FIELD", "VALUE"}, [][]string{
		{"name", cellText(e.Name)},
		{"description", dashOr(fullCell(e.Description, egressprofile.MaxDescriptionLen))},
		{"created_at", updatedCell(e.CreatedAt)},
		{"updated_at", updatedCell(e.UpdatedAt)},
	}); err != nil {
		return err
	}
	p.Printf("\n")
	rows := make([][]string, 0, len(e.Hosts))
	for _, h := range e.Hosts {
		override := "-"
		if slices.Contains(e.MultiPublisherOverride, h) {
			override = "yes"
		}
		rows = append(rows, []string{fullCell(h, egressprofile.MaxEntryLen), override})
	}
	if err := p.Table([]string{"HOST", "OVERRIDE"}, rows); err != nil {
		return err
	}
	for _, w := range e.Warnings {
		p.Printf("warning: %s\n", fullCell(w.Message, maxWarningLen))
	}
	return nil
}

// maxWarningLen bounds one printed warning: a warning quotes its entry (at most
// MaxEntryLen) inside a sentence or two of reason, so this is generous for any legal one.
const maxWarningLen = 4 * egressprofile.MaxEntryLen

// fullCell sanitizes a server string for one table cell (controls and invisible formatting
// stripped, tab and newline folded to a space) and bounds it at max runes, the field's own
// validated maximum, rather than cellText's 200-rune summary cap.
func fullCell(s string, max int) string {
	return capCell(uzicli.CellText(s), max)
}

// updatedCell renders a profile timestamp; the zero time (a server that omitted it) reads "-".
func updatedCell(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
