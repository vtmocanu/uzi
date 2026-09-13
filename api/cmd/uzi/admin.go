package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newAdminCmd — `uzi admin`. Read-only by construction: the admin write
// endpoints are cookie-only, so there is nothing for the CLI to call. Needs a
// uza_ (admin_ro) token; a 403 from the scope gate maps to exit 3 with an
// actionable message (statusError in the client).
func newAdminCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Read-only admin views (requires an admin-scoped token)",
	}

	users := &cobra.Command{
		Use:   "users",
		Short: "List all users",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			us, err := c.AdminListUsers(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(us)
			}
			rows := make([][]string, 0, len(us))
			for _, u := range us {
				rows = append(rows, []string{u.ID, u.Email, boolStr(u.IsAdmin)})
			}
			return p.Table([]string{"ID", "EMAIL", "ADMIN"}, rows)
		},
	}

	runs := &cobra.Command{
		Use:   "runs",
		Short: "List all runs across the factory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			rs, err := c.AdminListRuns(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(rs)
			}
			rows := make([][]string, 0, len(rs))
			for _, r := range rs {
				rows = append(rows, []string{r.ID, strOr(r.OwnerEmail, "-"), r.Kind, r.TriggerSource, effectiveRunStatus(r.Status, r.IsPlanning, r.IsRevising), runTitle(r.RunDTO)})
			}
			return p.Table([]string{"ID", "OWNER", "KIND", "TRIGGER", "STATUS", "TITLE"}, rows)
		},
	}

	workers := &cobra.Command{
		Use:   "workers",
		Short: "List all workers across the factory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			ws, err := c.AdminListWorkers(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(ws)
			}
			rows := make([][]string, 0, len(ws))
			for _, w := range ws {
				// 🔴 THE CROSS-TENANT ONE. This prints another user's worker name into an
				// ADMIN's terminal, and worker names are validated for length only — no
				// control-character check, no Cf check, and workers.name carries no CHECK.
				// So a crafted name is terminal control injection into someone else's
				// session, and an embedded newline forges a row in a table an admin reads
				// to make decisions. Strictly worse than the token-label case, where the
				// validator had already put ANSI out of reach.
				//
				// The render site is the trust boundary and does not depend on the
				// validator holding; hardening the validator is a separate change.
				//
				// 🔴 THE SIGNPOST THIS COMMENT USED TO CARRY IS DISCHARGED (#180). It read
				// "`uzi admin cli-tokens` below renders `t.Name` raw ... it goes up as a
				// follow-up", and that is no longer true of this file: uzicli.Printer.Table
				// now runs CellText over every header and every cell, so the cli-tokens
				// table below — and every other table in the CLI — is sanitized at the
				// render boundary whether or not its call site remembers.
				//
				// The explicit cellText here STAYS, and not out of caution. It states at the
				// call site that this value is untrusted, which the boundary cannot say; it
				// is idempotent, so it costs nothing; and a per-cell call survives a
				// refactor that stops routing through Printer.Table, which the direct
				// writers in version.go and root.go are the standing proof of.
				//
				// #169's OTHER half is untouched and still open: worker names have no
				// validator at all (handler/workers.go checks length only, and workers.name
				// carries no CHECK), so ESC remains STORABLE in one. That is a behaviour
				// change to a shipped endpoint and belongs in its own MR.
				rows = append(rows, []string{w.ID, w.OwnerEmail, cellText(w.Name), w.Status})
			}
			return p.Table([]string{"ID", "OWNER", "NAME", "STATUS"}, rows)
		},
	}

	usage := &cobra.Command{
		Use:   "usage",
		Short: "Show token/cost usage across the factory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			u, err := c.AdminUsage(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(u)
			}
			return renderAdminUsage(p, u)
		},
	}

	rateLimits := &cobra.Command{
		Use:   "rate-limits",
		Short: "Show Claude rate-limit meters per user",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			rls, err := c.AdminRateLimits(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(rls)
			}
			// One table row per (user, token) since #104 M5: the table gains a TOKEN
			// column, and a token-less user is a single row with an empty token cell and
			// a no_token status — so every user still appears exactly once when they hold
			// no token, and once per token when they hold several.
			rows := make([][]string, 0, len(rls))
			for _, rl := range rls {
				if len(rl.Tokens) == 0 {
					rows = append(rows, []string{
						rl.Email, vaultCell(rl.VaultLocked), "-", "no_token", "-", "-",
					})
					continue
				}
				for _, tk := range rl.Tokens {
					rows = append(rows, []string{
						rl.Email,
						vaultCell(rl.VaultLocked),
						tokenCell(tk.Label, tk.IsDefault),
						tk.Limits.Status,
						windowPct(tk.Limits.FiveHour),
						windowPct(tk.Limits.SevenDay),
					})
				}
			}
			return p.Table([]string{"EMAIL", "VAULT", "TOKEN", "STATUS", "5H%", "7D%"}, rows)
		},
	}

	cliTokens := &cobra.Command{
		Use:   "cli-tokens",
		Short: "List every CLI token in the factory (metadata only)",
		Long: "List every CLI token in the factory with its owner, so an operator can " +
			"answer who holds standing credentials to this instance and whether any are " +
			"stale or unexpected.\n\n" +
			"Never prints a token value or its hash: the value is not stored at all, and " +
			"the hash is excluded by the query's projection. USED is the coarse (<=1/min) " +
			"last-use stamp and, with the prefix, the only forensic signal there is -- " +
			"there is no per-request audit log. A blank EXPIRES is a never-expiring " +
			"user-scope token, which is the row most worth looking at.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			ts, err := c.AdminListCLITokens(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(ts)
			}
			rows := make([][]string, 0, len(ts))
			for _, t := range ts {
				rows = append(rows, []string{
					t.OwnerEmail,
					t.TokenPrefix,
					t.Name,
					t.Scope,
					tokenStateCell(t),
					tsCell(t.LastUsedAt),
					tsCell(t.ExpiresAt),
				})
			}
			return p.Table([]string{"OWNER", "PREFIX", "NAME", "SCOPE", "STATE", "USED", "EXPIRES"}, rows)
		},
	}

	guardrailImpact := &cobra.Command{
		Use:   "guardrail-impact",
		Short: "Pre-flight count of repos the push/merge guardrail would refuse",
		Long: "Run a LIVE, read-only scan of every enabled repo across the factory and " +
			"report how many would be refused under the PRD #66 guardrail (the bot can " +
			"push or merge to the default branch).\n\n" +
			"It persists nothing: it re-checks the forge on each call rather than reading " +
			"the stored privilege report, so it reflects the forge as it is right now. " +
			"UNEVALUABLE repos are counted separately and are NOT safe: a forge error, or " +
			"a repo with no default branch, means uzi could not tell — treat it as unknown, " +
			"not as zero affected.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			rep, err := c.GuardrailImpact(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(rep)
			}
			rows := make([][]string, 0, len(rep.Repos))
			for _, r := range rep.Repos {
				rows = append(rows, []string{cellText(r.Path), boolStr(r.Blocked), boolStr(r.Unevaluable)})
			}
			if err := p.Table([]string{"PATH", "BLOCKED", "UNEVALUABLE"}, rows); err != nil {
				return err
			}
			p.Printf("enabled=%d blocked=%d unevaluable=%d\n",
				rep.EnabledRepoCount, rep.BlockedCount, rep.UnevaluableCount)
			return nil
		},
	}

	blockedRepos := &cobra.Command{
		Use:   "blocked-repos",
		Short: "Cross-user list of repos the guardrail blocks or an admin has allowed",
		Long: "List every user's repos that the push/merge guardrail refuses right now, " +
			"OR that an admin has explicitly allowed (PRD #66 D8). It reads the STORED " +
			"privilege report (cheap, display-appropriate) rather than re-sweeping the " +
			"forge — so unlike `guardrail-impact`, a repo whose connection was never " +
			"checked (UZI_PRIVILEGE_CHECK_INTERVAL=0) is INVISIBLE here. When that is the " +
			"case a warning is printed and CHECKS_UNKNOWN is true: an empty list then means " +
			"\"unknown\", not \"none blocked\".",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			rep, err := c.AdminBlockedRepos(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(rep)
			}
			rows := make([][]string, 0, len(rep.Repos))
			for _, r := range rep.Repos {
				allowed := "—"
				if r.Override != nil {
					allowed = r.Override.By
				}
				rows = append(rows, []string{cellText(r.OwnerEmail), cellText(r.Path), boolStr(r.Blocked), cellText(allowed)})
			}
			if err := p.Table([]string{"OWNER", "PATH", "BLOCKED", "ALLOWED BY"}, rows); err != nil {
				return err
			}
			if rep.ChecksUnknown {
				p.Printf("note: at least one connection was never privilege-checked — this list may be incomplete (empty is \"unknown\", not \"none blocked\").\n")
			}
			return nil
		},
	}

	cmd.AddCommand(users, runs, workers, usage, rateLimits, cliTokens, guardrailImpact, blockedRepos, newAdminAgentSourceCmd(env, gf), newAdminReviewCmd(env, gf))
	return cmd
}

