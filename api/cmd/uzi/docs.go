package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/uzicli"
	"github.com/vtmocanu/uzi/api/internal/uzidocs"
)

// newDocsCmd — `uzi docs`. list/show/search read the corpus EMBEDDED in this
// binary (internal/uzidocs) with no server and no token: onboarding is the
// pre-connection moment (PRD #567 D5/D8). None of these verbs calls
// env.client(gf); `docs` is added to exemptFromVersionCheck (D8) so even a
// configured binary makes no network call of its own here.
//
// The `docs` parent has no RunE — it is a non-runnable group, so the drift test
// does not require it to be documented; the three leaves are. No PersistentPreRun
// is set here: the root PersistentPreRun seam is single-occupancy (see the 🔴
// comment in root.go), and setting one on this subtree would silently disable the
// root's skill-upgrade + skew hooks for it and every sibling walk.
func newDocsCmd(env Env, gf *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "docs",
		Short: "Read uzi's product docs, embedded and offline",
	}

	var listAudience string
	list := &cobra.Command{
		Use:   "list",
		Short: "List embedded docs, filtered by audience",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateAudience(listAudience); err != nil {
				return err
			}
			docs := uzidocs.List(listAudience)
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				out := make([]docListOut, 0, len(docs))
				for _, d := range docs {
					out = append(out, docListOut{
						Slug:     d.Slug,
						Title:    d.Meta.Title,
						Audience: d.Meta.Audience,
						Order:    d.Meta.Order,
					})
				}
				return p.JSON(out)
			}
			rows := make([][]string, 0, len(docs))
			for _, d := range docs {
				rows = append(rows, []string{d.Slug, d.Meta.Title, d.Meta.Audience, orderCell(d.Meta.Order)})
			}
			return p.Table([]string{"SLUG", "TITLE", "AUDIENCE", "ORDER"}, rows)
		},
	}
	list.Flags().StringVar(&listAudience, "audience", uzidocs.AudienceUser,
		"audience filter: user|operator|design|contributor|all")

	show := &cobra.Command{
		Use:   "show <slug>",
		Short: "Print an embedded doc's markdown body",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			slug := args[0]
			doc, ok := uzidocs.Get(slug)
			if !ok {
				if s := uzidocs.SuggestSlug(slug); s != "" {
					return uzicli.Exitf(uzicli.ExitNotFound, "unknown doc %q; did you mean %q?", slug, s)
				}
				return uzicli.Exitf(uzicli.ExitNotFound, "unknown doc %q", slug)
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(docShowOut{
					Slug: doc.Slug,
					Meta: docMetaOut{Title: doc.Meta.Title, Order: doc.Meta.Order, Audience: doc.Meta.Audience},
					Body: doc.Body,
				})
			}
			// D6: doc content is repo-controlled and trusted, so the body is printed
			// VERBATIM to stdout. It deliberately does NOT go through Printer.Printf/
			// Table, which would sanitize/reflow the markdown — a raw write of trusted
			// embedded content is correct here.
			_, _ = fmt.Fprintln(env.Stdout, doc.Body)
			return nil
		},
	}

	var searchAudience string
	search := &cobra.Command{
		Use:   "search <query>",
		Short: "Search embedded docs by substring over title and body",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateAudience(searchAudience); err != nil {
				return err
			}
			results := uzidocs.Search(args[0], searchAudience)
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				out := make([]docSearchOut, 0, len(results))
				for _, r := range results {
					out = append(out, docSearchOut{
						Slug:     r.Slug,
						Title:    r.Title,
						Audience: r.Audience,
						Excerpt:  r.Excerpt,
						InTitle:  r.InTitle,
					})
				}
				return p.JSON(out)
			}
			rows := make([][]string, 0, len(results))
			for _, r := range results {
				rows = append(rows, []string{r.Slug, r.Title, r.Excerpt})
			}
			return p.Table([]string{"SLUG", "TITLE", "SNIPPET"}, rows)
		},
	}
	search.Flags().StringVar(&searchAudience, "audience", uzidocs.AudienceUser,
		"audience filter: user|operator|design|contributor|all")

	cmd.AddCommand(list, show, search)
	return cmd
}

// docListOut / docShowOut / docMetaOut / docSearchOut are the stable --json shapes
// for the docs verbs. Order is a *int with omitempty so a doc with no `order:`
// frontmatter omits the key rather than emitting a misleading 0.
type docListOut struct {
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Audience string `json:"audience"`
	Order    *int   `json:"order,omitempty"`
}

type docMetaOut struct {
	Title    string `json:"title"`
	Order    *int   `json:"order,omitempty"`
	Audience string `json:"audience"`
}

type docShowOut struct {
	Slug string     `json:"slug"`
	Meta docMetaOut `json:"meta"`
	Body string     `json:"body"`
}

type docSearchOut struct {
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Audience string `json:"audience"`
	Excerpt  string `json:"excerpt"`
	InTitle  bool   `json:"in_title"`
}

// validateAudience accepts the four closed audiences plus "all". An invalid value
// is a usage error (exit 2), consistent with the rest of the CLI.
func validateAudience(v string) error {
	switch v {
	case uzidocs.AudienceUser, uzidocs.AudienceOperator, uzidocs.AudienceDesign,
		uzidocs.AudienceContributor, "all":
		return nil
	default:
		return uzicli.Exitf(uzicli.ExitUsage,
			"invalid --audience %q (want user|operator|design|contributor|all)", v)
	}
}

// orderCell renders an Order pointer for a table cell: the number, or empty when nil.
func orderCell(o *int) string {
	if o == nil {
		return ""
	}
	return strconv.Itoa(*o)
}
