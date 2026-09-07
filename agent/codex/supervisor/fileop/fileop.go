package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// resolveFlags is the KERNEL-enforced containment for every model-selected path.
// RESOLVE_BENEATH rejects an absolute pathname or any ".." that would ascend
// above the worktree-root dirfd (EXDEV); RESOLVE_NO_SYMLINKS refuses to follow
// ANY symbolic-link component, final or intermediate (ELOOP); RESOLVE_NO_MAGICLINKS
// refuses /proc-style magic links (ELOOP). Because openat2 resolves the whole path
// atomically against the anchored dirfd, there is no check-then-use window: a
// symlink/".."/magic-link swapped in mid-resolution cannot race the open.
const resolveFlags = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS

// Bounds on every untrusted-input surface. Each is a hard cap: an oversized
// request line, path, or read/write payload is rejected with a typed code, never
// allocated without limit. maxRequestLine dominates because a write payload rides
// base64-encoded inside one JSON line.
const (
	maxRequestLine = 2 * 1024 * 1024 // largest accepted request frame
	maxPathLen     = 4096            // PATH_MAX; a longer relpath is oversize
	maxReadBytes   = 1 * 1024 * 1024 // largest file body a read returns
	maxWriteBytes  = 1 * 1024 * 1024 // largest body a write accepts
	maxListEntries = 4096            // largest directory listing returned
)

// Operation names carried on the request channel (stdin).
const (
	opStat   = "stat"
	opRead   = "read"
	opWrite  = "write"
	opMkdir  = "mkdir"
	opRename = "rename"
	opUnlink = "unlink"
	opRmdir  = "rmdir"
	opList   = "list"
)

// Stable, bounded error-code vocabulary. A response NEVER carries a raw errno
// string, path, or file content: the model-visible failure is always one of these
// fixed tokens. New failure modes must map to one of these, not leak detail.
const (
	codeOversize  = "E_OVERSIZE"   // request line, path, or read/write body over a cap
	codeMalformed = "E_MALFORMED"  // unparseable JSON, bad base64, or an invalid component
	codeUnknownOp = "E_UNKNOWN_OP" // op not in the closed set
	codeDenied    = "E_DENIED"     // policy denial (any .git path component)
	codeEscape    = "E_ESCAPE"     // absolute path / ".." component (policy or openat2 EXDEV)
	codeSymlink   = "E_SYMLINK"    // openat2 refused a symlink/magic-link component (ELOOP)
	codeNotFound  = "E_NOT_FOUND"  // ENOENT
	codeExists    = "E_EXISTS"     // EEXIST
	codeNotDir    = "E_NOT_DIR"    // ENOTDIR
	codeIsDir     = "E_IS_DIR"     // EISDIR
	codeNotFile   = "E_NOT_FILE"   // read/write target is a FIFO/socket/device, not a regular file
	codeNotEmpty  = "E_NOT_EMPTY"  // ENOTEMPTY (rmdir of a non-empty dir)
	codePerm      = "E_PERM"       // EACCES/EPERM
	codeInternal  = "E_INTERNAL"   // a response could not be encoded
	codeIO        = "E_IO"         // any other bounded I/O failure
)

// Internal rejection sentinels, mapped to a code by classify. Kept distinct so
// path-policy failures never depend on a syscall ever running.
var (
	errMalformed    = errors.New("malformed")
	errTooLong      = errors.New("path too long")
	errDenied       = errors.New("denied")
	errEscape       = errors.New("escape")
	errBadComponent = errors.New("bad component")
	errBadArgs      = errors.New("bad args")
)

// request is one parsed, validated operation frame. NewPath is meaningful only for
// rename; Data is base64-encoded bytes for write.
type request struct {
	ID      int    `json:"id"`
	Op      string `json:"op"`
	Path    string `json:"path"`
	NewPath string `json:"newPath"`
	Data    string `json:"data"`
}

// server holds the worktree-root dirfd opened ONCE (O_DIRECTORY|O_PATH) and the
// per-op bounds. Bounds are fields (not bare consts) so tests can drive the same
// production code paths with small caps; newServer wires the production defaults.
type server struct {
	rootFD   int
	maxRead  int
	maxWrite int
	lineMax  int
}

