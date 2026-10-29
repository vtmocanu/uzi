package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// doReadTextRange scans the observed descriptor through EOF, including omitted
// bytes. Storage is bounded by maxExcerpt, a 4096-byte reader and one rune.
// A scan consumes at most maxScan+1 bytes; failure ends this request only.
// Concurrent changes have no snapshot guarantee.
func (s *server) doReadTextRange(req request) map[string]any {
	if req.Offset < 1 || req.Offset > 9007199254740991 || req.Limit < 1 || req.Limit > 2000 {
		return errResp(req.ID, codeMalformed)
	}
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	fd, err := s.openBeneath(req.Path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	f := os.NewFile(uintptr(fd), "read_text_range")
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return errResp(req.ID, classify(err))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			return errResp(req.ID, codeIsDir)
		}
		return errResp(req.ID, codeNotFile)
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		return errResp(req.ID, codeIO)
	}
	s.afterRangeStat()
	return s.scanTextRange(req, f)
}

// scanTextRange never accumulates a source line or an index. Once the selected
// excerpt fills, later runes are validated but cannot extend retained storage.
func (s *server) scanTextRange(req request, input io.Reader) map[string]any {
	reader := bufio.NewReaderSize(io.LimitReader(input, s.maxScan+1), 4096)
	out := make([]byte, 0, s.maxExcerpt)
	var size, line, lastLine int64
	line = 1
	lines := 0
	binary, stopped, partial, truncated := false, false, false, false
	for {
		r, n, err := reader.ReadRune()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return errResp(req.ID, codeIO)
		}
		size += int64(n)
		if size > s.maxScan {
			return errResp(req.ID, codeOversize)
		}
		if r == 0 || (r == utf8.RuneError && n == 1) {
			binary = true
		}
		if line >= req.Offset {
			selected := line-req.Offset < req.Limit
			if !binary && !stopped && selected && len(out)+n <= s.maxExcerpt {
				out = utf8.AppendRune(out, r)
				if lastLine != line {
					lines++
					lastLine = line
				}
			} else {
				stopped = true
				truncated = true
				if lastLine == line {
					partial = true
				}
			}
		}
		if r == '\n' {
			line++
		}
	}
	if binary {
		return okResp(req.ID, map[string]any{"kind": "binary"})
	}
	return okResp(req.ID, map[string]any{
		"kind": "text", "data": base64.StdEncoding.EncodeToString(out),
		"size": size, "offset": req.Offset, "linesReturned": lines,
		"partialLastLine": partial, "truncated": truncated,
	})
}
