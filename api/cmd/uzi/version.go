package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// serverProbeTimeout caps the GET /api/version this command makes.
//
// It exists because uzicli.NewHTTPClient defaults to a 30s client timeout, and
// `uzi version` runs inside Homebrew's sandboxed `test do` with no server anywhere
// (Formula/uzi-cli.rb, scripts/brew-local-test.sh). A 30s stall there is a failed
// release. Two seconds is long enough for a laptop compose stack or a LAN
// deployment and short enough that a human never wonders whether it hung.
const serverProbeTimeout = 2 * time.Second

// versionOut is the --json shape (PRD #175 M4, OQ-B).
//
// A WRAPPER, not a reshape: `version` keeps its exact meaning — the CLI's own
// ldflags stamp — so every existing `uzi version --json` parser is untouched, and
// the server's coordinates nest under a new key. Server is a POINTER to the shared
// apitypes DTO rather than a CLI-local copy, which is what preserves the
// unknown-versus-zero distinction the whole PRD is built around: the DTO's own
// pointers and omitempty tags carry through re-marshalling verbatim, so an
// unstamped server's absent fields stay absent here too. A local struct with plain
// int fields would quietly render them as 0.
type versionOut struct {
	Version string                 `json:"version"`
	Server  *apitypes.BuildInfoDTO `json:"server,omitempty"`
}

func newVersionCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the uzi CLI version, and the server's build info when reachable",
		// No literal `uzi …` string in this help text, deliberately. The extractor
		// behind TestPrintedInstructionsAreRegistered lifts any such span and demands
		// a registry entry — correctly, since that registry exists to stop the CLI
		// printing instructions nobody has run. A command's help naming ITSELF is a
		// circular reference that would consume an entry while asserting nothing, so
		// the wording avoids it rather than registering it.
		Long: "Print the uzi CLI version.\n\n" +
			"When a server URL is configured, also reports that server's build info " +
			"(version, source commit, build time, commit count, founding date, uptime). The server is " +
			"contacted best-effort with a short timeout: this command reports the CLI's " +
			"own version and exits 0 whether or not any server is reachable.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := env.printer(gf)
			srv := serverBuildInfo(cmd.Context(), env, gf)

			if p.Format == uzicli.FormatJSON {
				return p.JSON(versionOut{Version: version, Server: srv})
			}

			// The CLI's own version is printed FIRST and alone on line one, and that
			// is a hard constraint rather than a layout choice.
			// scripts/brew-local-test.sh matches `case "$out" in "v$version"*)`,
			// anchored at the start of the WHOLE stdout — stricter than the Formula's
			// substring assert_match. A header line, a "uzi " label, or a leading
			// blank line passes the Formula and fails the script.
			fmt.Fprintln(env.Stdout, version)
			if srv == nil {
				return nil
			}
			// Server-supplied build strings, printed outside the Printer because line
			// one must be the bare version and nothing else. So the uzicli boundary
			// cannot reach them and the sanitize is explicit here (#180). CellText, not
			// SanitizeTTY: %-8s pads a column, and a newline or tab in `version` or
			// `commit` would break the label rail these six lines share.
			for _, row := range serverRows(*srv) {
				fmt.Fprintf(env.Stdout, "server %-8s %s\n", row[0], uzicli.CellText(row[1]))
			}
			return nil
		},
	}
}

// serverBuildInfo fetches the server's build info, or returns nil.
//
// EVERY failure is nil, never an error: this command's contract is that it prints
// the CLI version and exits 0 regardless. That covers no URL configured (the brew
// sandbox), an unresolvable config, a plain-http remote refused by
// credentialSafeBase, a connection failure, and a server too old to answer.
//
// The no-URL case is checked BEFORE a client is built, so the common standalone
// invocation makes no network call at all and costs no time — otherwise the brew
// test would pay serverProbeTimeout for a connection that was never going to work.
func serverBuildInfo(ctx context.Context, env Env, gf *globalFlags) *apitypes.BuildInfoDTO {
	s, err := resolveSettings(env, gf)
	if err != nil || strings.TrimSpace(s.URL) == "" {
		return nil
	}
	c, err := env.client(gf)
	if err != nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, serverProbeTimeout)
	defer cancel()

	info, err := c.BuildInfo(ctx)
	if err != nil {
		return nil
	}
	return &info
}

// serverRows renders the present fields of a build info as label/value pairs, in a
// fixed order. Absent fields are SKIPPED rather than shown empty, mirroring the
// server's own omit-never-zero rule — a line reading `server commit` with nothing
// after it would assert exactly the false knowledge the server refuses to serve.
func serverRows(b apitypes.BuildInfoDTO) [][2]string {
	rows := [][2]string{{"version", b.Version}}
	if b.Commit != "" {
		rows = append(rows, [2]string{"commit", b.Commit})
	}
	if b.BuiltAt != "" {
		rows = append(rows, [2]string{"built", b.BuiltAt})
	}
	if b.Commits != nil {
		rows = append(rows, [2]string{"commits", fmt.Sprintf("%d", *b.Commits)})
	}
	if b.Founded != "" {
		rows = append(rows, [2]string{"founded", b.Founded})
	}
	if b.UptimeSeconds != nil {
		rows = append(rows, [2]string{"uptime", (time.Duration(*b.UptimeSeconds) * time.Second).String()})
	}
	return rows
}
