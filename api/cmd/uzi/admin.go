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
				rows = append(rows, []string{r.ID, strOr(r.OwnerEmail, "-"), r.Kind, r.TriggerSource, runStatusCell(r), runTitle(r.RunDTO)})
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
				// VERSION is the worker's self-reported version — untrusted, so cellText, with
				// the same sanitize-then-"-" fold `uzi worker list` uses (a version that is all
				// format characters must still read "-", not blank). UPGRADE reuses upgradeCell
				// (worker.go), the SAME five-state renderer `uzi worker list` uses, so the two
				// consumers never describe a worker differently — this is the roll-health column
				// the admin list was missing (PRD #1484). BLOCKING is the compact
				// container/reason behind an upgrade_failed, through cellText.
				version := cellText(strOr(w.Version, ""))
				if version == "" {
					version = "-"
				}
				rows = append(rows, []string{
					w.ID, w.OwnerEmail, cellText(w.Name), w.Status,
					version, upgradeCell(w.WorkerDTO), blockingCell(w.WorkerDTO),
					reportedRunsCell(w.WorkerDTO), outboxCell(w.WorkerDTO),
				})
			}
			// VERSION / UPGRADE / BLOCKING (PRD #1484 M3): the roll health the admin list finally
			// carries, now that ListAllWorkers joins worker_upgrade_reports (M2) — cross-user,
			// so an admin can see which owner's fleet is stuck rolling and why. RUNS (PRD #1390
			// M2c): what each worker SAYS it is executing, summarized per phase. OUTBOX (PRD
			// #1391 M5): the depth of a worker's locally-buffered updates waiting to replay —
			// "-" in the steady state, a count while the api was unreachable. RUNS/UPGRADE/OUTBOX
			// share their renderers with `uzi worker list` (worker.go), read off the embedded
			// WorkerDTO.
			return p.Table([]string{"ID", "OWNER", "NAME", "STATUS", "VERSION", "UPGRADE", "BLOCKING", "RUNS", "OUTBOX"}, rows)
		},
	}

	var healthAll, healthStrict bool
	health := &cobra.Command{
		Use:   "health",
		Short: "Instance health: the checks needing attention, a verdict and a tally",
		Long: "Read the admin health document (PRD #1484): a closed registry of checks over " +
			"what uzi knows about itself — worker rolls, queue and capacity, the controller " +
			"report, background loops, the database, integrations and housekeeping.\n\n" +
			"By default it prints only the checks needing attention (everything that is not " +
			"ok and not na), then the overall verdict and a per-severity tally. --all lists " +
			"every check. --json emits the endpoint's document unchanged.\n\n" +
			"EXIT CODE, for a probe: 0 unless the overall status is danger, then 8. --strict " +
			"also exits 8 on warn or unknown. Exit 8 is a SUCCESS-path exit — the HTTP call " +
			"returned 200 carrying an unhealthy verdict — so the full report prints first. A " +
			"transport or auth failure keeps its own code (3 for a 401, 6 for a 5xx or an " +
			"unreachable server), so a cron probe can tell \"unhealthy\" (8) from \"could not " +
			"ask\" (3/6). Needs a uza_ (admin_ro) token.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			return runAdminHealth(env, gf, c, cmd, healthAll, healthStrict)
		},
	}
	health.Flags().BoolVar(&healthAll, "all", false, "list every check, not just the ones needing attention")
	health.Flags().BoolVar(&healthStrict, "strict", false, "also exit 8 when the overall status is warn or unknown")

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

	var rlProvider string
	rateLimits := &cobra.Command{
		Use:   "rate-limits",
		Short: "Show rate-limit meters per user (--provider claude|codex)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateProvider(rlProvider); err != nil {
				return err
			}
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if rlProvider == providerCodex {
				return runAdminCodexRateLimits(cmd, c, p)
			}
			rls, err := c.AdminRateLimits(cmd.Context())
			if err != nil {
				return err
			}
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
	// --provider selects the credential family. Absent or "claude" keeps the historical
	// Anthropic output above byte-for-byte; "codex" renders the per-account Codex view
	// below (PRD #1209 M3).
	rateLimits.Flags().StringVar(&rlProvider, "provider", providerClaude, "credential family: claude|codex")

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

	cmd.AddCommand(users, runs, workers, health, usage, rateLimits, cliTokens, guardrailImpact, blockedRepos, newAdminAgentSourceCmd(env, gf), newAdminReviewCmd(env, gf))
	return cmd
}

