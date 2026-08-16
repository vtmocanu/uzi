package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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
				// Issue #124: `version` is WORKER SELF-REPORTED, and Printer.Table Fprintln's
				// cells through a tabwriter with no scrub of its own (uzicli/output.go) — so
				// this was the one free-text sink in the CLI not routed through sanitizeTTY,
				// while every other one is. Pre-item-6 the exposure was Cf-only, since the Cc
				// half was already stripped at ingest; ingest now strips both, and this covers
				// rows written before that. compactText is the shape the neighbouring table
				// cells already use.
				// Sanitize BEFORE the placeholder, not after: a version that is nothing but
				// format characters compacts to "" and must still read as "-", not as a blank
				// cell. (A judge could not do this, but a hostile worker holding a valid join
				// token could, and that is whose string this is.)
				//
				// `w.Name` IS sanitized too, through cellText (= compactText plus the tab fold
				// a column rail needs). This paragraph used to say the opposite — that a name
				// is deliberately left raw because `worker list` shows only the CALLER's own
				// workers, so its author and its only reader are the same person. That argument
				// holds for the SPOOFING half and does not cover the rest: worker names are
				// validated for LENGTH ONLY (handler/workers.go: TrimSpace + a 200-byte cap)
				// and workers.name is a bare `text` with no CHECK, so unlike a token label an
				// ESC is storable in one, and an embedded newline breaks the tabwriter rail for
				// every FOLLOWING row — a name can forge a table row in its owner's own output.
				// The admin/all-users mode this paragraph anticipated is not hypothetical either,
				// it is just not a FLAG on this command: `uzi admin workers` (admin.go) has
				// printed names cross-tenant since PRD #64 M7, and rendered them raw until
				// PRD #111 M5 put cellText on that cell. A sanitized cell beside an unsanitized
				// one in the same row is worse than either, because it reads as though the
				// question was considered.
				version := compactText(strOr(w.Version, ""))
				if version == "" {
					version = "-"
				}
				rows = append(rows, []string{
					w.ID, cellText(w.Name), w.Status, uptimeCell(w), version, upgradeCell(w), bindModeCell(w),
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
			return p.Table([]string{"ID", "NAME", "STATUS", "UPTIME", "VERSION", "UPGRADE", "TOKEN"}, rows)
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

// uptimeCell renders a worker's continuous-online duration for `uzi worker list`'s
// UPTIME column (PRD #251) — "how long has the online pill been green".
//
// "-" when the worker is not online or carries no anchor, mirroring upgradeCell's
// empty→"-" convention: an offline worker is not "up", and a worker whose
// online_since is nil (never stamped, or cleared by the sweeper) has no session to
// count from. The value is control-plane-owned (Decision 1), so it needs no scrub —
// unlike the version/name cells beside it, it is never worker self-reported.
func uptimeCell(w apitypes.WorkerDTO) string {
	if w.Status != "online" || w.OnlineSince == nil {
		return "-"
	}
	return formatUptimeDuration(time.Since(*w.OnlineSince))
}

// formatUptimeDuration renders a duration as "2d 4h" / "1h 23m" / "44m" / "<1m",
// the same buckets as the web's formatUptimeSince and rateLimits.formatCountdown
// (Decision 4), so uptime reads in the same vocabulary as the reset countdowns. It
// is the pure half of uptimeCell, split out so the buckets unit-test without a clock
// — uptimeCell itself calls it with time.Since. A zero or negative duration (clock
// skew) floors at "<1m".
func formatUptimeDuration(d time.Duration) string {
	total := int(d.Seconds())
	if total <= 0 {
		return "<1m"
	}
	days := total / 86_400
	hours := (total % 86_400) / 3_600
	mins := (total % 3_600) / 60
	switch {
	case days >= 1:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours >= 1:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case mins >= 1:
		return fmt.Sprintf("%dm", mins)
	default:
		return "<1m"
	}
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
