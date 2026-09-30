package main

// job.go holds `uzi job` (PRD #1908 M7): create, get, result, cancel and list repo-less jobs
// over the stable /api/v1/jobs API. Every server-supplied string (title, requested_by_label,
// failure_reason, report, finding text, locations) is untrusted display text: single-line
// fields go through uzicli.CellText and the multi-line report through Printer.Printf
// (SanitizeTTY), so control, ANSI and bidi characters never reach the terminal. --json prints
// the raw DTO, which encoding/json already escapes.

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Client-side mirrors of the server's create bounds (workersvc/jobs.go), so a bad input is a
// friendly usage error before any request. The server stays authoritative.
const (
	maxJobInputs        = 20
	maxJobInputTotal    = 1 << 20 // total input content per job
	maxJobPromptRead    = 1 << 20 // read bound for --prompt-file; the server enforces its own, lower cap
	jobInputNameMaxRune = 100
)

// jobInputNameRE mirrors the server's input-name rule; '..' is rejected separately.
var jobInputNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func newJobCmd(env Env, gf *globalFlags) *cobra.Command {
	job := &cobra.Command{
		Use:   "job",
		Short: "Create and inspect repo-less jobs (research, ...)",
		Long: "Create and inspect repo-less jobs (PRD #1908): a prompt and optional named text " +
			"inputs run by a worker, with a structured report and findings as the result. " +
			"Jobs use the stable /api/v1/jobs API with your CLI token.",
	}
	job.AddCommand(
		newJobCreateCmd(env, gf),
		newJobGetCmd(env, gf),
		newJobResultCmd(env, gf),
		newJobCancelCmd(env, gf),
		newJobListCmd(env, gf),
	)
	return job
}

func newJobCreateCmd(env Env, gf *globalFlags) *cobra.Command {
	var (
		jobType, prompt, promptFile, title string
		inputs                             []string
		budget                             int
	)
	cmd := &cobra.Command{
		Use:   "create --type <type> (--prompt <text> | --prompt-file <path>) [flags]",
		Short: "Create a job",
		Long: "Create a job. The prompt is given inline (--prompt) or from a file " +
			"(--prompt-file, '-' reads stdin). Attach named text inputs with " +
			"--input name=@file (repeatable). The title is derived from the prompt when omitted.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(jobType) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "--type is required (for example: research)")
			}
			hasPrompt, hasFile := cmd.Flags().Changed("prompt"), cmd.Flags().Changed("prompt-file")
			if hasPrompt == hasFile {
				return uzicli.Exitf(uzicli.ExitUsage, "give exactly one of --prompt or --prompt-file")
			}
			if hasFile {
				text, err := readJobPromptFile(env, promptFile)
				if err != nil {
					return err
				}
				prompt = text
			}
			if strings.TrimSpace(prompt) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "the prompt is empty")
			}
			jobInputs, err := readJobInputs(inputs)
			if err != nil {
				return err
			}
			req := apitypes.V1JobCreateRequest{Type: jobType, Prompt: prompt, Inputs: jobInputs}
			if cmd.Flags().Changed("title") {
				req.Title = &title
			}
			if cmd.Flags().Changed("budget-seconds") {
				if budget < 1 {
					return uzicli.Exitf(uzicli.ExitUsage, "--budget-seconds must be a positive integer")
				}
				req.WallSeconds = &budget
			}
			c, err := env.jobClient(gf)
			if err != nil {
				return err
			}
			j, err := c.JobCreate(cmd.Context(), req)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(j)
			}
			return renderJobDetail(p, j)
		},
	}
	f := cmd.Flags()
	f.StringVar(&jobType, "type", "", "job type (required), e.g. research")
	f.StringVar(&prompt, "prompt", "", "the job prompt")
	f.StringVar(&promptFile, "prompt-file", "", "read the prompt from this file ('-' for stdin)")
	f.StringVar(&title, "title", "", "job title (derived from the prompt when omitted)")
	f.StringArrayVar(&inputs, "input", nil, "attach a named text input, name=@file (repeatable)")
	f.IntVar(&budget, "budget-seconds", 0, "wall-clock limit in seconds (the server caps it)")
	return cmd
}

func newJobGetCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "get <job-id>",
		Short: "Show a job's status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.jobClient(gf)
			if err != nil {
				return err
			}
			j, err := c.JobGet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(j)
			}
			return renderJobDetail(p, j)
		},
	}
}

func newJobResultCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "result <job-id>",
		Short: "Show a job's report and findings",
		Long: "Show a job's report and findings. A job that has not reported yet prints its " +
			"status and no result.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.jobClient(gf)
			if err != nil {
				return err
			}
			r, err := c.JobResult(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(r)
			}
			renderJobResult(p, r)
			return nil
		},
	}
}

func newJobCancelCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <job-id>",
		Short: "Cancel a queued, running or waiting job",
		Long: "Cancel a queued, running or waiting job. A running job is stopped by its worker, so the " +
			"printed status may still read running for a moment.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.jobClient(gf)
			if err != nil {
				return err
			}
			j, err := c.JobCancel(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(j)
			}
			return renderJobDetail(p, j)
		},
	}
}

func newJobListCmd(env Env, gf *globalFlags) *cobra.Command {
	var (
		limit  int
		cursor string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List jobs, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("limit") && (limit < 1 || limit > 100) {
				return uzicli.Exitf(uzicli.ExitUsage, "--limit must be from 1 to 100")
			}
			c, err := env.jobClient(gf)
			if err != nil {
				return err
			}
			page, err := c.JobList(cmd.Context(), limit, cursor)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(page)
			}
			rows := make([][]string, 0, len(page.Jobs))
			for _, j := range page.Jobs {
				rows = append(rows, []string{j.ID, j.Type, j.Status, j.CreatedAt.UTC().Format(time.RFC3339), cellText(j.Title)})
			}
			if err := p.Table([]string{"ID", "TYPE", "STATUS", "CREATED", "TITLE"}, rows); err != nil {
				return err
			}
			if page.NextCursor != nil && *page.NextCursor != "" {
				p.Printf("more: uzi job list --cursor %s\n", *page.NextCursor)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "page size, 1 to 100 (server default 50)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "the next cursor printed by a previous page")
	return cmd
}

// renderJobDetail prints one job as a key/value block. requested_by_label is text the calling
// product supplied, so it is marked as reported by the product, not as a verified identity.
func renderJobDetail(p *uzicli.Printer, j apitypes.V1JobDTO) error {
	rows := [][]string{
		{"ID", j.ID},
		{"TYPE", j.Type},
		{"STATUS", j.Status},
		{"TITLE", j.Title},
	}
	if j.RequestedByLabel != nil && *j.RequestedByLabel != "" {
		rows = append(rows, []string{"REQUESTED_BY", *j.RequestedByLabel + " (reported by the product)"})
	}
	if j.FailureReason != nil && *j.FailureReason != "" {
		rows = append(rows, []string{"FAILURE", *j.FailureReason})
	}
	if j.WallSeconds != nil {
		rows = append(rows, []string{"WALL_SECONDS", strconv.Itoa(*j.WallSeconds)})
	}
	rows = append(rows,
		[]string{"CREATED", j.CreatedAt.UTC().Format(time.RFC3339)},
		[]string{"STARTED", tsCell(j.StartedAt)},
		[]string{"FINISHED", tsCell(j.FinishedAt)},
	)
	return p.Table(nil, rows)
}

// renderJobResult prints the status lines, then the findings, then the report last and
// indented one level under its label. The report is LLM text and SanitizeTTY keeps newlines,
// so an unindented report could forge a finding row or a status line; every report line is
// indented, so anything at column 0 was drawn by the CLI itself.
func renderJobResult(p *uzicli.Printer, r apitypes.V1JobResultDTO) {
	p.Printf("job status: %s\n", uzicli.CellText(r.JobStatus))
	if r.Result == nil {
		// A finished job that never reported will not report now; only a live one might yet.
		switch r.JobStatus {
		case "completed", "failed", "cancelled":
			p.Println("no result")
		default:
			p.Println("no result yet")
		}
		return
	}
	p.Printf("result status: %s\n", uzicli.CellText(r.Result.Status))
	if len(r.Result.Findings) > 0 {
		p.Println("\nFINDINGS")
		for _, f := range r.Result.Findings {
			line := fmt.Sprintf("- [%s] %s", uzicli.CellText(f.Severity), uzicli.CellText(f.MessageMd))
			if loc := findingLocation(f); loc != "" {
				line += " (" + loc + ")"
			}
			p.Println(line)
		}
	}
	report := strings.TrimSpace(r.Result.ReportMd)
	if report == "" {
		p.Println("\nREPORT\n    (empty)")
		return
	}
	p.Println("\nREPORT")
	for _, line := range strings.Split(uzicli.SanitizeTTY(report), "\n") {
		p.Printf("    %s\n", line)
	}
}

// findingLocation is a finding's URL or file[:line], folded to one line.
func findingLocation(f apitypes.V1JobFindingDTO) string {
	if f.URL != nil && *f.URL != "" {
		return uzicli.CellText(*f.URL)
	}
	if f.File != nil && *f.File != "" {
		loc := uzicli.CellText(*f.File)
		if f.Line != nil {
			loc += ":" + strconv.Itoa(*f.Line)
		}
		return loc
	}
	return ""
}

// readJobPromptFile reads the prompt from a file or, for "-", stdin, bounded.
func readJobPromptFile(env Env, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", uzicli.Exitf(uzicli.ExitUsage, "--prompt-file needs a path (use '-' to read stdin)")
	}
	if path == "-" {
		if env.Stdin == nil {
			return "", uzicli.Exitf(uzicli.ExitUsage, "no stdin to read the prompt from")
		}
		return readBoundedText(env.Stdin, "the prompt on stdin", maxJobPromptRead)
	}
	return readRegularFile(path, "prompt file", maxJobPromptRead)
}