// newAdminReviewCmd — `uzi admin review`. A container group (no RunE) of two READ-ONLY
// leaves onto the admin "All users" judge aggregate (PRD #1184 M5): `backlog` reads GET
// /admin/judge/recommendations and `stats` reads GET /admin/judge/stats. Both mount in the
// admin READ group server-side (RequireUser + RequireAdminRO), so a uza_ (admin_ro) token
// reads them; a masked uzc_/non-admin session is a 403 (exit 3).
//
// There is deliberately NO write verb here — the cross-user Mark done / Undo are cookie-only
// (§379's read/write split), so there is nothing for the CLI to call — and NO --run flag on
// backlog: the admin path has no ?run= anchor, because an anchor names a run and this view
// hides attribution. Rendering shows only the aggregate counts ("K users · M runs · N open")
// and never a per-run/occurrence line, so no owner, run id or run title reaches the terminal.
func newAdminReviewCmd(env Env, gf *globalFlags) *cobra.Command {
	group := &cobra.Command{
		Use:   "review",
		Short: "Read-only admin \"All users\" judge aggregate (attribution hidden)",
	}

	backlog := &cobra.Command{
		Use:   "backlog",
		Short: "List every user's judge recommendations, deduped and attribution-hidden",
		Long: "List every recommendation across ALL users' runs, deduped by (category, target),\n" +
			"with attribution hidden: each group reads \"K users · M runs · N open\" and there\n" +
			"are NO per-run or occurrence lines — no owner, run id or run title is shown. This is\n" +
			"the terminal form of the Judge page's admin \"All users\" view. Needs a uza_\n" +
			"(admin_ro) token; there is no --run anchor (an anchor names a run).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runAdminReviewBacklog(env, gf, c, cmd)
		},
	}
	// Reuse the owner backlog's help text verbatim (the ONE place the CLI writes each set
	// down), and forward both values verbatim: the server owns the validators, so an unknown
	// bucket/category is its own 400 → the usage exit code, never a silently empty list.
	backlog.Flags().String("bucket", "", backlogBucketFlagUsage)
	backlog.Flags().String("category", "", backlogCategoryFlagUsage)

	stats := &cobra.Command{
		Use:   "stats",
		Short: "Show the \"All users\" judge triage totals across every user's runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			t, err := c.AdminJudgeStats(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(t)
			}
			rows := [][]string{
				{"TOTAL", fmt.Sprintf("%d", t.Total)},
				{"TO DO", fmt.Sprintf("%d", t.Todo)},
				{"FILED", fmt.Sprintf("%d", t.Filed)},
				{"DONE", fmt.Sprintf("%d", t.Done)},
				{"DISMISSED", fmt.Sprintf("%d", t.Dismissed)},
				{"FALSE POSITIVES", fmt.Sprintf("%d", t.FalsePositives)},
			}
			return p.Table(nil, rows)
		},
	}

	group.AddCommand(backlog, stats)
	return group
}

