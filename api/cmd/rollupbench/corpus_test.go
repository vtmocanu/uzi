package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeNDJSON(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestNDJSONParsesDTOFieldsAndMergesInCreatedAtOrder(t *testing.T) {
	dir := t.TempDir()
	writeNDJSON(t, dir, "a", `{"seq":1,"kind":"text","agent":"lead","agent_instance":null,"agent_label":"m1","payload":{"text":"one"},"created_at":"2026-03-01T10:00:00.123456789Z"}
{"seq":2,"kind":"status","agent":null,"payload":{"event":"init"},"created_at":"2026-03-01T10:00:02.000001Z"}
`)
	writeNDJSON(t, dir, "b", `{"seq":1,"kind":"tool_use","payload":{"id":"x"},"created_at":"2026-03-01T10:00:01.5Z"}
`)
	src, err := newNDJSONSource(dir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Runs()) != 2 || src.Runs()[0].Count != 2 || src.Runs()[1].Count != 1 {
		t.Fatalf("runs %+v", src.Runs())
	}
	if src.Runs()[0].ID == src.Runs()[1].ID || !src.Runs()[0].Terminal {
		t.Fatal("ids must be fresh and runs terminal")
	}
	var got []Row
	if err := src.Stream(context.Background(), func(r *Row) error { got = append(got, *r); return nil }); err != nil {
		t.Fatal(err)
	}
	// created_at order across files: a/1, b/1, a/2.
	order := []struct{ run, seq int }{{0, 1}, {1, 1}, {0, 2}}
	for i, o := range order {
		if got[i].RunIdx != o.run || got[i].Rec.Seq != o.seq {
			t.Fatalf("row %d is run %d seq %d", i, got[i].RunIdx, got[i].Rec.Seq)
		}
	}
	first := got[0].Rec
	if first.Agent == nil || *first.Agent != "lead" || first.AgentInstance != nil || first.AgentLabel == nil || *first.AgentLabel != "m1" {
		t.Fatalf("nullable agent fields not mapped: %+v", first)
	}
	if first.ClaimGeneration != nil {
		t.Fatal("claim_generation must be NULL for an export")
	}
	// Nanosecond input keeps microsecond precision, the column's own precision.
	if first.CreatedAtMicro%1_000_000 != 123456 {
		t.Fatalf("created_at micro %d", first.CreatedAtMicro)
	}
	if got[2].Rec.Agent != nil {
		t.Fatal("null agent decoded as a value")
	}
}

func TestNDJSONMalformedAndDuplicateAbortWithoutEchoingContent(t *testing.T) {
	secret := "SENSITIVE-PAYLOAD-TEXT"
	for name, body := range map[string]string{
		"malformed": `{"seq":1,"kind":"text","payload":{"text":"ok"},"created_at":"2026-03-01T10:00:00Z"}` + "\n" + `{"seq":2,"kind":"text","payload":{"text":"` + secret + `"` + "\n",
		"duplicate": `{"seq":1,"kind":"text","payload":{"text":"` + secret + `"},"created_at":"2026-03-01T10:00:00Z"}` + "\n" + `{"seq":1,"kind":"text","payload":{"text":"x"},"created_at":"2026-03-01T10:00:01Z"}` + "\n",
		"badtime":   `{"seq":1,"kind":"text","payload":{"text":"` + secret + `"},"created_at":"yesterday"}` + "\n",
		"noseq":     `{"kind":"text","payload":{"text":"` + secret + `"},"created_at":"2026-03-01T10:00:00Z"}` + "\n",
	} {
		dir := t.TempDir()
		writeNDJSON(t, dir, "run-file-name-must-not-leak", body)
		_, err := newNDJSONSource(dir, 1000)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		msg := err.Error()
		if strings.Contains(msg, secret) || strings.Contains(msg, "run-file-name") || !strings.Contains(msg, "file #1") {
			t.Fatalf("%s: error %q leaks content or lacks the ordinal", name, msg)
		}
	}
	// The line number is reported.
	dir := t.TempDir()
	writeNDJSON(t, dir, "x", `{"seq":1,"kind":"text","payload":{},"created_at":"2026-03-01T10:00:00Z"}`+"\n"+"not json\n")
	if _, err := newNDJSONSource(dir, 1000); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("got %v", err)
	}
}