// newServer builds a server anchored at rootFD with the production bounds.
func newServer(rootFD int) *server {
	return &server{
		rootFD:   rootFD,
		maxRead:  maxReadBytes,
		maxWrite: maxWriteBytes,
		lineMax:  maxRequestLine,
	}
}

// serve runs the newline-delimited-JSON request/response loop: one request per
// line on r, one response per line on w. Unlike the trusted control protocol, a
// malformed or oversized request is a per-request typed error and the server STAYS
// HEALTHY (it resynchronizes to the next newline and keeps serving); only a
// transport (writer) failure or clean EOF ends the loop.
func serve(s *server, r io.Reader, w io.Writer) error {
	lr := &lineReader{r: r, max: s.lineMax}
	for {
		line, over, err := lr.readLine()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if over {
			if werr := writeResponse(w, errResp(0, codeOversize)); werr != nil {
				return werr
			}
			continue
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		resp := errResp(0, codeMalformed)
		if req, perr := parseRequest(line); perr == nil {
			resp = s.handle(req)
		}
		if werr := writeResponse(w, resp); werr != nil {
			return werr
		}
	}
}

// parseRequest decodes one request line. A JSON failure is the only thing that
// loses the id; every op/path rejection is handled downstream WITH the id echoed.
func parseRequest(line []byte) (request, error) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		return request{}, errMalformed
	}
	return req, nil
}

// writeResponse marshals one response map to a single NDJSON line. A marshal
// failure degrades to a static internal-error line rather than leaking detail.
func writeResponse(w io.Writer, m map[string]any) error {
	payload, err := json.Marshal(m)
	if err != nil {
		payload, _ = json.Marshal(errResp(0, codeInternal))
	}
	_, err = w.Write(append(payload, '\n'))
	return err
}

// handle dispatches one validated request to its primitive. Every branch returns
// a bounded response map; an unknown op is a typed error with the id echoed.
func (s *server) handle(req request) map[string]any {
	switch req.Op {
	case opStat:
		return s.doStat(req)
	case opRead:
		return s.doRead(req)
	case opWrite:
		return s.doWrite(req)
	case opMkdir:
		return s.doMkdir(req)
	case opRename:
		return s.doRename(req)
	case opUnlink:
		return s.doUnlink(req)
	case opRmdir:
		return s.doRmdir(req)
	case opList:
		return s.doList(req)
	default:
		return errResp(req.ID, codeUnknownOp)
	}
}

// openBeneath resolves rel against the root dirfd with the KERNEL containment
// flags. Every path-consuming operation goes through here; nothing else opens by
// pathname. O_CLOEXEC is always set so a transient fd never leaks across a fork.
func (s *server) openBeneath(rel string, flags int, mode uint32) (int, error) {
	how := &unix.OpenHow{
		Flags:   uint64(flags) | unix.O_CLOEXEC,
		Mode:    uint64(mode),
		Resolve: resolveFlags,
	}
	return unix.Openat2(s.rootFD, rel, how)
}

// resolveParent splits rel into (parent directory, final single component) and
// opens the parent beneath root as an O_PATH|O_DIRECTORY dirfd. Operations that
// openat2 cannot express directly (mkdir/rename/unlink/rmdir) run their final,
// slash-free component against this pinned dirfd, so the *at syscall operates on
// the real directory inode and cannot be redirected by a later swap. ownFD is
// false when the parent IS the root (do not close the shared root dirfd).
func (s *server) resolveParent(rel string) (parentFD int, base string, ownFD bool, err error) {
	dir, base := path.Split(rel)
	switch base {
	case "", ".":
		return -1, "", false, errBadComponent
	case "..":
		return -1, "", false, errEscape
	}
	dir = strings.TrimRight(dir, "/")
	if dir == "" {
		return s.rootFD, base, false, nil
	}
	fd, oerr := s.openBeneath(dir, unix.O_PATH|unix.O_DIRECTORY, 0)
	if oerr != nil {
		return -1, "", false, oerr
	}
	return fd, base, true, nil
}

