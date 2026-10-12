package main

import (
	"context"
	"slices"
	"testing"
)

// memStore is an in-memory archStore: rows 1..wm live in encoded chunks, wm+1..n are live rows.
type memStore struct {
	wm     int
	chunks map[int][]byte
	metaL  []chunkMeta
	live_  []Record
}

func newMemStore(t *testing.T, eng *Engine, all []Record, wm int, b ChunkBounds) *memStore {
	t.Helper()
	m := &memStore{wm: wm, chunks: map[int][]byte{}}
	for _, c := range SplitChunks(all[:wm], b) {
		data, err := eng.Encode(CodecSpec{ID: codecZstd, Level: 3}, c)
		if err != nil {
			t.Fatal(err)
		}
		m.chunks[c[0].Seq] = data
		m.metaL = append(m.metaL, chunkMeta{first: c[0].Seq, last: c[len(c)-1].Seq, n: len(c)})
	}
	m.live_ = all[wm:]
	return m
}

func (m *memStore) watermark(context.Context) (int, error) { return m.wm, nil }

func (m *memStore) metas(_ context.Context, sp ReadSpec, cursor int, haveCursor bool) ([]chunkMeta, error) {
	var out []chunkMeta
	for _, c := range m.metaL {
		if sp.After > 0 && c.last <= sp.After {
			continue
		}
		if sp.Before > 0 && c.first >= sp.Before {
			continue
		}
		if haveCursor && ((sp.Desc && c.first >= cursor) || (!sp.Desc && c.first <= cursor)) {
			continue
		}
		out = append(out, c)
	}
	if sp.Desc {
		slices.Reverse(out)
	}
	if len(out) > metaPage {
		out = out[:metaPage]
	}
	return out, nil
}

func (m *memStore) chunkData(_ context.Context, firsts []int) (map[int][]byte, error) {
	out := map[int][]byte{}
	for _, f := range firsts {
		out[f] = m.chunks[f]
	}
	return out, nil
}

func (m *memStore) live(_ context.Context, sp ReadSpec, low, want int) ([]Record, error) {
	ks := kindSet(sp.Kinds)
	var out []Record
	for _, r := range m.live_ {
		if r.Seq <= low || (sp.Before > 0 && r.Seq >= sp.Before) || (ks != nil && !ks[r.Kind]) {
			continue
		}
		out = append(out, r)
	}
	if sp.Desc {
		slices.Reverse(out)
	}
	if len(out) > want {
		out = out[:want]
	}
	return out, nil
}

// oracle answers a ReadSpec from the full row list the way the live SQL would.
func oracle(all []Record, sp ReadSpec) []Record {
	ks := kindSet(sp.Kinds)
	var out []Record
	for _, r := range all {
		if r.Seq <= sp.After || (sp.Before > 0 && r.Seq >= sp.Before) || (ks != nil && !ks[r.Kind]) {
			continue
		}
		out = append(out, r)
	}
	if sp.Desc {
		slices.Reverse(out)
	}
	if sp.Limit > 0 && len(out) > sp.Limit {
		out = out[:sp.Limit]
	}
	return out
}

func TestArchivedReadMatchesLiveSemanticsAcrossChunkBoundaries(t *testing.T) {
	eng := newTestEngine(t, 8<<20)
	const n = 400
	all := sampleRecords(n)
	// Make one kind rare so filter-then-limit matters: only every 37th row is 'plan'.
	for i := range all {
		if all[i].Kind == "tool_use" {
			all[i].Kind = "text"
		}
		if i%37 == 0 {
			all[i].Kind = "plan"
		}
	}
	bounds := ChunkBounds{ByteBound: 2000, CountCeiling: 13}
	ctx := context.Background()
	for _, wm := range []int{0, 1, 13, 200, 399, 400} { // none archived, tiny, boundary, mixed, nearly, all
		store := newMemStore(t, eng, all, wm, bounds)
		var specs []ReadSpec
		for _, after := range []int{0, 1, 12, 13, 14, 199, 200, 201, 399} {
			for _, limit := range []int{0, 1, 5, 14, 200} {
				for _, kinds := range [][]string{nil, {"plan"}, {"status", "plan"}} {
					specs = append(specs, ReadSpec{After: after, Limit: limit, Kinds: kinds})
				}
			}
		}
		for _, before := range []int{n + 1, 400, 201, 200, 14, 13, 2} {
			for _, limit := range []int{1, 5, 14, 200} {
				for _, kinds := range [][]string{nil, {"plan"}} {
					specs = append(specs, ReadSpec{Before: before, Limit: limit, Kinds: kinds, Desc: true})
				}
			}
		}
		for _, sp := range specs {
			got, st, err := readArchived(ctx, store, eng, sp)
			if err != nil {
				t.Fatal(err)
			}
			want := oracle(all, sp)
			if !sameResult(want, got, projFull) {
				t.Fatalf("wm=%d spec=%+v: got %d rows want %d", wm, sp, len(got), len(want))
			}
			if st.RowsOut != len(want) {
				t.Fatalf("stats rows %d != %d", st.RowsOut, len(want))
			}
			if st.ChunksMatched > st.ChunksFetched {
				t.Fatal("matched exceeds fetched")
			}
		}
	}
}

func TestArchivedFilteredReadSkipsChunksWithoutMatchingKinds(t *testing.T) {
	// The memStore does not apply the kinds filter to chunk metadata (the SQL does with kinds &&),
	// so this checks the lazy decode: a satisfied limit stops fetching further chunks.
	eng := newTestEngine(t, 8<<20)
	all := sampleRecords(300)
	store := newMemStore(t, eng, all, 300, ChunkBounds{ByteBound: 1 << 20, CountCeiling: 10})
	sp := ReadSpec{Limit: 5}
	got, st, err := readArchived(context.Background(), store, eng, sp)
	if err != nil {
		t.Fatal(err)
	}
	if !sameResult(oracle(all, sp), got, projFull) {
		t.Fatalf("got %d rows, want the first 5", len(got))
	}
	if st.ChunksFetched != 1 {
		t.Fatalf("a 5-row page decoded %d chunks, want exactly 1", st.ChunksFetched)
	}
}

func TestProjectJobsMirrorsPayloadTextOperator(t *testing.T) {
	cases := map[string]string{
		`{"text":"hello"}`: "hello", `{"other":1}`: "", `{"text":null}`: "", `{"text":42}`: "42", `not json`: "",
		`{"text":"a \"quoted\" b"}`: `a "quoted" b`,
	}
	for in, want := range cases {
		if got := jsonText([]byte(in)); got != want {
			t.Fatalf("%s: got %q want %q", in, got, want)
		}
	}
}