// runAdminReviewBacklog fetches and renders the admin "All users" aggregate. --bucket and
// --category are forwarded verbatim (empty omits the parameter, so the server's default
// applies — "todo" for bucket, "all labels" for category); an unknown value is the server's
// 400, never a silent empty list. --json passes the server's envelope through unchanged, so an
// agent sees `truncated` and the canonical `triage` alongside the attribution-hidden groups.
func runAdminReviewBacklog(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command) error {
	bucket, _ := cmd.Flags().GetString("bucket")
	category, _ := cmd.Flags().GetString("category")
	b, err := c.AdminJudgeBacklog(cmd.Context(), strings.TrimSpace(bucket), strings.TrimSpace(category))
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(b)
	}
	return renderAdminBacklog(p, b)
}

// renderAdminBacklog is the human view of the admin "All users" aggregate: the canonical
// triage line, then one block per group showing ONLY the aggregate counts
// ("K users · M runs · N open") and the rationale preview. It deliberately prints NO per-run
// or occurrence line — no owner, run id or run title — which is the CLI half of the
// four-layer attribution hiding (§379); the DTO carries none of those fields to leak.
//
// The tally comes from the response's Triage, which the server sources from the separate
// all-users stats query rather than tallying off these rows, so it stays correct under both
// the bucket filter and truncation. Do NOT recompute it from the groups on screen.
func renderAdminBacklog(p *uzicli.Printer, b apitypes.JudgeAdminBacklogDTO) error {
	p.Println(triageLine(b.Triage))
	if b.Truncated {
		// The cap bounds ROWS and applies BEFORE grouping, so a surviving group's counts can
		// be understated and a group whose only open occurrence fell outside the cut is missing
		// from a todo view entirely. A MISSING group is UNKNOWN, not settled. Unlike the owner
		// backlog there is no --run remedy — the admin path has no anchor — so the warning only
		// states the caveat.
		p.Println("warning: backlog truncated at the server's row cap — counts below may be understated and groups may be missing; a missing group is UNKNOWN, not settled")
	}
	if len(b.Groups) == 0 {
		p.Println("no recommendations in this bucket")
		return nil
	}
	p.Printf("groups (%d):\n", len(b.Groups))
	for _, g := range b.Groups {
		// Target and the rationale preview are attacker-influencable free text rendered into a
		// terminal, so both go through sanitizeTTY — the same treatment the owner backlog gives.
		// g.Category is CHECK-constrained to the closed taxonomy and bucket is a closed server
		// enum, so both print raw, matching renderBacklog's considered exception.
		p.Printf("- [%s] %s → %s · %s\n",
			g.Bucket, g.Category, sanitizeTTY(g.Target), adminGroupCountsPhrase(g.UserCount, g.RunCount, g.OpenCount))
		if r := sanitizeTTY(strings.TrimSpace(g.RationalePreview)); r != "" {
			for _, line := range strings.Split(r, "\n") {
				p.Printf("    %s\n", line)
			}
		}
	}
	return nil
}

