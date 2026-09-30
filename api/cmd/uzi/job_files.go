package main

// job_files.go holds the job-file verbs (PRD #1909 M7): `uzi job files <job-id>` lists a job's
// input and output files, `uzi job file get <file-id>` downloads one, and uploadJobFiles backs
// `uzi job create --file`. Every server-supplied string (display names, source URLs, refusal
// reasons) is untrusted display text and goes through the Printer's table cells (CellText) or
// cellText; --json prints the raw DTO, which encoding/json already escapes. A downloaded file's
// bytes are untrusted data too: they are written to disk and never printed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// storageNameRE is the shape of the content-derived storage name the server sends in
// Content-Disposition: `<sha256>` plus an optional short extension. Anything else is not used as
// a local file name.
var storageNameRE = regexp.MustCompile(`^([0-9a-f]{64})(\.[A-Za-z0-9]{1,8})?$`)

func newJobFilesCmd(env Env, gf *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "files <job-id>",
		Short: "List a job's input and output files",
		Long: "List the files of one job: the input files attached to it and the output files it " +
			"produced, with direction, size, short sha256, state, expiry and (for an output " +
			"fetched from a site during the job) the source URL, then the outputs that were not " +
			"stored and why. Download one with `job file get <file-id>`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.jobClient(gf)
			if err != nil {
				return err
			}
			d, err := c.JobFiles(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(d)
			}
			return renderJobFiles(p, d)
		},
	}
}

func renderJobFiles(p *uzicli.Printer, d apitypes.V1JobFilesDTO) error {
	if len(d.Files) == 0 && len(d.RefusedFiles) == 0 {
		p.Println("no files")
		return nil
	}
	if len(d.Files) > 0 {
		rows := make([][]string, 0, len(d.Files))
		for _, f := range d.Files {
			src := "-"
			if f.SourceURL != nil && *f.SourceURL != "" {
				src = *f.SourceURL
			}
			rows = append(rows, []string{
				f.ID, f.DisplayName, f.Direction, strconv.FormatInt(f.ByteSize, 10),
				fileSHACell(f.Sha256), f.State, tsCell(f.ExpiresAt), src,
			})
		}
		if err := p.Table([]string{"ID", "NAME", "DIRECTION", "SIZE", "SHA256", "STATE", "EXPIRES", "SOURCE"}, rows); err != nil {
			return err
		}
	}
	if len(d.RefusedFiles) > 0 {
		if len(d.Files) > 0 {
			p.Println()
		}
		p.Println("REFUSED (not stored)")
		rows := make([][]string, 0, len(d.RefusedFiles))
		for _, r := range d.RefusedFiles {
			rows = append(rows, []string{r.DisplayName, strconv.FormatInt(r.ByteSize, 10), r.Reason})
		}
		return p.Table([]string{"NAME", "SIZE", "REASON"}, rows)
	}
	return nil
}

// fileSHACell is a file's short digest, "-" when the row keeps none (an expired file).
func fileSHACell(s string) string {
	if s == "" {
		return "-"
	}
	return shortSHA(s)
}

func newJobFileCmd(env Env, gf *globalFlags) *cobra.Command {
	file := &cobra.Command{
		Use:   "file",
		Short: "Download a job's file",
		Long:  "Work with one file of a job. List a job's files with `job files <job-id>`.",
	}
	file.AddCommand(newJobFileGetCmd(env, gf))
	return file
}

func newJobFileGetCmd(env Env, gf *globalFlags) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "get <file-id> [-o <path>]",
		Short: "Download a file",
		Long: "Download one file by id (list ids with `job files`). It is written to -o <path>, or by " +
			"default to its storage name (<sha256>.<ext>) in the current directory. An existing " +
			"file is never overwritten: the command refuses instead. The bytes are streamed to a " +
			"temporary file next to the target and renamed into place only when complete; when the " +
			"storage name carries the file's sha256 the digest is verified. The bytes are " +
			"untrusted data: open them with care. An expired file is reported as expired.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := env.jobClient(gf)
			if err != nil {
				return err
			}
			dl, err := c.DownloadFile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			defer func() { _ = dl.Body.Close() }()
			target := out
			if !cmd.Flags().Changed("output") {
				if !storageNameRE.MatchString(dl.StorageName) {
					return uzicli.Exitf(uzicli.ExitGeneric, "the server sent no usable file name; pass -o <path>")
				}
				target = dl.StorageName
			} else if strings.TrimSpace(out) == "" {
				return uzicli.Exitf(uzicli.ExitUsage, "-o needs a path")
			}
			n, sum, err := saveDownload(dl, target)
			if err != nil {
				return err
			}
			p := env.printer(gf)
			if p.Format == uzicli.FormatJSON {
				return p.JSON(map[string]any{"path": target, "bytes": n, "sha256": sum})
			}
			p.Printf("saved %s (%d bytes, sha256 %s)\n", cellText(target), n, sum)
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "write the file here (default: its storage name in the current directory)")
	return cmd
}

