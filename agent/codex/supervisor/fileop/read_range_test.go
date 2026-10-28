package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRangeProtocol(t *testing.T) {
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "text"), []byte("one\ntwo\nthree"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := serve(s, strings.NewReader("{\"id\":7,\"op\":\"read_range\",\"path\":\"text\",\"offset\":2,\"limit\":1}\n"), &out); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wantOK(t, got)
	if string(respData(t, got)) != "two\n" || got["linesReturned"] != float64(1) || got["truncated"] != true || got["partialLastLine"] != false || got["incomplete"] != false {
		t.Fatalf("unexpected metadata: %v", got)
	}
	if _, exists := got["reason"]; exists {
		t.Fatal("complete response has reason")
	}
	// Exercise argv/root startup and NDJSON serialization through the production
	// entrypoint as well as the instrumentable server.
	out.Reset()
	var stderr bytes.Buffer
	input := "{\"id\":8,\"op\":\"read_range\",\"path\":\"text\",\"offset\":1,\"limit\":2}\n"
	if rc := run([]string{"--root", root}, strings.NewReader(input), &out, &stderr); rc != 0 {
		t.Fatalf("run=%d stderr=%q", rc, stderr.String())
	}
	got = nil
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	wantOK(t, got)
	if got["id"] != float64(8) || string(respData(t, got)) != "one\ntwo\n" || got["size"] != float64(13) || got["offset"] != float64(1) {
		t.Fatalf("protocol metadata %v", withoutData(got))
	}

}
