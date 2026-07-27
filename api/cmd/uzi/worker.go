package main

import (
	"strings"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
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
				rows = append(rows, []string{
					w.ID, w.Name, w.Status, strOr(w.Version, "-"), upgradeCell(w), bindModeCell(w),
				})
			}
			// VERSION is here because docs/run-auto-stopped.md's first remedy for an
			// auto-stopped run is "check the worker's version" — v0.10.1+ isolates a
			// poisoned message itself, so an upgrade is the real fix and the version is
			// the first thing an operator needs. The web has rendered it since PRD #42
			// (WorkersSettings.tsx); the CLI is a first-class second consumer and did
			// not, so the doc shipped a remedy one of its two audiences could not
			// follow. "-" when a worker has never registered a version.
			// TOKEN is PRD #111 M5, and it closes a CLI-parity hole M3 opened: the CLI
			// gained a WRITE (`worker set-token --auto`) with no human-readable READ, so
			// the only way to confirm what a worker was set to was `--json`. A three-way
			// user choice you can set and cannot see is worse than one you cannot set.
			return p.Table([]string{"ID", "NAME", "STATUS", "VERSION", "UPGRADE", "TOKEN"}, rows)
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

	// set-token sets HOW a worker chooses its Anthropic credential (PRD #104 M3,
	// widened by PRD #111 M3). Unlike `create`, this mints nothing and hands back no
	// credential — it re-points a worker between tokens the caller already owns — so
	// it is reachable from a CLI token (D8).
	//
	// Exactly ONE of a label, --default or --auto, and one of them is required:
	// `uzi worker set-token <id>` with none would be ambiguous between "clear the
	// binding" and "show me the binding", and silently picking either is worse than
	// asking. The command keeps its name rather than becoming `set-bind-mode`,
	// because a rename would break every script that already calls it and the verb
	// still describes what a user is doing — choosing which token pays.
	var toDefault, toAuto bool
	setToken := &cobra.Command{
		Use:   "set-token <worker-id> [label]",
		Short: "Choose how a worker picks its Anthropic token: a label, --default, or --auto",
		Long: "Choose which Anthropic credential a worker's runs spend.\n\n" +
			"  <label>     pin the worker to that named token\n" +
			"  --default   use your default token\n" +
			"  --auto      let uzi pick per claim, from the tokens you opted into the\n" +
			"              pool with `uzi token pool` — preferring the account with the\n" +
			"              most rate-limit headroom\n\n" +
			"With --auto and an empty or unreadable pool the worker simply uses your\n" +
			"default token; auto never fails a run for want of a candidate.\n\n" +
			"Takes effect on the worker's next claim: no restart, no new join token.\n" +
			"Chat runs still spend your default token whatever the mode.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			label := ""
			if len(args) == 2 {
				label = args[1]
			}
			// Counted rather than checked pairwise: with three choices, pairwise
			// conditions grow quadratically and the one that gets forgotten is always
			// the pair added last.
			chosen := 0
			for _, on := range []bool{label != "", toDefault, toAuto} {
				if on {
					chosen++
				}
			}
			if chosen != 1 {
				return uzicli.Exitf(uzicli.ExitUsage,
					"pass exactly one of: a token label, --default, or --auto")
			}
			mode := "pinned"
			switch {
			case toDefault:
				mode = "default"
			case toAuto:
				mode = "auto"
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			wkr, err := c.SetWorkerBindMode(cmd.Context(), args[0], mode, label)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(wkr)
			}
			if !gf.quiet {
				switch mode {
				case "auto":
					p.Printf("worker %s now auto-selects from your Anthropic token pool\n", args[0])
				case "default":
					p.Printf("worker %s now uses your default Anthropic token\n", args[0])
				default:
					// The label is user-authored; cellText is what every other
					// user-authored cell in this binary goes through.
					p.Printf("worker %s now uses Anthropic token %q\n", args[0], cellText(label))
				}
			}
			return nil
		},
	}
	setToken.Flags().BoolVar(&toAuto, "auto", false, "auto-select per claim from your opted-in token pool")
	setToken.Flags().BoolVar(&toDefault, "default", false, "clear the binding; use the account default token")

	cmd.AddCommand(list, rm, setToken)
	return cmd
}

// upgradeCell renders a worker's upgrade status for the table (PRD #113 M5).
//
// Words, not symbols: this column is read by operators and by agents parsing --json's
// sibling, and the CLI's other columns are plain words too. The api's five-state enum is
// passed through as-is rather than re-mapped, so the CLI and the web badge can never
// describe the same worker differently — a second vocabulary is a second thing to keep in
// sync.
//
// "unknown" renders as "-" and not as the word: an unstamped local build, an unparseable
// report and a `dev` control plane all classify unknown, and none of them is a finding.
// Rendering it as text would put a column of "unknown" in front of every developer running
// a local stack and train them to ignore this column.
func upgradeCell(w apitypes.WorkerDTO) string {
	switch w.UpgradeStatus {
	case "", "unknown":
		return "-"
	case "up_to_date":
		return "up to date"
	case "upgrade_failed":
		return "FAILED"
	default:
		// outdated | upgrading, and any state a newer api adds. Passing an unrecognized
		// value through is deliberate: a newer server's new state should read as itself,
		// not as "-" hiding a state this build has no opinion about.
		return strings.ReplaceAll(w.UpgradeStatus, "_", " ")
	}
}

// bindModeCell renders HOW a worker chooses its Anthropic credential, for
// `uzi worker list`'s TOKEN column (PRD #111 M5).
//
// The three modes need three different renderings because they answer different
// questions, and only one of them has a name to show:
//
//	default  →  "default"        the owner's default token; no binding
//	pinned   →  "<label>"        the credential itself, which is the useful fact
//	auto     →  "auto"           chosen per claim from the pool; no fixed answer
//
// A pinned worker prints its LABEL rather than the word "pinned" because the label
// is what the user set and what `uzi worker set-token` takes back. The other two have
// no label to print: `default` resolves at claim time and `auto` resolves differently
// on every claim, so naming a token there would be a snapshot presented as a setting.
//
// The server reports the EFFECTIVE mode (handler's effectiveBindMode), so `pinned`
// always arrives with an id and a label beside it — D9's pinned-with-a-deleted-token
// case has already been mapped to `default` upstream. The nil guard below is
// therefore belt-and-braces against a DTO that contradicts itself, not a case the
// current server can produce; it renders "default" because that is what such a
// worker would actually spend.
//
// An UNRECOGNISED mode prints as itself, for the reason every other renderer in this
// PRD does: the CLI is versioned separately from the API.
func bindModeCell(w apitypes.WorkerDTO) string {
	switch w.AnthropicBindMode {
	case "default":
		return "default"
	case "auto":
		return "auto"
	case "pinned":
		if w.AnthropicSecretLabel == nil || *w.AnthropicSecretLabel == "" {
			return "default"
		}
		// User-authored text into a table cell: cellText folds newlines and tabs,
		// which would otherwise break the column rail, and caps the length.
		return cellText(*w.AnthropicSecretLabel)
	}
	return w.AnthropicBindMode
}