// runAdminHealth fetches and renders the admin health document, then returns the exit-8
// sentinel when the overall verdict warrants it. The flow is deliberate: PRINT THE FULL
// REPORT FIRST, then return uzicli.ErrHealthDanger (which ExitCodeFor maps to
// ExitHealthDanger = 8). Exit 8 is a SUCCESS-path exit — the HTTP GET returned 200 carrying
// an unhealthy verdict — so a transport/auth failure (c.AdminHealth error) is returned
// straight through and keeps its own code (3/6), never 8.
//
// EVERY server string that lands in a table cell goes through the bounded package-local
// cellText, never the unbounded uzicli.CellText: the document is server-authored from fixed
// templates, but it interpolates already-sanitized identifiers an owner controls (worker
// names, repo paths), and this is an ADMIN's terminal. cellText's 200-rune cap is what bounds
// a hostile megabyte-long summary that CellText alone would print in full.
func runAdminHealth(env Env, gf *globalFlags, c uzicli.Client, cmd *cobra.Command, all, strict bool) error {
	doc, err := c.AdminHealth(cmd.Context())
	if err != nil {
		return err
	}
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		// --json emits the endpoint's document unchanged (a re-marshal of the decoded DTO).
		// The verdict still drives the exit code so a probe piping --json to a log still
		// distinguishes danger (8) from healthy (0).
		if err := p.JSON(doc); err != nil {
			return err
		}
		return healthVerdictError(doc.Status, strict)
	}
	rows := make([][]string, 0, len(doc.Checks))
	for _, ck := range doc.Checks {
		if !all && (ck.Severity == "ok" || ck.Severity == "na") {
			continue
		}
		rows = append(rows, []string{
			cellText(strings.ToUpper(ck.Severity)),
			cellText(ck.ID),
			cellText(healthSince(ck.Since)),
			cellText(ck.Summary),
		})
	}
	if len(rows) > 0 {
		if err := p.Table([]string{"SEVERITY", "CHECK", "SINCE", "SUMMARY"}, rows); err != nil {
			return err
		}
	} else if !all {
		// Nothing needs attention and --all was not asked for: say so rather than print an
		// empty table with only a header.
		p.Println("all checks passing")
	}
	p.Printf("status: %s\n", strings.ToUpper(doc.Status))
	p.Printf("checks: %d ok, %d warn, %d danger, %d unknown, %d na\n",
		doc.Counts.OK, doc.Counts.Warn, doc.Counts.Danger, doc.Counts.Unknown, doc.Counts.NA)
	return healthVerdictError(doc.Status, strict)
}

// healthVerdictError maps the overall status to the exit-8 sentinel when the CLI should
// exit nonzero: always on danger, and additionally on warn/unknown under --strict. A plain
// ok returns nil (exit 0). Any value OUTSIDE the closed overall enum (ok | warn | danger |
// unknown — "na" is never an overall status) is a malformed document and returns an error:
// an empty Status from a `{}` body must NOT read as healthy to a cron probe, or exit 0 would
// silently mask a broken endpoint. The bare sentinel reads cleanly on the danger path; the
// strict path wraps it with the actual status so the stderr line is honest that a warn or
// unknown, not a danger, drove the exit-8 code.
func healthVerdictError(status string, strict bool) error {
	switch status {
	case "danger":
		return uzicli.ErrHealthDanger
	case "warn", "unknown":
		if strict {
			return fmt.Errorf("overall health status is %s (--strict): %w", status, uzicli.ErrHealthDanger)
		}
		return nil
	case "ok":
		return nil
	default:
		return fmt.Errorf("invalid overall health status %q", status)
	}
}