// adminGroupCountsPhrase renders an admin aggregate group's evidence chip:
// "K users · M runs · N open" (PRD #1184 M5), the cross-user cousin of the owner backlog's
// openOfRunsPhrase. The user count leads because how many distinct people hit a pattern is the
// strongest cross-user priority signal (the backlog ranks by it). The user and run nouns
// singularise at 1; "open" is a bare count, matching the aggregate's spoken form.
func adminGroupCountsPhrase(users, runs, open int) string {
	return usersPhrase(users) + " · " + runsPhrase(runs) + " · " + fmt.Sprintf("%d open", open)
}

// usersPhrase renders the distinct-user evidence chip, singular at 1. The admin aggregate's
// answer to runsPhrase.
func usersPhrase(n int) string {
	if n == 1 {
		return "1 user"
	}
	return fmt.Sprintf("%d users", n)
}

// newAdminAgentSourceCmd — `uzi admin agent-source`. A container group (no RunE) of
// two READ-ONLY leaves that read GET /admin/agent-source (PRD #602 M6). The writes —
// "Sync now" and approve-and-apply — stay web-only per ADR 0602 (cookie-only admin),
// so there is deliberately no sync/apply/config verb here: a uza_ (admin_ro) token
// can read the config and status, nothing more.
func newAdminAgentSourceCmd(env Env, gf *globalFlags) *cobra.Command {
	group := &cobra.Command{
		Use:   "agent-source",
		Short: "Read-only agent-source config + sync status (writes are web-only)",
	}

	get := &cobra.Command{
		Use:   "get",
		Short: "Show the agent-source config (repo, ref, folder, enabled, interval, credential set?)",
		Long: "Print the agent-source configuration: the source repo URL, ref, and folder " +
			"(the repo-relative subfolder role files are read from), whether the interval " +
			"puller is enabled, the poll interval, and whether a fetch credential is " +
			"configured.\n\n" +
			"Never prints the credential value — only whether one is set. Read-only: this " +
			"does not trigger a fetch. Changing the config is web-only (ADR 0602).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			as, err := c.AdminAgentSource(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			cfg := as.Config
			if p.Format == uzicli.FormatJSON {
				return p.JSON(cfg)
			}
			rows := [][]string{
				{"url", cellText(cfg.URL)},
				{"ref", cellText(cfg.Ref)},
				{"folder", cellText(cfg.Folder)},
				{"enabled", boolStr(cfg.Enabled)},
				{"interval", cfg.Interval},
				{"credential_configured", boolStr(cfg.CredentialConfigured)},
			}
			return p.Table([]string{"FIELD", "VALUE"}, rows)
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the agent-source sync status (last sync/apply, staged counts, pending)",
		Long: "Print the agent-source sync status: the last fetch's time/sha/status/error, " +
			"the last apply's time/sha, the staged snapshot's counts, and whether a staged " +
			"snapshot is pending review.\n\n" +
			"Read-only: this does not trigger a sync. Approving and applying a staged " +
			"snapshot is web-only (ADR 0602).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			as, err := c.AdminAgentSource(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				// Dump status + the staged snapshot's pending/counts so `--json` carries the
				// whole read surface (Staged is nil when nothing has been staged yet).
				return p.JSON(struct {
					Status apitypes.AgentSourceStatusDTO  `json:"status"`
					Staged *apitypes.AgentSourceStagedDTO `json:"staged"`
				}{Status: as.Status, Staged: as.Staged})
			}
			st := as.Status
			rows := [][]string{
				{"last_sync_at", dashOr(st.LastSyncAt)},
				{"last_sync_sha", dashOr(st.LastSyncSHA)},
				{"last_sync_status", dashOr(st.LastSyncStatus)},
				// PAT-scrubbed + display-sanitized server-side; cellText defensively here.
				{"last_sync_error", dashOr(cellText(st.LastSyncError))},
				{"last_applied_at", dashOr(st.LastAppliedAt)},
				{"last_applied_sha", dashOr(st.LastAppliedSHA)},
				{"staged", stagedCountCell(as.Staged, func(c apitypes.AgentSourceCountsDTO) int { return c.Staged })},
				{"changed", stagedCountCell(as.Staged, func(c apitypes.AgentSourceCountsDTO) int { return c.Changed })},
				{"failed", stagedCountCell(as.Staged, func(c apitypes.AgentSourceCountsDTO) int { return c.Failed })},
				{"pending", boolStr(as.Staged != nil && as.Staged.Pending)},
				// Derived server-side (PRD #702 M4, Decision 6); the CLI does no egress.
				{"update_available", boolStr(st.UpdateAvailable)},
			}
			if st.LatestRef != "" {
				rows = append(rows, []string{"latest_ref", cellText(st.LatestRef)})
			}
			if st.UpdateCheckedAt != "" {
				rows = append(rows, []string{"update_checked_at", cellText(st.UpdateCheckedAt)})
			}
			return p.Table([]string{"FIELD", "VALUE"}, rows)
		},
	}

	group.AddCommand(get, status)
	return group
}

