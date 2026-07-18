package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

func newVersionCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the uzi CLI version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]string{"version": version})
			}
			fmt.Fprintln(env.Stdout, version)
			return nil
		},
	}
}
