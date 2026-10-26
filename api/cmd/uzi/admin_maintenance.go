package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newAdminMaintenanceCmd is `uzi admin maintenance`. Maintenance mode is switched on and off
// from the web Admin Instance page only (PRD #3454 D6: the settings write is cookie + CSRF
// only), so this group carries read-only verbs.
func newAdminMaintenanceCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "Inspect instance maintenance mode (read-only)",
		Long: "Instance maintenance mode holds new runs in the queue so an operator can drain the " +
			"instance before node maintenance. It is switched on and off in the web UI at " +
			"Admin → Instance; the CLI only reads it.",
	}
	cmd.AddCommand(newAdminMaintenanceStatusCmd(env, gf))
	return cmd
}

func newAdminMaintenanceStatusCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show maintenance mode and whether the instance has drained",
		Long: "Show whether maintenance mode is on (since when, the reason, who set it) and the drain " +
			"readout: how many runs are active, held (parked, waiting on an owner or on limits) or " +
			"under recovery custody, plus the queued runs and idle bound workers that do not block. " +
			"The CLEAR line means no blockers were visible in one snapshot at the printed time. It is " +
			"not an eviction authorization: re-run this command immediately before evicting, because " +
			"a claim that started before maintenance was enabled can still commit afterwards. " +
			"Each blocker table lists at most 50 runs; the counts are exact.\n\n" +
			"The exit code is 0 whether or not the instance is clear (errors use the usual codes), so " +
			"read the CLEAR / NOT CLEAR line, or use --json and check readout.clear. Needs an admin " +
			"session or an admin token.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			doc, err := c.AdminMaintenance(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(doc)
			}
			return renderMaintenanceStatus(p, doc)
		},
	}
}

// maintenanceTime renders an RFC3339 server timestamp as given, sanitized; "-" when absent.
func maintenanceTime(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return cellText(*s)
}

// maintenanceAge is how long an item had been open at the snapshot time (asOf), "-" when either
// timestamp is missing or unparsable.
func maintenanceAge(asOf string, since *string) string {
	if since == nil {
		return "-"
	}
	a, aerr := time.Parse(time.RFC3339, asOf)
	s, serr := time.Parse(time.RFC3339, *since)
	if aerr != nil || serr != nil {
		return "-"
	}
	d := a.Sub(s)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func optCell(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return cellText(*s)
}

// renderMaintenanceStatus prints the state, the counts, the CLEAR verdict and the blocker
// tables. Every server string goes through cellText/sanitizeTTY (the owner label and repo are
// user-authored, the reason is admin-authored).
func renderMaintenanceStatus(p *uzicli.Printer, doc apitypes.MaintenanceAdminDTO) error {
	st, ro := doc.State, doc.Readout
	if st.Active {
		p.Printf("Maintenance mode: ON\n")
		p.Printf("Since:  %s\n", maintenanceTime(st.Since))
		p.Printf("By:     %s\n", optCell(st.ByLabel))
		p.Printf("Reason: %s\n", optCell(st.Reason))
	} else {
		p.Printf("Maintenance mode: OFF\n")
	}
	p.Printf("As of:  %s\n\n", sanitizeTTY(ro.AsOf))
	p.Printf("Active runs:        %d\n", ro.Active.Count)
	p.Printf("Held runs:          %d\n", ro.Held.Count)
	p.Printf("Recovery custody:   %d\n", ro.Custody.Count)
	p.Printf("Queued (waiting):   %d\n", ro.Queued.Count)
	p.Printf("Idle bound workers: %d\n\n", ro.IdleBoundWorkers.Count)
	if ro.Clear {
		p.Printf("CLEAR: no running or held work is visible (as of %s).\n", sanitizeTTY(ro.AsOf))
	} else {
		p.Printf("NOT CLEAR: %d active, %d held and %d in recovery custody (as of %s).\n",
			ro.Active.Count, ro.Held.Count, ro.Custody.Count, sanitizeTTY(ro.AsOf))
	}
	p.Printf("This is a snapshot, not an eviction authorization: re-run this command right before evicting.\n")
	if !st.Active {
		p.Printf("Maintenance mode is off, so new runs are not being held back.\n")
	}
	for _, sec := range []struct {
		title string
		s     apitypes.MaintenanceSectionDTO
	}{{"Active", ro.Active}, {"Held", ro.Held}, {"Recovery custody", ro.Custody}} {
		if sec.s.Count == 0 {
			continue
		}
		p.Printf("\n%s (showing %d of %d)\n", sec.title, len(sec.s.Items), sec.s.Count)
		rows := make([][]string, 0, len(sec.s.Items))
		for _, it := range sec.s.Items {
			rows = append(rows, []string{
				sanitizeTTY(it.RunID), cellText(it.Kind), cellText(it.Status), cellText(it.OwnerLabel),
				optCell(it.Repo), maintenanceAge(ro.AsOf, it.Since), optCell(it.Tag),
			})
		}
		if err := p.Table([]string{"RUN", "KIND", "STATUS", "OWNER", "REPO", "AGE", "TAG"}, rows); err != nil {
			return err
		}
	}
	return nil
}
