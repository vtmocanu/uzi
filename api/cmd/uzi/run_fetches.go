package main

import (
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
			dto, err := c.RunFetches(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return renderRunFetches(env.printer(gf), dto)
		},
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
