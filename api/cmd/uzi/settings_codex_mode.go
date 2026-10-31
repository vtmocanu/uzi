package main

import (
	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newSettingsCodexModeCmd is `uzi settings codex-mode default|auto` (issue #3719): choose
// whether a new Codex run binds your default login or picks among the logins you pooled
// with `uzi token pool <label> --kind codex --on`.
func newSettingsCodexModeCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "codex-mode default|auto",
		Short: "Set how new Codex runs choose a login",
		Long: "Set how a new Codex run chooses its login. default binds your default login, as\n" +
			"before. auto picks, at run creation, the pooled login with the most limit headroom\n" +
			"(pool a login with `uzi token pool <label> --kind codex --on`; the pool is empty\n" +
			"until you opt logins in, and auto then falls back to your default).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := args[0]
			if mode != "default" && mode != "auto" {
				return uzicli.Exitf(uzicli.ExitUsage, "invalid mode %q; use default or auto", mode)
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			stored, err := c.SetCodexAccountMode(cmd.Context(), mode)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"codex_account_mode": stored})
			}
			p.Println(codexModeLine(stored))
			return nil
		},
	}
}

// codexModeLine renders the Codex account mode for `settings get` and `settings codex-mode`.
// An empty mode (a server that predates the setting) reads as default, its only behaviour.
func codexModeLine(mode string) string {
	if mode == "" {
		mode = "default"
	}
	return "Codex account mode: " + cellText(mode)
}

func settingsCodexModeLine(s apitypes.UserSettingsDTO) string {
	return codexModeLine(s.CodexAccountMode)
}
