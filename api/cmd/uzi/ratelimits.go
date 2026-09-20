package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Provider flag values shared by `uzi rate-limits` and `uzi admin rate-limits`
// (PRD #1209 M3). The Anthropic/Claude side is the historical default so an existing
// invocation with no --provider is byte-for-byte unchanged.
const (
	providerClaude = "claude"
	providerCodex  = "codex"
)

// validateProvider gates the --provider flag to the two allowed values, returning a
// usage error (exit 2) on anything else so a typo fails loudly rather than silently
// falling through to one branch.
func validateProvider(p string) error {
	switch p {
	case providerClaude, providerCodex:
		return nil
	default:
		return uzicli.Exitf(uzicli.ExitUsage, "unknown --provider %q; want %q or %q", p, providerClaude, providerCodex)
	}
}

// newRateLimitsCmd — `uzi rate-limits [--provider claude|codex]`. The OWNER's own
// rate-limit meters, the terminal twin of the web sidebar meters. --provider selects
// the credential family (default claude); both branches render a human table plus
// --json. This is the read-only owner view; the factory-wide cross-user view is
// `uzi admin rate-limits` (also --provider-aware).
func newRateLimitsCmd(env Env, gf *globalFlags) *cobra.Command {
	var provider string
	cmd := &cobra.Command{
		Use:   "rate-limits",
		Short: "Show your own rate-limit meters (--provider claude|codex)",
		Long: "Show your own rate-limit meters for a credential family.\n\n" +
			"--provider claude (the default) lists your Anthropic tokens' 5h/7d windows;\n" +
			"--provider codex lists your linked Codex accounts' per-bucket utilization. A\n" +
			"window uzi has no reading for renders \"—\", never 0, so partial or unknown\n" +
			"state is never confused with a genuine zero.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateProvider(provider); err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if provider == providerCodex {
				return runSelfCodexRateLimits(cmd, c, p)
			}
			return runSelfRateLimits(cmd, c, p)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", providerClaude, "credential family: claude|codex")
	return cmd
}

// runSelfRateLimits renders the owner's Anthropic token meters as TOKEN/STATUS/5H%/7D%,
// mirroring the admin table's column style (minus the cross-user EMAIL/VAULT columns).
// --json passes the raw []TokenRateLimitDTO through unchanged. Reuses tokenCell/windowPct
// so the owner and admin views cannot render the same meter differently.
func runSelfRateLimits(cmd *cobra.Command, c uzicli.Client, p *uzicli.Printer) error {
	meters, err := c.SelfRateLimits(cmd.Context())
	if err != nil {
		return err
	}
	if p.Format == uzicli.FormatJSON {
		return p.JSON(meters)
	}
	rows := make([][]string, 0, len(meters))
	for _, tk := range meters {
		rows = append(rows, []string{
			tokenCell(tk.Label, tk.IsDefault),
			tk.Limits.Status,
			windowPct(tk.Limits.FiveHour),
			windowPct(tk.Limits.SevenDay),
		})
	}
	return p.Table([]string{"TOKEN", "STATUS", "5H%", "7D%"}, rows)
}

// runSelfCodexRateLimits renders the owner's Codex account meters, one row per
// (account, bucket): ACCOUNT/STATUS/BUCKET/PRIMARY/SECONDARY/RESET. An account with no
// buckets still gets a single row so its status is visible. A nil window (or one with no
// reading) renders "—", never 0. --json passes the raw []CodexAccountRateLimitDTO
// through unchanged.
func runSelfCodexRateLimits(cmd *cobra.Command, c uzicli.Client, p *uzicli.Printer) error {
	accts, err := c.SelfCodexRateLimits(cmd.Context())
	if err != nil {
		return err
	}
	if p.Format == uzicli.FormatJSON {
		return p.JSON(accts)
	}
	rows := codexRateLimitRows(accts)
	return p.Table([]string{"ACCOUNT", "STATUS", "BUCKET", "PRIMARY", "SECONDARY", "RESET"}, rows)
}

