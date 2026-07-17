package main

import "github.com/spf13/cobra"

// newLoginCmd — `uzi login`. Browser-brokered, poll-based token acquisition
// (M5 server endpoints + M8 client). Stub in M3.
func newLoginCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "login",
		Short: "Log in via the browser and store a CLI token",
		Args:  cobra.NoArgs,
		RunE:  stubRunE("login"),
	}
}

// newLogoutCmd — `uzi logout`. Local-only: deletes the stored credential.
// Revoking server-side is a webui action (Decision 16). Stub in M3.
func newLogoutCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the locally stored CLI credential (does not revoke it server-side)",
		Args:  cobra.NoArgs,
		RunE:  stubRunE("logout"),
	}
}
