package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// maxByteOffset is the largest exact integer a JSON double carries (2^53-1).
const maxByteOffset = 9007199254740991

// byteOffset parses a zero-based byte offset with the same JSON-number grammar
// as rangeInteger, additionally accepting a zero value ("0", "0.0", "0e5").
func byteOffset(raw json.RawMessage) (int64, error) {
	text := strings.TrimSpace(string(raw))
	var number json.Number
	if len(text) > 0 && text[0] == '0' && json.Unmarshal([]byte(text), &number) == nil {
		mantissa := text
		if i := strings.IndexAny(text, "eE"); i >= 0 {
			mantissa = text[:i]
		}
		if strings.Trim(mantissa, "0.") == "" {
			return 0, nil
		}
	}
	return rangeInteger(raw, maxByteOffset)
}

// identOf renders the opening fstat identity: dev:ino:mtime_ns:ctime_ns.
func identOf(st *unix.Stat_t) string {
	return fmt.Sprintf("%d:%d:%d:%d", uint64(st.Dev), uint64(st.Ino),
		int64(st.Mtim.Sec)*1_000_000_000+int64(st.Mtim.Nsec),
		int64(st.Ctim.Sec)*1_000_000_000+int64(st.Ctim.Nsec))
}

// doReadBytes returns a positioned byte window [offset, offset+limit) of a
// regular file. size and ident come from the opening descriptor's fstat; a
// mutation between that fstat and the pread is not detected (the response
// reflects the fstat sample). The deadline is cooperative: a blocking pread is
// not interrupted, the context is checked after every blocking step.
func (s *server) doReadBytes(req request) map[string]any {
	offset, err := byteOffset(req.Offset)
	if err != nil {
		return errResp(req.ID, codeMalformed)
	}
	limit, err := rangeInteger(req.Limit, maxReadBytes)
	if err != nil {
		return errResp(req.ID, codeMalformed)
	}
	if err := validatePath(req.Path); err != nil {
		return errResp(req.ID, classify(err))
	}
	ctx, cancel := s.rangeContext()
	defer cancel()
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	fd, err := s.openBeneath(req.Path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		if ctx.Err() != nil {
			return errResp(req.ID, codeTimeout)
		}
		return errResp(req.ID, classify(err))
	}
	f := os.NewFile(uintptr(fd), "read_bytes")
	if f == nil {
		unix.Close(fd)
		return errResp(req.ID, codeIO)
	}
	defer s.fileClose(f)
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	var st unix.Stat_t
	err = s.fstat(fd, &st)
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	if err != nil {
		return errResp(req.ID, classify(err))
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			return errResp(req.ID, codeIsDir)
		}
		return errResp(req.ID, codeNotFile)
	}
	err = s.nonblock(fd, false)
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	if err != nil {
		return errResp(req.ID, codeIO)
	}
	size := int64(st.Size)
	n := min(limit, max(int64(0), size-offset))
	buf := make([]byte, n)
	var got int64
	for got < n {
		c, rerr := s.fileReadAt(f, buf[got:], offset+got)
		if ctx.Err() != nil {
			return errResp(req.ID, codeTimeout)
		}
		if c < 0 || int64(c) > n-got {
			return errResp(req.ID, codeIO)
		}
		got += int64(c)
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			if c == 0 && errors.Is(rerr, unix.EINTR) {
				continue
			}
			return errResp(req.ID, codeIO)
		}
		if c == 0 {
			return errResp(req.ID, codeIO)
		}
	}
	if ctx.Err() != nil {
		return errResp(req.ID, codeTimeout)
	}
	return okResp(req.ID, map[string]any{
		"data":   base64.StdEncoding.EncodeToString(buf[:got]),
		"size":   size,
		"offset": offset,
		"ident":  identOf(&st),
	})
}