// healthSince renders a check's nullable RFC3339 `since` for the SINCE column: "-" when the
// check carries none (an ok/na check, or a source with no timestamp).
func healthSince(since *string) string {
	if since == nil || *since == "" {
		return "-"
	}
	return *since
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

// failRate renders the failed-run rate as a one-decimal percentage with a "%"
// suffix, or "-" when there are no finished runs (PRD #1293 D6). The rate is
// client-computed (failed/finished) so the CLI and web share one formatting rule
// and cannot disagree with each other or the server.
func failRate(failed, finished int64) string {
	if finished == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(failed)/float64(finished)*100)
}

// failedFigure renders the failed count with the needs-landing sub-cut appended when any of
// those failed runs is human-landable (issue #1418): "106" normally, "106 (3 need landing)"
// when needs_landing > 0. This is the CLI twin of the dashboard splitting the `failed` bar into
// "failed" and "failed, needs landing". needs_landing is a SUBSET of failed (needs_landing <=
// failed), never a new denominator member, so it rides beside the figure rather than as a
// separate column; the clause is emit-only-when-positive, keeping the common no-landing case
// terse and every existing zero-landing row byte-for-byte unchanged.
func failedFigure(failed, needsLanding int64) string {
	if needsLanding > 0 {
		return fmt.Sprintf("%d (%d need landing)", failed, needsLanding)
	}
	return fmt.Sprintf("%d", failed)
}

// renderAdminUsage prints the factory lifetime totals plus the per-user breakdown.
// The factory line and the per-user table carry the PRD #1293 failed-run figures
// (lifetime), mirroring the web column order (D8): Runs · Failed · Fail rate come
// before the token columns. SHARE stays web-only.
func renderAdminUsage(p *uzicli.Printer, u apitypes.AdminUsageDTO) error {
	lt := u.Factory.Lifetime
	lo := u.Factory.Outcomes.Lifetime
	// failed carries the needs-landing sub-cut inline (issue #1418): "failed=106" normally,
	// "failed=106 (3 need landing)" when some failed runs are human-landable — the CLI half of
	// the dashboard's failed-bar split, terse and appended only when needs_landing > 0.
	p.Printf("factory (lifetime): input=%d cache_read=%d cache_creation=%d output=%d cost=$%.2f (runs=%d) finished=%d failed=%s fail_rate=%s\n",
		lt.InputTokens, lt.CacheReadTokens, lt.CacheCreationTokens, lt.OutputTokens, lt.CostUSD, u.Factory.RunCount,
		lo.Finished, failedFigure(lo.Failed, lo.NeedsLanding), failRate(lo.Failed, lo.Finished))
	if len(u.Users) == 0 {
		return nil
	}
	p.Println()
	rows := make([][]string, 0, len(u.Users))
	for _, row := range u.Users {
		rows = append(rows, []string{
			row.Email,
			fmt.Sprintf("%d", row.RunCount),
			// FAILED carries the same inline needs-landing sub-cut as the factory line, so the
			// per-user row and the total read the split the same way (no new column).
			failedFigure(row.Outcomes.Failed, row.Outcomes.NeedsLanding),
			failRate(row.Outcomes.Failed, row.Outcomes.Finished),
			fmt.Sprintf("%d", row.Usage.InputTokens),
			fmt.Sprintf("%d", row.Usage.OutputTokens),
			fmt.Sprintf("$%.2f", row.Usage.CostUSD),
		})
	}
	return p.Table([]string{"EMAIL", "RUNS", "FAILED", "FAIL%", "INPUT", "OUTPUT", "COST"}, rows)
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
