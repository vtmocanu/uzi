package main

import (
	"context"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// newRunFetchesCmd builds `uzi run fetches <run-id> [--json]` (PRD #1906 M3): the source
// log of a profile-bound research run, one row per fetch attempt, allowed or refused.
// Every text cell (URL, final URL, content type, reason) is site- or agent-controlled, so
// it goes through cellText (control and format runes stripped, 200-rune cap) even though
// the api already escaped it at write time: the render site is the trust boundary.
//
// The api pages the log (fetchctl.FetchesPageSize rows a page); the command follows every
// page and prints the whole log, which fetch_max_run_attempts bounds (default 500, so one
// page). There is no --after/--limit: a partial log is not what an owner audits. A walk of
// several pages while the run is still fetching can skip an attempt that commits late (the
// api's keyset is the row's transaction-start time; handler.ListRunFetches), so a log above
// one page is complete only when read after the run has ended and its fetches reported.
func newRunFetchesCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "fetches <run-id>",
		Short: "Show a research run's source log: every web fetch it attempted",
		Long: "List every fetch attempt of a run bound to a site list (official-sources " +
			"research), oldest first: when it started, whether it was allowed or refused and " +
			"why, the upstream HTTP status, the bytes returned, the content type, the URL " +
			"asked for and the URL the content came from after redirects.\n\n" +
			"Owner-only: another user's run reads as not found (exit 4). A run that never " +
			"fetched shows no rows. Long URLs are cut in the table; --json prints them whole, " +
			"with each file's sha256.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.client(gf)
			if err != nil {
				return err
			}
			dto, err := allRunFetches(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			return renderRunFetches(env.printer(gf), dto)
		},
	}
}

// maxFollowedFetches is the most rows allRunFetches collects: the fetch_max_run_attempts
// ceiling (api/internal/settings), which no honest log exceeds. With the checks below it
// bounds how long a misbehaving server can keep the command paging.
const maxFollowedFetches = 100000

// allRunFetches follows the log's next_cursor chain from the first page and returns every
// row, with NextCursor cleared. A page that carries a cursor but no rows, a cursor that
// repeats, or more rows than maxFollowedFetches is a server fault, reported as an error
// rather than looped on.
func allRunFetches(ctx context.Context, c uzicli.Client, runID string) (apitypes.RunFetchesDTO, error) {
	out := apitypes.RunFetchesDTO{Fetches: []apitypes.RunFetchDTO{}}
	seen := map[string]bool{}
	after := ""
	for {
		page, err := c.RunFetches(ctx, runID, after)
		if err != nil {
			return apitypes.RunFetchesDTO{}, err
		}
		out.Fetches = append(out.Fetches, page.Fetches...)
		if len(out.Fetches) > maxFollowedFetches {
			return apitypes.RunFetchesDTO{}, uzicli.Exitf(uzicli.ExitGeneric, "the server returned more than %d fetches for one run", maxFollowedFetches)
		}
		if page.NextCursor == "" {
			return out, nil
		}
		if len(page.Fetches) == 0 || seen[page.NextCursor] {
			return apitypes.RunFetchesDTO{}, uzicli.Exitf(uzicli.ExitGeneric, "the server's fetch log pagination did not advance")
		}
		seen[page.NextCursor] = true
		after = page.NextCursor
	}
}

func renderRunFetches(p *uzicli.Printer, dto apitypes.RunFetchesDTO) error {
	if p.Format == uzicli.FormatJSON {
		if dto.Fetches == nil {
			dto.Fetches = []apitypes.RunFetchDTO{}
		}
		return p.JSON(dto)
	}
	rows := make([][]string, 0, len(dto.Fetches))
	for _, f := range dto.Fetches {
		status := "-"
		if f.HTTPStatus > 0 {
			status = strconv.Itoa(f.HTTPStatus)
		}
		rows = append(rows, []string{
			updatedCell(f.StartedAt),
			cellText(f.Verdict),
			dashOr(cellText(f.Reason)),
			status,
			strconv.FormatInt(f.Bytes, 10),
			dashOr(cellText(f.ContentType)),
			cellText(f.URL),
			dashOr(cellText(f.FinalURL)),
		})
	}
	return p.Table([]string{"STARTED", "VERDICT", "REASON", "HTTP", "BYTES", "CONTENT TYPE", "URL", "FINAL URL"}, rows)
}
