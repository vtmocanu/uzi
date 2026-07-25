package main

import (
	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// newWorkerCmd — `uzi worker`. list and rm are both wired. There is no
// `create`: minting a join token is a webui action (it returns a credential
// that reads decrypted secrets), so it stays cookie-only (Decision 18).
func newWorkerCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "List and remove workers",
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "List workers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			workers, err := c.ListWorkers(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(workers)
			}
			rows := make([][]string, 0, len(workers))
			for _, w := range workers {
				rows = append(rows, []string{w.ID, w.Name, w.Status, strOr(w.Version, "-")})
			}
			// VERSION is here because docs/run-auto-stopped.md's first remedy for an
			// auto-stopped run is "check the worker's version" — v0.10.1+ isolates a
			// poisoned message itself, so an upgrade is the real fix and the version is
			// the first thing an operator needs. The web has rendered it since PRD #42
			// (WorkersSettings.tsx); the CLI is a first-class second consumer and did
			// not, so the doc shipped a remedy one of its two audiences could not
			// follow. "-" when a worker has never registered a version.
			return p.Table([]string{"ID", "NAME", "STATUS", "VERSION"}, rows)
		},
	}

	rm := &cobra.Command{
		Use:   "rm <worker-id>",
		Short: "Remove a worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			if err := c.DeleteWorker(cmd.Context(), args[0]); err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"id": args[0], "deleted": true})
			}
			if !gf.quiet {
				p.Printf("worker %s removed\n", args[0])
			}
			return nil
		},
	}

	// set-token points a worker at one of the caller's named Anthropic tokens
	// (PRD #104 M3). Unlike `create`, this mints nothing and hands back no
	// credential — it re-points a worker between tokens the caller already owns —
	// so it is reachable from a CLI token (D8).
	//
	// `--default` and a label are mutually exclusive, and one of them is required:
	// `uzi worker set-token <id>` with neither would be ambiguous between "clear the
	// binding" and "show me the binding", and silently picking either is worse than
	// asking.
	var toDefault bool
	setToken := &cobra.Command{
		Use:   "set-token <worker-id> [label]",
		Short: "Point a worker at one of your Anthropic tokens (or --default)",
		Long: "Bind a worker to a named Anthropic token, so its runs spend that\n" +
			"credential instead of your default one. Pass --default to clear the\n" +
			"binding and fall back to your default token.\n\n" +
			"Takes effect on the worker's next claim: no restart, no new join token.\n" +
			"Chat runs on a bound worker still spend your default token.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			label := ""
			if len(args) == 2 {
				label = args[1]
			}
			switch {
			case toDefault && label != "":
				return uzicli.Exitf(uzicli.ExitUsage, "pass either a token label or --default, not both")
			case !toDefault && label == "":
				return uzicli.Exitf(uzicli.ExitUsage, "pass a token label, or --default to clear the binding")
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			wkr, err := c.SetWorkerToken(cmd.Context(), args[0], label)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(wkr)
			}
			if !gf.quiet {
				if label == "" {
					p.Printf("worker %s now uses your default Anthropic token\n", args[0])
				} else {
					p.Printf("worker %s now uses Anthropic token %q\n", args[0], label)
				}
			}
			return nil
		},
	}
	setToken.Flags().BoolVar(&toDefault, "default", false, "clear the binding; use the account default token")

	cmd.AddCommand(list, rm, setToken)
	return cmd
}
