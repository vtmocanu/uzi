package main

// Catalog subtree (`uzi schedule catalog`) and the sweep-label guardrail helpers, split
// out of schedule.go (PRD #1009 M2). Declaration motion only.

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newScheduleCatalogCmd — `uzi schedule catalog` and its verbs (PRD #589 M3): the builtin
// default-schedule catalog and enabling a default job on one or more repos.
func newScheduleCatalogCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Browse and enable the builtin default scheduled jobs",
	}
	cmd.AddCommand(
		newScheduleCatalogListCmd(env, gf),
		newScheduleCatalogEnableCmd(env, gf),
	)
	return cmd
}

func newScheduleCatalogListCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the builtin default scheduled jobs and where you've enabled them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			cat, err := c.ListScheduleCatalog(cmd.Context())
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(cat)
			}
			// Count enabled repos per slug so the ON column reads "N repo(s)" or "-".
			enabledBySlug := map[string]int{}
			for _, en := range cat.Enablements {
				enabledBySlug[en.Slug]++
			}
			rows := make([][]string, 0, len(cat.Entries))
			for _, e := range cat.Entries {
				rows = append(rows, []string{
					e.Slug,
					e.Target,
					e.Cron,
					catalogEnabledOn(enabledBySlug[e.Slug]),
					e.Name,
				})
			}
			return p.Table([]string{"SLUG", "TARGET", "CRON", "ENABLED", "NAME"}, rows)
		},
	}
}

// catalogEnabledOn renders the ENABLED column of `catalog list`: how many of the caller's
// repos have this default enabled, or "-" when none.
func catalogEnabledOn(n int) string {
	if n == 0 {
		return "-"
	}
	if n == 1 {
		return "1 repo"
	}
	return fmt.Sprintf("%d repos", n)
}

func newScheduleCatalogEnableCmd(env Env, gf *globalFlags) *cobra.Command {
	enable := &cobra.Command{
		Use:   "enable <slug>",
		Short: "Enable a builtin default job on one or more repos",
		Long: "Enable a builtin default scheduled job (by SLUG from `uzi schedule catalog list`)\n" +
			"on one or more repos. Multi-repo enablement is a CLIENT-SIDE fan-out: this issues one\n" +
			"idempotent per-repo enable call per --repo, so a partial retry is safe and re-enabling\n" +
			"a repo simply reports it as already enabled.\n\n" +
			"For a SWEEP default (one that selects issues by label), the enable first checks each\n" +
			"target repo for the selector label and WARNS on any that is missing (the schedule is\n" +
			"still created — the sweep just will not match until the label exists). Pass\n" +
			"--create-missing-labels to create the missing labels on the forge FIRST, then enable.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := strings.TrimSpace(args[0])
			if slug == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "a catalog slug is required (list them with the catalog list command)")
			}
			repos, _ := cmd.Flags().GetStringArray("repo")
			repos = nonBlankTrimmed(repos)
			if len(repos) == 0 {
				return uzicli.Exitf(uzicli.ExitUsage, "at least one --repo is required (a repo id from `uzi repo list`)")
			}
			createMissing, _ := cmd.Flags().GetBool("create-missing-labels")
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			// Sweep-label guardrail (PRD #589 M4): resolve the selector labels for this slug
			// from the catalog (a default row stores NULL labels; the catalog is the source of
			// truth). Only a sweep default carries labels — for a prompt default this is empty
			// and the guardrail is a no-op.
			selector, err := resolveCatalogSweepLabels(cmd.Context(), c, slug)
			if err != nil {
				return err
			}
			runSweepLabelGuardrail(cmd, env, gf, c, repos, selector, createMissing)
			// Client-side fan-out: one idempotent per-repo enable per --repo. Accumulate the
			// per-repo results so --json returns them all and a mid-loop failure still reports
			// what already landed BEFORE the error propagates (the per-repo endpoint is
			// idempotent, so a partial landing plus a retry is safe).
			results := make([]catalogEnableResult, 0, len(repos))
			for _, repoID := range repos {
				s, created, eerr := c.EnableCatalogSchedule(cmd.Context(), repoID, slug)
				if eerr != nil {
					_ = renderCatalogEnableResults(env, gf, results)
					return eerr
				}
				results = append(results, catalogEnableResult{RepoID: repoID, ScheduleID: s.ID, Created: created})
			}
			return renderCatalogEnableResults(env, gf, results)
		},
	}
	enable.Flags().StringArray("repo", nil, "a repo id to enable this default on (repeatable for multi-repo enablement)")
	enable.Flags().Bool("create-missing-labels", false, "for a sweep default: create any selector label missing on a target repo before enabling (default: warn only)")
	return enable
}

