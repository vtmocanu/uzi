package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestTextRangeSemantics(t *testing.T) {
	s, root := newTestServer(t)
	for _, tc := range []struct {
		text, want         string
		offset, limit      int64
		lines              int
		partial, truncated bool
	}{
		{"", "", 1, 200, 0, false, false},
		{"a\nb\nc\n", "b\n", 2, 1, 1, false, true},
		{"\ufeffé😀\r\n\n尾", "\ufeffé😀\r\n\n尾", 1, 200, 3, false, false},
		{"a\n", "a\n", 1, 1, 1, false, false},
		{"a", "a", 1, 1, 1, false, false},
		{"a\n", "", 9007199254740991, 200, 0, false, false},
		{"a\nb", "b", 2, 1, 1, false, false},
		{strings.Repeat("a", 65535) + "😀tail", strings.Repeat("a", 65535), 1, 200, 1, true, true},
		{strings.Repeat("a", 65535) + "\r\ntail", strings.Repeat("a", 65535) + "\r", 1, 200, 1, true, true},
		{strings.Repeat("a", 65535) + "\ntail", strings.Repeat("a", 65535) + "\n", 1, 200, 1, false, true},
		{strings.Repeat("a", 65536), strings.Repeat("a", 65536), 1, 200, 1, false, false},
		{strings.Repeat("x", 4095) + "😀\n", strings.Repeat("x", 4095) + "😀\n", 1, 200, 1, false, false},
	} {
		if err := os.WriteFile(filepath.Join(root, "text"), []byte(tc.text), 0600); err != nil {
			t.Fatal(err)
		}
		r := s.handle(request{Op: opReadTextRange, Path: "text", Offset: tc.offset, Limit: tc.limit})
		wantOK(t, r)
		got := respData(t, r)
		if !bytes.Equal(got, []byte(tc.want)) {
			t.Fatalf("excerpt mismatch: lengths %d/%d", len(got), len(tc.want))
		}
		if r["kind"] != "text" || r["size"] != int64(len(tc.text)) || r["offset"] != tc.offset || r["linesReturned"] != tc.lines || r["partialLastLine"] != tc.partial || r["truncated"] != tc.truncated {
			t.Fatalf("metadata mismatch for input length %d: size=%v offset=%v lines=%v partial=%v truncated=%v", len(tc.text), r["size"], r["offset"], r["linesReturned"], r["partialLastLine"], r["truncated"])
		}
	}
}