// doStat reports existence, type, and size. A missing target is ok with
// exists:false; an escape/symlink/other failure is a typed error. A symlink is
// never "exists:false": RESOLVE_NO_SYMLINKS makes it E_SYMLINK.
func (s *server) doStat(req request) map[string]any {
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	fd, err := s.openBeneath(req.Path, unix.O_PATH, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return okResp(req.ID, map[string]any{"exists": false})
		}
		return errResp(req.ID, classify(err))
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return errResp(req.ID, classify(err))
	}
	return okResp(req.ID, map[string]any{
		"exists": true,
		"type":   fileType(st.Mode),
		"size":   int64(st.Size),
	})
}

// doRead returns a regular file's bytes, base64-encoded, bounded by maxRead. The
// LimitReader guard bounds memory even if the file grows after the stat. O_NONBLOCK
// is set on the open so a FIFO/device (whose blocking open would hang this
// single-threaded helper) returns immediately; the fstat then classifies it as
// codeIsDir (directory) or codeNotFile (FIFO/socket/device) before any read. For a
// confirmed regular file O_NONBLOCK is a no-op, but it is cleared anyway so the read
// path is a plain blocking read.
func (s *server) doRead(req request) map[string]any {
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	fd, err := s.openBeneath(req.Path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return errResp(req.ID, classify(err))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			return errResp(req.ID, codeIsDir)
		}
		return errResp(req.ID, codeNotFile)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return errResp(req.ID, codeIO)
	}
	f := os.NewFile(uintptr(fd), "read")
	if f == nil {
		unix.Close(fd)
		return errResp(req.ID, codeIO)
	}
	defer f.Close()
	if st.Size > int64(s.maxRead) {
		return errResp(req.ID, codeOversize)
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(s.maxRead)+1))
	if err != nil {
		return errResp(req.ID, codeIO)
	}
	if len(data) > s.maxRead {
		return errResp(req.ID, codeOversize)
	}
	return okResp(req.ID, map[string]any{
		"size": int64(len(data)),
		"data": base64.StdEncoding.EncodeToString(data),
	})
}

// doWrite creates-or-truncates a regular file and writes the decoded bytes. An
// oversize payload is rejected BEFORE the file is opened, so it neither creates nor
// truncates. O_NOFOLLOW plus RESOLVE_NO_SYMLINKS refuse a symlink final. O_TRUNC is
// deliberately NOT set on the open: truncation must not act on a special file, so the
// open uses O_NONBLOCK (a readerless FIFO/socket then fails fast at open with ENXIO
// instead of blocking this single-threaded helper), the fstat confirms a regular file
// (else codeNotFile, with nothing written or truncated), and only then does an
// explicit Ftruncate(fd, 0) reset the length before the write.
func (s *server) doWrite(req request) map[string]any {
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	data, derr := base64.StdEncoding.DecodeString(req.Data)
	if derr != nil {
		return errResp(req.ID, codeMalformed)
	}
	if len(data) > s.maxWrite {
		return errResp(req.ID, codeOversize)
	}
	fd, err := s.openBeneath(req.Path, unix.O_WRONLY|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o644)
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		unix.Close(fd)
		return errResp(req.ID, classify(err))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		unix.Close(fd)
		return errResp(req.ID, codeNotFile)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		unix.Close(fd)
		return errResp(req.ID, codeIO)
	}
	if err := unix.Ftruncate(fd, 0); err != nil {
		unix.Close(fd)
		return errResp(req.ID, codeIO)
	}
	f := os.NewFile(uintptr(fd), "write")
	if f == nil {
		unix.Close(fd)
		return errResp(req.ID, codeIO)
	}
	defer f.Close()
	if _, werr := f.Write(data); werr != nil {
		return errResp(req.ID, codeIO)
	}
	return okResp(req.ID, map[string]any{"size": int64(len(data))})
}

