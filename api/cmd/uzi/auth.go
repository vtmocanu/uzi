package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// newAuthCmd — `uzi auth` groups credential-plumbing verbs.
func newAuthCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Manage the stored CLI credential",
	}

	// `uzi auth token` — read a static token from stdin (piped) or prompt (TTY)
	// and store it. There is deliberately no --token flag: a credential must
	// never land on argv (PRD #64). Stub in M3 (M7 implements the stdin read).
	token := &cobra.Command{
		Use:   "token",
		Short: "Store a static CLI token read from stdin (or a prompt on a TTY)",
		Args:  cobra.NoArgs,
		RunE:  stubRunE("auth token"),
	}
	token.Flags().Bool("with-token", false, "read the token from stdin even on a TTY")

	// `uzi auth status` — report whether a credential is stored locally. Stub in M3.
	status := &cobra.Command{
		Use:   "status",
		Short: "Show whether a credential is stored for the current context",
		Args:  cobra.NoArgs,
		RunE:  stubRunE("auth status"),
	}

	cmd.AddCommand(token, status)
	return cmd
}

// newWhoamiCmd — `uzi whoami`. Reports the identity and effective scope of the
// current credential (GET /api/auth/me): is_admin is false for a uzc_ token,
// true only for a uza_ token. Wired to the Client interface.
func newWhoamiCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the identity and scope of the current credential",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			u, err := c.Whoami(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(u)
			}
			return p.Table(
				[]string{"ID", "EMAIL", "ADMIN"},
				[][]string{{u.ID, u.Email, fmt.Sprintf("%t", u.IsAdmin)}},
			)
		},
	}
}