// dashOr renders an empty string as "-" so an absent status field reads as "not set"
// rather than a blank cell.
func dashOr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// stagedCountCell reads one count off the staged snapshot, or "-" when nothing is
// staged (the snapshot is nil until the first fetch stages one).
func stagedCountCell(staged *apitypes.AgentSourceStagedDTO, pick func(apitypes.AgentSourceCountsDTO) int) string {
	if staged == nil {
		return "-"
	}
	return fmt.Sprintf("%d", pick(staged.Counts))
}

// renderAdminUsage prints the factory lifetime totals plus the per-user breakdown.
func renderAdminUsage(p *uzicli.Printer, u apitypes.AdminUsageDTO) error {
	lt := u.Factory.Lifetime
	p.Printf("factory (lifetime): input=%d cache_read=%d cache_creation=%d output=%d cost=$%.2f (runs=%d)\n",
		lt.InputTokens, lt.CacheReadTokens, lt.CacheCreationTokens, lt.OutputTokens, lt.CostUSD, u.Factory.RunCount)
	if len(u.Users) == 0 {
		return nil
	}
	p.Println()
	rows := make([][]string, 0, len(u.Users))
	for _, row := range u.Users {
		rows = append(rows, []string{
			row.Email,
			fmt.Sprintf("%d", row.Usage.InputTokens),
			fmt.Sprintf("%d", row.Usage.OutputTokens),
			fmt.Sprintf("$%.2f", row.Usage.CostUSD),
			fmt.Sprintf("%d", row.RunCount),
		})
	}
	return p.Table([]string{"EMAIL", "INPUT", "OUTPUT", "COST", "RUNS"}, rows)
}