// doMkdir creates one directory beneath root.
func (s *server) doMkdir(req request) map[string]any {
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	parent, base, own, err := s.resolveParent(req.Path)
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	if own {
		defer unix.Close(parent)
	}
	if err := unix.Mkdirat(parent, base, 0o755); err != nil {
		return errResp(req.ID, classify(err))
	}
	return okResp(req.ID, nil)
}

// doRename moves old to new with BOTH parents resolved beneath root. Renameat2
// operates on slash-free final components against the pinned dirfds, so neither
// endpoint can escape via a swapped component.
func (s *server) doRename(req request) map[string]any {
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	if err := validatePath(req.NewPath); err != nil {
		return errResp(req.ID, classify(err))
	}
	oParent, oBase, oOwn, err := s.resolveParent(req.Path)
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	if oOwn {
		defer unix.Close(oParent)
	}
	nParent, nBase, nOwn, err := s.resolveParent(req.NewPath)
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	if nOwn {
		defer unix.Close(nParent)
	}
	if err := unix.Renameat2(oParent, oBase, nParent, nBase, 0); err != nil {
		return errResp(req.ID, classify(err))
	}
	return okResp(req.ID, nil)
}

// doUnlink removes one non-directory entry beneath root.
func (s *server) doUnlink(req request) map[string]any {
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	parent, base, own, err := s.resolveParent(req.Path)
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	if own {
		defer unix.Close(parent)
	}
	if err := unix.Unlinkat(parent, base, 0); err != nil {
		return errResp(req.ID, classify(err))
	}
	return okResp(req.ID, nil)
}

// doRmdir removes one empty directory beneath root.
func (s *server) doRmdir(req request) map[string]any {
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	parent, base, own, err := s.resolveParent(req.Path)
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	if own {
		defer unix.Close(parent)
	}
	if err := unix.Unlinkat(parent, base, unix.AT_REMOVEDIR); err != nil {
		return errResp(req.ID, classify(err))
	}
	return okResp(req.ID, nil)
}

// doList reads a directory beneath root, returning bounded (name, type) entries.
// A symlink entry is reported as type "symlink" (the broker decides); the helper
// still refuses to FOLLOW it on any subsequent open.
func (s *server) doList(req request) map[string]any {
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	fd, err := s.openBeneath(req.Path, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	f := os.NewFile(uintptr(fd), "list")
	if f == nil {
		unix.Close(fd)
		return errResp(req.ID, codeIO)
	}
	defer f.Close()
	des, rerr := f.ReadDir(maxListEntries + 1)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return errResp(req.ID, classify(rerr))
	}
	truncated := false
	if len(des) > maxListEntries {
		des = des[:maxListEntries]
		truncated = true
	}
	entries := make([]map[string]any, 0, len(des))
	for _, de := range des {
		entries = append(entries, map[string]any{
			"name": de.Name(),
			"type": direntType(de),
		})
	}
	return okResp(req.ID, map[string]any{
		"entries":   entries,
		"truncated": truncated,
	})
}

// validatePath applies the pathname policy that does not need a syscall, screening
// EVERY slash-delimited component rather than only the first. It rejects: an empty
// path (E_MALFORMED) or an over-length one (E_OVERSIZE); a leading-slash absolute
// path (E_ESCAPE); any ".." component (E_ESCAPE); any empty or "." interior
// component such as "a//b" or "a/./b" (E_MALFORMED); and any ".git" component,
// nested or not (E_DENIED). A bare "." is allowed as the anchor itself.
//
// The per-component ".." and ".git" screens are load-bearing, not cosmetic: the
// KERNEL containment via openBeneath uses RESOLVE_BENEATH, which PERMITS ".." as long
// as resolution stays beneath the root. So the kernel alone would let "sub/../.git"
// resolve to the real ".git" beneath root; a first-component-only check saw "sub" and
// passed it. Screening ".." (unnecessary given RESOLVE_BENEATH anyway) and every
// ".git" component here closes that bypass and mirrors the TS broker's screen for the
// repo git dir and any nested/submodule ".git".
func validatePath(p string) error {
	if p == "" {
		return errMalformed
	}
	if len(p) > maxPathLen {
		return errTooLong
	}
	if p[0] == '/' {
		return errEscape
	}
	if p == "." {
		return nil
	}
	for _, comp := range strings.Split(p, "/") {
		switch comp {
		case "", ".":
			return errMalformed
		case "..":
			return errEscape
		case ".git":
			return errDenied
		}
	}
	return nil
}