func TestTextRangeWholeInputValidation(t *testing.T) {
	s, root := newTestServer(t)
	for _, bad := range []string{"\x00", "\xff", "\xc0\xaf", "\xe2\x82"} {
		for _, text := range []string{bad + "\nok\n", "prefix\nok\n" + bad, "prefix\n" + strings.Repeat("x", 65536) + bad, strings.Repeat("a", 4095) + bad} {
			if err := os.WriteFile(filepath.Join(root, "text"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			r := s.handle(request{Op: opReadTextRange, Path: "text", Offset: 2, Limit: 1})
			wantOK(t, r)
			if len(r) != 3 || r["kind"] != "binary" {
				t.Fatalf("binary response contains excerpt metadata: keys=%d", len(r))
			}
		}
	}
}

func TestTextRangeBudgetAndGrowth(t *testing.T) {
	s, root := newTestServer(t)
	s.maxScan = 32
	s.maxExcerpt = 8
	for _, n := range []int{31, 32, 33} {
		if err := os.WriteFile(filepath.Join(root, "text"), []byte(strings.Repeat("x", n)), 0600); err != nil {
			t.Fatal(err)
		}
		r := s.handle(request{Op: opReadTextRange, Path: "text", Offset: 1, Limit: 1})
		if n > 32 {
			wantErr(t, r, codeOversize)
		} else {
			wantOK(t, r)
			if len(respData(t, r)) != 8 || r["size"] != int64(n) {
				t.Fatal("bounds")
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, "text"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	s.afterRangeStat = func() {
		if err := os.WriteFile(filepath.Join(root, "text"), []byte(strings.Repeat("g", 33)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	wantErr(t, s.handle(request{Op: opReadTextRange, Path: "text", Offset: 1, Limit: 1}), codeOversize)
	s.afterRangeStat = func() {}
	wantOK(t, s.handle(request{Op: opRead, Path: "text"}))
	// Reader synthesizes a huge line without retaining it. The scan's only
	// variable retained buffer remains maxExcerpt; the reader and rune are fixed.
	r := s.scanTextRange(request{Offset: 2, Limit: 1}, strings.NewReader(strings.Repeat("p", 30)+"\nx"))
	wantOK(t, r)
	if string(respData(t, r)) != "x" || r["truncated"] != false {
		t.Fatal("skipped prefix")
	}
}

func TestTextRangeWireArguments(t *testing.T) {
	s, _ := newTestServer(t)
	for _, args := range []string{`{}`, `{"offset":0,"limit":1}`, `{"offset":1,"limit":2001}`, `{"offset":9007199254740992,"limit":1}`, `{"offset":1.5,"limit":1}`, `{"offset":"1","limit":1}`, `{"offset":null,"limit":1}`} {
		var m map[string]any
		if err := json.Unmarshal([]byte(args), &m); err != nil {
			t.Fatal(err)
		}
		m["id"] = 1
		m["op"] = opReadTextRange
		m["path"] = "text"
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := serve(s, bytes.NewReader(append(data, '\n')), &out); err != nil {
			t.Fatal(err)
		}
		r := decodeResponses(t, &out)
		if len(r) != 1 || r[0]["ok"] != false || r[0]["code"] != codeMalformed {
			t.Fatalf("invalid arguments accepted: %s", args)
		}
	}
}

func TestTextRangePathBoundary(t *testing.T) {
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "text"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"link": "text", "intermediate": "dir", "outside": t.TempDir()} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Fatalf("socket setup unavailable: %v", err)
	}
	defer listener.Close()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{opRead, opReadTextRange} {
		t.Run(op+"/git-config", func(t *testing.T) {
			wantErr(t, s.handle(request{Op: op, Path: ".git/config", Offset: 1, Limit: 1}), codeDenied)
		})
		for _, p := range []string{"link", "intermediate/text", "outside/text", "../escape", "/absolute", "pipe", "socket", "dir"} {
			r := s.handle(request{Op: op, Path: p, Offset: 1, Limit: 1})
			if r["ok"] != false {
				t.Fatal(fmt.Sprintf("boundary accepted %s %s", op, p))
			}
		}
	}
}

// repeatedByte produces large observed input with constant fixture storage.
type repeatedByte struct{ left int64 }

func (r *repeatedByte) Read(p []byte) (int, error) {
	if r.left == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.left {
		n = int(r.left)
	}
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	r.left -= int64(n)
	return n, nil
}

func TestTextRangeFixedStorageHugeLines(t *testing.T) {
	s, _ := newTestServer(t)
	s.maxScan = 4 * 1024 * 1024
	s.maxExcerpt = 7
	for _, offset := range []int64{1, 2} {
		input := &repeatedByte{left: s.maxScan - 1}
		r := s.scanTextRange(request{Offset: offset, Limit: 1}, io.MultiReader(input, strings.NewReader("\n")))
		wantOK(t, r)
		if input.left != 0 || r["size"] != s.maxScan {
			t.Fatal("did not validate entire input")
		}
		if offset == 1 {
			if string(respData(t, r)) != "xxxxxxx" || r["partialLastLine"] != true {
				t.Fatal("excerpt cap")
			}
		} else if len(respData(t, r)) != 0 || r["truncated"] != false {
			t.Fatal("prefix was retained")
		}
	}
}

func TestTextRangeInjectedUTF8Caps(t *testing.T) {
	s, _ := newTestServer(t)
	for cap := 1; cap <= 8; cap++ {
		s.maxExcerpt = cap
		r := s.scanTextRange(request{Offset: 1, Limit: 1}, strings.NewReader("é😀z\r\n"))
		wantOK(t, r)
		want := ""
		for _, part := range []string{"é", "😀", "z", "\r", "\n"} {
			if len(want)+len(part) > cap {
				break
			}
			want += part
		}
		if !bytes.Equal(respData(t, r), []byte(want)) {
			t.Fatalf("cap=%d", cap)
		}
	}
}
