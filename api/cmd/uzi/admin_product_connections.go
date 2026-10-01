package main

// admin_product_connections.go is `uzi admin products connections <product>` (PRD #1910 M5): the
// read-only list of the users who have connected a product through OAuth. Revoking a connection is
// a browser-session admin action (the route is cookie-only), so this verb never writes. Emails are
// untrusted display text and go through the Printer's table cells.

import (
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func newAdminProductConnectionsCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "connections <product>",
		Short: "List the users who connected a product (read-only)",
		Long: "List the live OAuth connections of one product (PRD #1910): the user, the approved " +
			"scopes, when they connected and when the connection was last used. <product> is the " +
			"product's name or id. A connection is listed while it is live, whatever the state of " +
			"its access tokens. Revoking a connection is a browser-only admin action on Admin -> " +
			"Products. --json prints {\"connections\": [...], \"truncated\": bool}; truncated is " +
			"true when the server cut the list at its row cap.",
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
			list, err := c.AdminProductConnections(cmd.Context(), prod.ID)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				if list.Connections == nil {
					list.Connections = []apitypes.AdminOAuthConnectionDTO{}
				}
				return p.JSON(list)
			}
			rows := make([][]string, 0, len(list.Connections))
			for _, conn := range list.Connections {
				used := "-"
				if conn.LastUsedAt != nil {
					used = conn.LastUsedAt.UTC().Format(time.RFC3339)
				}
				rows = append(rows, []string{conn.OwnerEmail, strings.Join(conn.Scopes, ","), conn.ConnectedAt.UTC().Format(time.RFC3339), used, conn.ID})
			}
			if err := p.Table([]string{"USER", "SCOPES", "CONNECTED", "LAST USED", "ID"}, rows); err != nil {
				return err
			}
			if list.Truncated {
				p.Println("list truncated at the server's row cap; the oldest connections are not shown")
			}
			return nil
		},
	}
}