// codexRateLimitRows folds one user's Codex accounts into the per-bucket table rows shared
// by the owner and admin renderers. ACCOUNT/STATUS repeat on each of an account's bucket
// rows (matching admin rate-limits' per-token repetition); a bucket-less account is a
// single row with "-"/"—" cells so it still appears once.
func codexRateLimitRows(accts []apitypes.CodexAccountRateLimitDTO) [][]string {
	rows := make([][]string, 0, len(accts))
	for _, a := range accts {
		acct := codexAccountCell(a)
		if len(a.Buckets) == 0 {
			rows = append(rows, []string{acct, a.Status, "-", "—", "—", "—"})
			continue
		}
		for _, b := range a.Buckets {
			rows = append(rows, []string{
				acct,
				a.Status,
				codexBucketCell(b),
				codexPctCell(b.Primary),
				codexPctCell(b.Secondary),
				codexResetCell(b.Primary),
			})
		}
	}
	return rows
}

// runAdminCodexRateLimits renders the factory-wide per-user Codex meters (PRD #1209 M3),
// grouped by user: EMAIL/VAULT prefix each of a user's per-(account, bucket) rows. A user
// with no linked Codex accounts is a single row so they still appear once. --json passes
// the raw []CodexAdminRateLimitRowDTO through unchanged. Reuses codexRateLimitRows so the
// admin and owner Codex tables cannot render the same meter differently.
func runAdminCodexRateLimits(cmd *cobra.Command, c uzicli.Client, p *uzicli.Printer) error {
	users, err := c.AdminCodexRateLimits(cmd.Context())
	if err != nil {
		return err
	}
	if p.Format == uzicli.FormatJSON {
		return p.JSON(users)
	}
	rows := make([][]string, 0, len(users))
	for _, u := range users {
		email := cellText(u.Email)
		vault := vaultCell(u.VaultLocked)
		if len(u.Accounts) == 0 {
			rows = append(rows, []string{email, vault, "-", "-", "-", "—", "—", "—"})
			continue
		}
		for _, acctRow := range codexRateLimitRows(u.Accounts) {
			rows = append(rows, append([]string{email, vault}, acctRow...))
		}
	}
	return p.Table([]string{"EMAIL", "VAULT", "ACCOUNT", "STATUS", "BUCKET", "PRIMARY", "SECONDARY", "RESET"}, rows)
}

// codexAccountCell renders a Codex account's identity for the table: its aliases joined,
// default-badged, or the account id when it carries no alias. The aliases are
// user-authored, so cellText sanitizes them at the render boundary.
func codexAccountCell(a apitypes.CodexAccountRateLimitDTO) string {
	label := strings.Join(a.Aliases, ", ")
	if strings.TrimSpace(label) == "" {
		label = a.AccountID
	}
	if a.IsDefault {
		return cellText(label) + " (default)"
	}
	return cellText(label)
}

// codexBucketCell renders a bucket's human label — its display name, or its stable id
// when it carries none. The display name is provider-authored free text, so cellText
// sanitizes it.
func codexBucketCell(b apitypes.CodexRateLimitBucketDTO) string {
	if strings.TrimSpace(b.DisplayName) != "" {
		return cellText(b.DisplayName)
	}
	return cellText(b.ID)
}

// codexPctCell renders a Codex window's utilization as NN%, or "—" when the window is
// absent or carries no reading — never 0, so partial/unknown state stays distinct from a
// genuine zero utilization.
func codexPctCell(w *apitypes.CodexRateLimitWindowDTO) string {
	if w == nil || w.UsedPercent == nil {
		return "—"
	}
	return fmt.Sprintf("%d%%", int(*w.UsedPercent+0.5))
}

// codexResetCell renders a Codex window's reset timing as a short duration until reset,
// preferring the seconds-until form and falling back to the absolute epoch. "—" when the
// window is absent or reported no reset. shortDuration (shared with the TUI meters)
// clamps a past reset to "0s".
func codexResetCell(w *apitypes.CodexRateLimitWindowDTO) string {
	if w == nil {
		return "—"
	}
	if w.ResetAfterSeconds != nil {
		return shortDuration(time.Duration(*w.ResetAfterSeconds) * time.Second)
	}
	if w.ResetAt != nil {
		return shortDuration(time.Duration(*w.ResetAt-time.Now().Unix()) * time.Second)
	}
	return "—"
}
