package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newAdminProviderCmd - `uzi admin provider`: two READ-ONLY leaves over GET /api/admin/providers[/{id}]
// (PRD #4324). Creating, editing, retiring and deleting a shared definition are cookie-only admin
// writes (the web Admin page is the editor), so there is deliberately no write verb here.
//
// Every server string is drawn through cellText: the render site is the trust boundary.
func newAdminProviderCmd(env Env, gf *globalFlags) *cobra.Command {
	group := &cobra.Command{
		Use:   "provider",
		Short: "Read-only admin-shared model providers: definitions with selected-user and key counts (writes are web-only)",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List every admin-shared provider definition with its counts",
		Long: "List the admin-shared model provider definitions on this instance: name, engine, base URL,\n" +
			"auth style, whether it is retired, its current revision, how many users have selected it\n" +
			"and how many users have attached a key to it.\n\n" +
			"Read-only. Creating and editing a shared definition is done from the web Admin page\n" +
			"(cookie-only admin writes). Needs a uza_ (admin_ro) token.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			ps, err := c.AdminListSharedProviders(cmd.Context())
			if err != nil {
				return providerErr(err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				if ps == nil {
					ps = []apitypes.AdminSharedProviderDTO{}
				}
				return p.JSON(ps)
			}
			rows := make([][]string, 0, len(ps))
			for _, d := range ps {
				rows = append(rows, []string{
					cellText(d.Name), cellText(d.Engine), cellText(d.BaseURL), cellText(d.AuthStyle),
					boolStr(d.Retired), fmt.Sprintf("%d", d.CurrentRevision),
					fmt.Sprintf("%d", d.SelectedUserCount), fmt.Sprintf("%d", d.AttachedKeyCount),
				})
			}
			return p.Table([]string{"NAME", "ENGINE", "URL", "AUTH", "RETIRED", "REVISION", "SELECTED", "KEYS"}, rows)
		},
	}

	get := &cobra.Command{
		Use:   "get <id|name>",
		Short: "Show one admin-shared provider definition with its model map",
		Long: "Print one admin-shared provider definition: its fields, counts and model map. The argument\n" +
			"is the provider id, or its name (case-insensitive).\n\n" +
			"Read-only; exit 4 when no shared definition has that id or name (a personal provider is\n" +
			"never shown here). Needs a uza_ (admin_ro) token.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			arg := strings.TrimSpace(args[0])
			all, err := c.AdminListSharedProviders(cmd.Context())
			if err != nil {
				return providerErr(err)
			}
			id := ""
			for _, d := range all {
				if d.ID == arg {
					id = d.ID
					break
				}
			}
			if id == "" {
				for _, d := range all {
					if strings.EqualFold(d.Name, arg) {
						id = d.ID
						break
					}
				}
			}
			if id == "" {
				return uzicli.Exitf(uzicli.ExitNotFound,
					"no admin-shared provider with id or name %q; personal providers are not shown here, `uzi admin provider list` shows the shared ones", cellText(arg))
			}
			d, err := c.AdminGetSharedProvider(cmd.Context(), id)
			if err != nil {
				return providerErr(err)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(d)
			}
			return renderAdminSharedProvider(p, d)
		},
	}

	group.AddCommand(list, get)
	return group
}

// renderAdminSharedProvider prints the human view of one definition: a FIELD/VALUE table.
func renderAdminSharedProvider(p *uzicli.Printer, d apitypes.AdminSharedProviderDTO) error {
	return p.Table([]string{"FIELD", "VALUE"}, [][]string{
		{"id", cellText(d.ID)},
		{"name", cellText(d.Name)},
		{"engine", cellText(d.Engine)},
		{"base_url", cellText(d.BaseURL)},
		{"auth_style", cellText(d.AuthStyle)},
		{"retired", boolStr(d.Retired)},
		{"revision", fmt.Sprintf("%d", d.CurrentRevision)},
		{"selected_user_count", fmt.Sprintf("%d", d.SelectedUserCount)},
		{"attached_key_count", fmt.Sprintf("%d", d.AttachedKeyCount)},
		{"models", dashOr(modelsCell(d.ModelMap))},
	})
}