func vaultCell(locked bool) string {
	if locked {
		return "locked"
	}
	return "unlocked"
}

// tokenCell renders a token's label for the admin table, marking the default so an
// operator can see at a glance which credential a user's unbound workers spend
// against (#104 M5). The label is a name, never the credential.
//
// 🔴 THE ONLY PATH WHERE A CRAFTED LABEL REACHES SOMEONE OTHER THAN ITS AUTHOR, which
// is why it goes through cellText even though every other admin cell is
// server-controlled. Everywhere else a hostile label can only spoof its own owner's
// terminal; here it renders in an ADMIN's, listing every user's credentials.
//
// PRD #111 M2's validateSecretLabel now rejects Cf on write, which narrows this
// without closing it: rows written before that landed were never re-validated, and
// nothing re-validates on read. The auditor enumerated every write path that sets
// user_secrets.label — the two body-driven ones validate, and UpsertDefaultUserSecret
// and the seed pass compiled-in constants — so HISTORY is precisely the remaining
// route, and a render-site defense is what covers history and future drift alike.
func tokenCell(label string, isDefault bool) string {
	if isDefault {
		return cellText(label) + " (default)"
	}
	return cellText(label)
}

// windowPct renders a rate-limit window's percentage, or "-" when the window is
// absent (status other than "ok", or Anthropic reported none).
func windowPct(w *apitypes.RateLimitWindow) string {
	if w == nil {
		return "-"
	}
	return fmt.Sprintf("%d", w.Pct)
}

// tokenStateCell folds the two independent ways a token stops working into one
// column. They are genuinely different states and an operator reading an incident
// needs to tell them apart: "revoked" is a deliberate act with a trail, "expired" is
// the server-set 90-day bound arriving. Revoked wins when both are true, because it
// is the one someone DID.
func tokenStateCell(t apitypes.AdminCLITokenDTO) string {
	switch {
	case t.Revoked:
		return "revoked"
	case t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()):
		return "expired"
	default:
		return "active"
	}
}

// tsCell renders a nullable timestamp. "-" means the column is genuinely empty, not
// unknown: never used, or (for expires_at) never expires.
func tsCell(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