// classify maps any failure to a stable, bounded code. Internal sentinels are
// checked first so a policy rejection never depends on a syscall having run.
func classify(err error) string {
	switch {
	case err == nil:
		return codeInternal
	case errors.Is(err, errMalformed), errors.Is(err, errBadComponent):
		return codeMalformed
	case errors.Is(err, errTooLong):
		return codeOversize
	case errors.Is(err, errDenied):
		return codeDenied
	case errors.Is(err, errEscape), errors.Is(err, unix.EXDEV):
		return codeEscape
	case errors.Is(err, unix.ELOOP):
		return codeSymlink
	case errors.Is(err, unix.ENOENT):
		return codeNotFound
	case errors.Is(err, unix.EEXIST):
		return codeExists
	case errors.Is(err, unix.ENOTEMPTY):
		return codeNotEmpty
	case errors.Is(err, unix.ENOTDIR):
		return codeNotDir
	case errors.Is(err, unix.EISDIR):
		return codeIsDir
	case errors.Is(err, unix.ENXIO):
		// A readerless FIFO or a socket file rejected at open with O_NONBLOCK: it is
		// not a regular file, so it maps to the same code the fstat type check yields.
		return codeNotFile
	case errors.Is(err, unix.ENAMETOOLONG):
		return codeOversize
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return codePerm
	default:
		return codeIO
	}
}

// fileType renders a stat mode as a stable type token.
func fileType(mode uint32) string {
	switch mode & unix.S_IFMT {
	case unix.S_IFREG:
		return "file"
	case unix.S_IFDIR:
		return "dir"
	case unix.S_IFLNK:
		return "symlink"
	default:
		return "other"
	}
}

// direntType renders a directory entry's type as a stable token from its d_type.
func direntType(de os.DirEntry) string {
	m := de.Type()
	switch {
	case m.IsDir():
		return "dir"
	case m&os.ModeSymlink != 0:
		return "symlink"
	case m.IsRegular():
		return "file"
	default:
		return "other"
	}
}

// okResp builds a success response, merging any op-specific fields.
func okResp(id int, extra map[string]any) map[string]any {
	m := map[string]any{"id": id, "ok": true}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// errResp builds a typed-error response carrying only a bounded code.
func errResp(id int, code string) map[string]any {
	return map[string]any{"id": id, "ok": false, "code": code}
}

// lineReader frames newline-delimited requests under a byte ceiling. Unlike the
// control reader, an over-length line does NOT end the session: readLine drains
// bytes up to the next newline and reports over=true so serve can emit E_OVERSIZE
// and keep going. Memory stays bounded to max+one chunk while draining.
type lineReader struct {
	r    io.Reader
	max  int
	buf  []byte
	over bool
}

// readLine returns the next frame (without its newline). It returns over=true for
// an over-length frame (with buf resynchronized past the terminator), or io.EOF
// at a clean end (a partial unterminated trailer is dropped as end-of-stream).
func (lr *lineReader) readLine() ([]byte, bool, error) {
	for {
		if i := bytes.IndexByte(lr.buf, '\n'); i >= 0 {
			line := lr.buf[:i]
			lr.buf = append([]byte(nil), lr.buf[i+1:]...)
			if lr.over || len(line) > lr.max {
				lr.over = false
				return nil, true, nil
			}
			return line, false, nil
		}
		if !lr.over && len(lr.buf) > lr.max {
			lr.over = true
		}
		if lr.over {
			lr.buf = lr.buf[:0]
		}
		chunk := make([]byte, 4096)
		n, err := lr.r.Read(chunk)
		if n > 0 {
			lr.buf = append(lr.buf, chunk[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				if lr.over {
					lr.over = false
					return nil, true, nil
				}
				lr.buf = nil
				return nil, false, io.EOF
			}
			return nil, false, err
		}
	}
}
