package main

// admin_product_skills.go is `uzi admin products skills <product>` (PRD #1909 M7): the read-only
// view of one product's skill set. Setting the source, the clone token, syncing and approving
// stay browser-only admin actions. Every server-supplied string (repo URL, ref, skill names,
// drop reasons, the staging admin) is untrusted display text and goes through the Printer's
// table cells or cellText; skill bodies are never printed (--json carries them).

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func newAdminProductSkillsCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "skills <product>",
		Short: "Show a product's skill set (read-only)",
		Long: "Show one product's skill set (PRD #1909): the source config (repo URL, ref, whether " +
			"a clone token is set, whether the instance enables the feature), the applied set " +
			"(sha, when, skill names) and any staged snapshot waiting for approval (sha, diff " +
			"against the applied set, skills the sync dropped and why). <product> is the " +
			"product's name or id. Setting the source, syncing and approving are browser-only " +
			"admin actions; the clone token is never shown.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			ps, err := c.AdminListProducts(cmd.Context())
			if err != nil {
				return err
			}
			prod, err := resolveProduct(ps, args[0])
			if err != nil {
				return err
			}
			d, err := c.AdminProductSkills(cmd.Context(), prod.ID)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(d)
			}
			return renderProductSkills(p, prod, d)
		},
	}
}

// resolveProduct finds a product by id, else by exact name (case-sensitive, then
// case-insensitive). A name shared by several products is a usage error listing their ids.
func resolveProduct(ps []apitypes.ProductDTO, ref string) (apitypes.ProductDTO, error) {
	ref = strings.TrimSpace(ref)
	for _, p := range ps {
		if p.ID == ref {
			return p, nil
		}
	}
	for _, match := range []func(string) bool{
		func(n string) bool { return n == ref },
		func(n string) bool { return strings.EqualFold(n, ref) },
	} {
		var hits []apitypes.ProductDTO
		for _, p := range ps {
			if match(p.Name) {
				hits = append(hits, p)
			}
		}
		switch len(hits) {
		case 0:
			continue
		case 1:
			return hits[0], nil
		}
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.ID
		}
		return apitypes.ProductDTO{}, uzicli.Exitf(uzicli.ExitUsage, "%d products are named %q: use an id (%s)", len(hits), cellText(ref), strings.Join(ids, ", "))
	}
	return apitypes.ProductDTO{}, uzicli.Exitf(uzicli.ExitNotFound, "no product %q: give the name or id of a registered product", cellText(ref))
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func skillNames(skills []apitypes.ProductSkillDTO) string {
	if len(skills) == 0 {
		return "-"
	}
	names := make([]string, len(skills))
	for i, s := range skills {
		names[i] = cellText(s.Name)
	}
	return strings.Join(names, ", ")
}

func nameList(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = cellText(n)
	}
	return strings.Join(out, ", ")
}

func renderProductSkills(p *uzicli.Printer, prod apitypes.ProductDTO, d apitypes.ProductSkillsDTO) error {
	repo := d.Config.SkillsRepoURL
	if repo == "" {
		repo = "-"
	}
	ref := d.Config.SkillsRef
	if ref == "" {
		ref = "-"
	}
	rows := [][]string{
		{"PRODUCT", prod.Name + " (" + prod.ID + ")"},
		{"REPO_URL", repo},
		{"REF", ref},
		{"TOKEN_SET", yesNo(d.Config.SkillsTokenSet)},
		{"ENABLED", yesNo(d.Config.Enabled)},
	}
	if !d.Config.Enabled {
		rows = append(rows, []string{"NOTE", "the instance has no allowed skills repo base URL (UZI_PRODUCT_SKILLS_ALLOWED_BASE_URLS), so nothing is delivered to jobs"})
	}
	appliedSHA := d.Applied.SHA
	if appliedSHA == "" {
		appliedSHA = "none"
	}
	rows = append(rows,
		[]string{"APPLIED_SHA", appliedSHA},
		[]string{"APPLIED_AT", tsCell(d.Applied.AppliedAt)},
	)
	if d.Applied.AppliedBy != nil {
		rows = append(rows, []string{"APPLIED_BY", *d.Applied.AppliedBy})
	}
	rows = append(rows, []string{"APPLIED_SKILLS", skillNames(d.Applied.Skills)})
	if d.Staged == nil {
		rows = append(rows, []string{"STAGED", "none"})
		return p.Table(nil, rows)
	}
	s := d.Staged
	rows = append(rows,
		[]string{"STAGED_SHA", s.SHA},
		[]string{"STAGED_AT", s.StagedAt.UTC().Format(time.RFC3339)},
	)
	if s.StagedBy != nil {
		rows = append(rows, []string{"STAGED_BY", *s.StagedBy})
	}
	dropped := "-"
	if len(s.Dropped) > 0 {
		parts := make([]string, len(s.Dropped))
		for i, dr := range s.Dropped {
			name := cellText(dr.Name)
			if name == "" {
				name = fmt.Sprintf("%d files", dr.Count)
			}
			parts[i] = name + " (" + cellText(dr.Reason) + ")"
		}
		dropped = strings.Join(parts, ", ")
	}
	rows = append(rows,
		[]string{"STAGED_SKILLS", skillNames(s.Skills)},
		[]string{"DIFF_ADDED", nameList(s.Diff.Added)},
		[]string{"DIFF_CHANGED", nameList(s.Diff.Changed)},
		[]string{"DIFF_REMOVED", nameList(s.Diff.Removed)},
		[]string{"DIFF_UNCHANGED", strconv.Itoa(len(s.Diff.Unchanged))},
		[]string{"DROPPED", dropped},
	)
	return p.Table(nil, rows)
}