// readJobInputs parses every --input name=@file, mirroring the server's rules client-side.
func readJobInputs(specs []string) ([]apitypes.V1JobInputDTO, error) {
	if len(specs) > maxJobInputs {
		return nil, uzicli.Exitf(uzicli.ExitUsage, "at most %d --input values are allowed", maxJobInputs)
	}
	out := make([]apitypes.V1JobInputDTO, 0, len(specs))
	seen := map[string]bool{}
	total := 0
	for _, spec := range specs {
		name, path, ok := strings.Cut(spec, "=@")
		if !ok || path == "" {
			return nil, uzicli.Exitf(uzicli.ExitUsage, "--input must look like name=@file, got %q", uzicli.CellText(spec))
		}
		if !jobInputNameRE.MatchString(name) || strings.Contains(name, "..") {
			return nil, uzicli.Exitf(uzicli.ExitUsage,
				"input name %q must be 1-%d characters of letters, digits, '.', '_' or '-', start with a letter or digit and not contain '..'",
				uzicli.CellText(name), jobInputNameMaxRune)
		}
		if seen[name] {
			return nil, uzicli.Exitf(uzicli.ExitUsage, "input name %q is given more than once", name)
		}
		seen[name] = true
		content, err := readRegularFile(path, "input file for "+name, maxJobInputTotal)
		if err != nil {
			return nil, err
		}
		total += len(content)
		if total > maxJobInputTotal {
			return nil, uzicli.Exitf(uzicli.ExitUsage, "total input content must be at most %d bytes", maxJobInputTotal)
		}
		out = append(out, apitypes.V1JobInputDTO{Name: name, Content: content})
	}
	return out, nil
}

// readRegularFile reads a local file the user named, refusing anything that is not a regular
// file (a FIFO or device could block or never end) and anything over max bytes or not UTF-8.
// The open is non-blocking, so a path swapped to a FIFO between the stat and the open cannot
// hang the open itself; the fstat on the opened handle then refuses it.
func readRegularFile(path, what string, max int64) (string, error) {
	if fi, err := os.Stat(path); err != nil {
		return "", uzicli.Exitf(uzicli.ExitUsage, "cannot read %s: %v", what, err)
	} else if !fi.Mode().IsRegular() {
		return "", uzicli.Exitf(uzicli.ExitUsage, "%s %q is not a regular file", what, path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNonblock, 0) //nolint:gosec // G304: the operator's own argument to their local CLI.
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitUsage, "cannot read %s: %v", what, err)
	}
	defer func() { _ = f.Close() }()
	// Re-check on the opened handle: a swap after the stat is caught here.
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", uzicli.Exitf(uzicli.ExitUsage, "%s %q is not a regular file", what, path)
	}
	return readBoundedText(f, what, max)
}

// readBoundedText reads at most max bytes; more is a usage error, never a silent truncation.
func readBoundedText(r io.Reader, what string, max int64) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitGeneric, "reading %s: %v", what, err)
	}
	if int64(len(b)) > max {
		return "", uzicli.Exitf(uzicli.ExitUsage, "%s is larger than %d bytes", what, max)
	}
	if !utf8.Valid(b) {
		return "", uzicli.Exitf(uzicli.ExitUsage, "%s is not valid UTF-8 text", what)
	}
	return string(b), nil
}