// saveDownload streams dl to target without ever replacing an existing file: the name is
// reserved with O_EXCL first, the bytes go to a temp file in the same directory, and the temp is
// renamed over the reservation only after the size and, when the storage name carries one, the
// sha256 check out. Any failure removes both the temp and the reservation.
func saveDownload(dl *uzicli.FileDownload, target string) (int64, string, error) {
	dir := filepath.Dir(target)
	placeholder, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator's own -o argument to their local CLI.
	if err != nil {
		if os.IsExist(err) {
			return 0, "", uzicli.Exitf(uzicli.ExitUsage, "%q already exists: refusing to overwrite it (choose another -o path or remove it)", target)
		}
		return 0, "", uzicli.Exitf(uzicli.ExitGeneric, "cannot create %q: %v", target, err)
	}
	_ = placeholder.Close()
	done := false
	tmpPath := ""
	defer func() {
		if !done {
			_ = os.Remove(target)
			if tmpPath != "" {
				_ = os.Remove(tmpPath)
			}
		}
	}()
	tmp, err := os.CreateTemp(dir, ".uzi-download-*")
	if err != nil {
		return 0, "", uzicli.Exitf(uzicli.ExitGeneric, "cannot write next to %q: %v", target, err)
	}
	tmpPath = tmp.Name()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), dl.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, "", uzicli.Exitf(uzicli.ExitUnreachable, "download failed: %v", err)
	}
	if dl.Size >= 0 && n != dl.Size {
		return 0, "", uzicli.Exitf(uzicli.ExitUnreachable, "download incomplete: got %d of %d bytes", n, dl.Size)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if m := storageNameRE.FindStringSubmatch(dl.StorageName); m != nil && m[1] != sum {
		return 0, "", uzicli.Exitf(uzicli.ExitGeneric, "downloaded bytes do not match the file's sha256 (%s, expected %s): not saved", sum, m[1])
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return 0, "", uzicli.Exitf(uzicli.ExitGeneric, "cannot move the download into place: %v", err)
	}
	done = true
	return n, sum, nil
}

// uploadJobFiles uploads every --file path and returns the stored file ids, in order. Each file
// is hashed first (the server wants the digest before the body), then streamed. The first failure
// stops the run; files already uploaded stay unattached and expire on their own.
func uploadJobFiles(ctx context.Context, c uzicli.Client, paths []string) ([]string, error) {
	ids := make([]string, 0, len(paths))
	for _, path := range paths {
		id, err := uploadJobFile(ctx, c, path)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func uploadJobFile(ctx context.Context, c uzicli.Client, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", uzicli.Exitf(uzicli.ExitUsage, "--file needs a path")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitUsage, "cannot read file %q: %v", cellText(path), err)
	}
	if !fi.Mode().IsRegular() {
		return "", uzicli.Exitf(uzicli.ExitUsage, "file %q is not a regular file", cellText(path))
	}
	if fi.Size() == 0 {
		return "", uzicli.Exitf(uzicli.ExitUsage, "file %q is empty", cellText(path))
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNonblock, 0) //nolint:gosec // G304: the operator's own argument to their local CLI.
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitUsage, "cannot read file %q: %v", cellText(path), err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", uzicli.Exitf(uzicli.ExitGeneric, "reading file %q: %v", cellText(path), err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", uzicli.Exitf(uzicli.ExitGeneric, "reading file %q: %v", cellText(path), err)
	}
	up, err := c.UploadFile(ctx, filepath.Base(path), size, hex.EncodeToString(h.Sum(nil)), io.LimitReader(f, size))
	if err != nil {
		return "", fmt.Errorf("uploading %s: %w", cellText(filepath.Base(path)), err)
	}
	return up.ID, nil
}