func TestSyntheticIsDeterministicAndHasOversizedMessage(t *testing.T) {
	p := synthParams{Seed: 7, Runs: 20, Large: 1, LargeMin: 300, TargetMiB: 2, Window: 4, TextRoot: "../../..", BigMiB: 1}
	collect := func() ([]Row, *synthSource) {
		s := newSynthSource(p)
		var rows []Row
		if err := s.Stream(context.Background(), func(r *Row) error { rows = append(rows, *r); return nil }); err != nil {
			t.Fatal(err)
		}
		return rows, s
	}
	a, sa := collect()
	b, _ := collect()
	if len(a) != len(b) || len(a) == 0 {
		t.Fatalf("lengths %d %d", len(a), len(b))
	}
	big := 0
	perRun := map[int]int{}
	var lastTS int64
	for i := range a {
		if string(a[i].Rec.Payload) != string(b[i].Rec.Payload) || a[i].RunIdx != b[i].RunIdx || a[i].Rec.CreatedAtMicro != b[i].Rec.CreatedAtMicro {
			t.Fatalf("row %d differs between identical seeds", i)
		}
		if len(a[i].Rec.Payload) > 512<<10 {
			big++
		}
		if a[i].Rec.CreatedAtMicro <= lastTS {
			t.Fatal("rows must be in strictly increasing created_at order")
		}
		lastTS = a[i].Rec.CreatedAtMicro
		perRun[a[i].RunIdx]++
		if a[i].Rec.Seq != perRun[a[i].RunIdx] {
			t.Fatalf("run %d seq %d out of order", a[i].RunIdx, a[i].Rec.Seq)
		}
	}
	if big == 0 {
		t.Fatal("no message larger than the largest chunk bound")
	}
	liveRuns, interleaved := 0, 0
	for i, r := range sa.Runs() {
		if perRun[i] != r.Count {
			t.Fatalf("run %d has %d rows, plan says %d", i, perRun[i], r.Count)
		}
		if !r.Terminal {
			liveRuns++
		}
	}
	for i := 1; i < len(a); i++ {
		if a[i].RunIdx != a[i-1].RunIdx {
			interleaved++
		}
	}
	if interleaved < len(a)/10 {
		t.Fatalf("heap is not interleaved: %d run switches in %d rows", interleaved, len(a))
	}
	if liveRuns == 0 {
		t.Fatal("expected some runs to stay live")
	}
}

func TestScrubRemovesIDsAndDSNs(t *testing.T) {
	in := "run 123e4567-e89b-12d3-a456-426614174000 via postgres://user:pw@host:5432/db?sslmode=disable failed" //nolint:gosec // G101: a fake DSN fixture to prove the scrubber removes it.
	out := scrub(in)
	if strings.Contains(out, "123e4567") || strings.Contains(out, "pw@") || strings.Contains(out, "postgres://") {
		t.Fatal(out)
	}
}

func TestSummarizePercentilesNearestRank(t *testing.T) {
	var d []time.Duration
	for i := 100; i >= 1; i-- {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	l := summarize(d)
	if l.N != 100 || l.P50Ms != 50 || l.P95Ms != 95 || l.P99Ms != 99 || l.MaxMs != 100 {
		t.Fatalf("%+v", l)
	}
	if got := summarize(nil); got.N != 0 {
		t.Fatal("empty input")
	}
}

func TestCritReaders(t *testing.T) {
	for in, want := range map[string]int{"1,8,32": 8, "1,4": 4, "16,32": 16, "8": 8} {
		var rs []int
		for _, f := range strings.Split(in, ",") {
			n, _ := strconv.Atoi(f)
			rs = append(rs, n)
		}
		if got := critReaders(rs); got != want {
			t.Fatalf("%s: got %d want %d", in, got, want)
		}
	}
}
