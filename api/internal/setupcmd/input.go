package setupcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workertmpl"
)

// Input bounds. Inputs are operator-local but still bounded.
const (
	// MaxTokenBytes is the most the token source may deliver; one more byte is
	// read to detect an oversized source, then the read stops.
	MaxTokenBytes = 16 * 1024
	// MaxRepos is the most nonempty repo paths one invocation accepts.
	MaxRepos = 200
	// MaxRepoInputBytes bounds the raw --repos value.
	MaxRepoInputBytes = 64 * 1024
	// maxRepoPathBytes bounds one path_with_namespace.
	maxRepoPathBytes = 1024
	// maxWorkerNameBytes mirrors the handler's worker-name limit.
	maxWorkerNameBytes = 200
)

// errHelp reports that -h was requested; Run prints usage and exits 0.
var errHelp = errors.New("help requested")

// input is the validated, normalised command line.
type input struct {
	forgeType      string
	baseURL        string
	repos          []string
	tokenFile      string
	workerName     string
	workerTemplate string
}

// parseArgs parses and validates the flags. It writes flag diagnostics to
// stderr only. Everything it can check without the environment is checked
// here, so an invalid command line fails before any I/O.
func parseArgs(args []string, stderr io.Writer) (input, error) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	forgeType := fs.String("forge-type", "", "forge type: gitlab, forgejo or github (required)")
	baseURL := fs.String("forge-base-url", "", "forge base URL, must be in FORGE_ALLOWED_BASE_URLS (required)")
	repos := fs.String("repos", "", "comma-separated path_with_namespace values to enable")
	tokenFile := fs.String("forge-token-file", "", "file holding the forge bot token; - reads stdin (required)")
	workerName := fs.String("worker-name", "", "name of the first worker to create")
	workerTemplate := fs.String("worker-template", "", "worker template; requires --worker-name")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return input{}, errHelp
		}
		return input{}, fmt.Errorf("invalid command line: %w", err)
	}
	if fs.NArg() > 0 {
		return input{}, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	var in input
	in.forgeType = strings.TrimSpace(*forgeType)
	switch forge.Type(in.forgeType) {
	case forge.TypeGitLab, forge.TypeForgejo, forge.TypeGitHub:
	case "":
		return input{}, errors.New("--forge-type is required (gitlab, forgejo or github); there is no default and no detection")
	default:
		return input{}, errors.New("--forge-type must be gitlab, forgejo or github")
	}
	in.baseURL = strings.TrimSpace(*baseURL)
	if in.baseURL == "" {
		return input{}, errors.New("--forge-base-url is required")
	}
	in.tokenFile = strings.TrimSpace(*tokenFile)
	if in.tokenFile == "" {
		return input{}, errors.New("--forge-token-file is required (a path, or - for stdin); the token is never taken from a flag value or the environment")
	}

	paths, err := parseRepos(*repos)
	if err != nil {
		return input{}, err
	}
	in.repos = paths

	in.workerName = strings.TrimSpace(*workerName)
	in.workerTemplate = strings.TrimSpace(*workerTemplate)
	if in.workerTemplate != "" && in.workerName == "" {
		return input{}, errors.New("--worker-template requires --worker-name")
	}
	if *workerName != "" {
		if in.workerName == "" || len(in.workerName) > maxWorkerNameBytes {
			return input{}, errors.New("--worker-name must be non-empty and at most 200 bytes")
		}
		if err := termsafe.Validate("--worker-name", in.workerName); err != nil {
			return input{}, err
		}
	}
	if in.workerTemplate != "" && !workertmpl.Valid(in.workerTemplate) {
		return input{}, errors.New("--worker-template is not a known worker template")
	}
	return in, nil
}

// parseRepos splits, trims, validates and de-duplicates the --repos value,
// keeping first-seen order.
func parseRepos(raw string) ([]string, error) {
	if len(raw) > MaxRepoInputBytes {
		return nil, fmt.Errorf("--repos is larger than %d bytes", MaxRepoInputBytes)
	}
	var out []string
	seen := map[string]struct{}{}
	for _, part := range strings.Split(raw, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if len(p) > maxRepoPathBytes {
			return nil, fmt.Errorf("a --repos entry is longer than %d bytes", maxRepoPathBytes)
		}
		if err := termsafe.Validate("--repos entry", p); err != nil {
			return nil, err
		}
		if strings.ContainsFunc(p, unicode.IsSpace) || !strings.Contains(p, "/") || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
			return nil, fmt.Errorf("--repos entry %q is not a path_with_namespace such as acme/app", p)
		}
		if _, dup := seen[p]; dup {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
		if len(out) > MaxRepos {
			return nil, fmt.Errorf("--repos names more than %d repos", MaxRepos)
		}
	}
	return out, nil
}

// readToken reads the forge token from the file named by path, or from stdin
// for "-". It reads at most MaxTokenBytes+1 bytes and returns an error without
// echoing any of the content. A cancelled ctx interrupts a blocked stdin read.
func readToken(ctx context.Context, path string, stdin io.Reader) (string, error) {
	src := stdin
	if path != "-" {
		f, err := openRegularFile(path)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		src = f
	}
	raw, err := readBounded(ctx, src, MaxTokenBytes)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(raw))
	if token == "" {
		return "", errors.New("the token source is empty")
	}
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", errors.New("the token must be one line without whitespace or control characters")
		}
	}
	return token, nil
}

// openRegularFile opens the token file without blocking and refuses anything
// that is not a regular file. A plain open of a FIFO with no writer blocks in
// the kernel before any context-aware read can start, so the open is
// non-blocking (O_NONBLOCK has no effect on a regular file) and the type is
// checked on the opened descriptor, which a path swap cannot change.
func openRegularFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: the operator names the token file on purpose
	if err != nil {
		return nil, fmt.Errorf("cannot open the token file: %w", errors.Unwrap(err))
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, errors.New("the token file must be a regular file; use - to read the token from stdin")
	}
	return f, nil
}

// readBounded reads from r until EOF or limit+1 bytes, whichever comes first,
// and rejects a source larger than limit. The read runs in a goroutine so a
// cancelled ctx returns at once even while r blocks; that goroutine ends when
// r delivers or closes (the process exits right after in production).
func readBounded(ctx context.Context, r io.Reader, limit int64) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(io.LimitReader(r, limit+1))
		ch <- result{b, err}
	}()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("reading the token source: %w", ctx.Err())
	case res := <-ch:
		if res.err != nil {
			return nil, errors.New("reading the token source failed")
		}
		if int64(len(res.b)) > limit {
			return nil, fmt.Errorf("the token source is larger than %d bytes", limit)
		}
		return res.b, nil
	}
}