// resolveCatalogSweepLabels returns the sweep selector labels for a catalog slug, resolved
// from the catalog list (a default schedule stores NULL labels, so the catalog is the
// source of truth). It returns an empty slice for a prompt default or an unknown slug — in
// which case the guardrail is a no-op and the enable's own call surfaces any real 404.
func resolveCatalogSweepLabels(ctx context.Context, c uzicli.Client, slug string) ([]string, error) {
	cat, err := c.ListScheduleCatalog(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range cat.Entries {
		if e.Slug == slug {
			if e.Target == schedTargetSweep {
				return e.Labels, nil
			}
			return nil, nil
		}
	}
	return nil, nil
}

// runSweepLabelGuardrail warns on (or, when createMissing is set, creates) any selector
// label missing on each target repo BEFORE the schedule is enabled/created/edited (PRD
// #589 M4).
//
// It is PURELY ADVISORY and NEVER blocks the write — not even on its own forge errors. A
// failed label CHECK (expired token, rate limit, forge unreachable) prints a WARNING to
// stderr and proceeds, so a transient forge outage can never abort an enable/create/edit
// that otherwise needs no forge read at all (a `catalog enable` computes next_fire_at from
// the catalog cron and did no forge read before this guardrail existed). A failed label
// CREATE (with --create-missing-labels) likewise warns and proceeds: the schedule is the
// primary goal and the label is idempotently retryable. A missing-but-checked label just
// means the sweep will not match until it exists. Diagnostics go to stderr so a --json
// stdout stays clean; an empty selector (a non-sweep target) makes this a no-op.
func runSweepLabelGuardrail(cmd *cobra.Command, env Env, gf *globalFlags, c uzicli.Client, repos, selector []string, createMissing bool) {
	if len(selector) == 0 {
		return
	}
	for _, repoID := range repos {
		missing, err := c.CheckRepoLabels(cmd.Context(), repoID, selector)
		if err != nil {
			if !gf.quiet {
				_, _ = fmt.Fprintf(env.Stderr,
					"WARNING: could not check labels on repo %s: %v; proceeding anyway\n", repoID, err)
			}
			continue
		}
		if len(missing) == 0 {
			continue
		}
		if createMissing {
			if err := c.EnsureRepoLabels(cmd.Context(), repoID, missing); err != nil {
				if !gf.quiet {
					_, _ = fmt.Fprintf(env.Stderr,
						"WARNING: could not create label(s) %s on repo %s: %v; proceeding anyway (the label is idempotently retryable)\n",
						strings.Join(missing, ", "), repoID, err)
				}
				continue
			}
			if !gf.quiet {
				_, _ = fmt.Fprintf(env.Stderr, "created missing label(s) %s on repo %s\n", strings.Join(missing, ", "), repoID)
			}
			continue
		}
		if !gf.quiet {
			_, _ = fmt.Fprintf(env.Stderr,
				"WARNING: label(s) %s do not exist on repo %s — the sweep will not match until they are created (re-run with --create-missing-labels to create them)\n",
				strings.Join(missing, ", "), repoID)
		}
	}
}

// catalogEnableResult is one per-repo outcome of the multi-repo `catalog enable` fan-out.
type catalogEnableResult struct {
	RepoID     string `json:"repo_id"`
	ScheduleID string `json:"schedule_id"`
	Created    bool   `json:"created"`
}

// renderCatalogEnableResults prints the per-repo enable outcomes: under --json the whole
// slice, in human mode one "enabled/already enabled on <repo>" line per row. Called both on
// success and, with the partial slice, on a mid-loop failure so what already landed is
// legible before the error propagates.
func renderCatalogEnableResults(env Env, gf *globalFlags, results []catalogEnableResult) error {
	p := env.printer(gf)
	if p.Format == uzicli.FormatJSON {
		return p.JSON(results)
	}
	if gf.quiet {
		return nil
	}
	for _, res := range results {
		state := "already enabled"
		if res.Created {
			state = "enabled"
		}
		p.Printf("%s on %s · schedule %s\n", state, res.RepoID, res.ScheduleID)
	}
	return nil
}
