package main

// admin_product_egress.go is `uzi admin products egress-profiles <product>` (PRD #1976 M2): the
// read-only list of the site lists (egress profiles) a product's tokens may name on job create.
// Allowing and revoking a list are browser-session admin actions (the routes are cookie-only), so
// this verb never writes. Names and descriptions are untrusted display text and go through the
// Printer's table cells.

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func newAdminProductEgressProfilesCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "egress-profiles <product>",
		Short: "List the site lists a product may use (read-only)",
		Long: "List the site lists (egress profiles) an admin has allowed one product's tokens to " +
			"name when they create a job (PRD #1976). <product> is the product's name or id. A " +
			"product token may name only a list in this set; your own uzc_ token may name any " +
			"list. Allowing and removing a list are browser-only admin actions on Admin -> " +
			"Products. Removing one affects only jobs created afterwards.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			ps, err := c.AdminListProducts(cmd.Context())
			if err != nil {
				return err
			}
			prod, err := resolveProduct(ps, args[0])
			if err != nil {
				return err
			}
			list, err := c.AdminProductEgressProfiles(cmd.Context(), prod.ID)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				if list == nil {
					list = []uzicli.ProductEgressProfile{}
				}
				return p.JSON(list)
			}
			rows := make([][]string, 0, len(list))
			for _, e := range list {
				desc := e.Description
				if desc == "" {
					desc = "-"
				}
				rows = append(rows, []string{e.Name, e.CreatedAt.UTC().Format(time.RFC3339), desc})
			}
			return p.Table([]string{"NAME", "ALLOWED", "DESCRIPTION"}, rows)
		},
	}
}
