package workersvc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// The shared fixtures under fixtures/memory-park are the exact bytes the worker's serializer writes;
// the agent suite reads the same files, so a drift on either side reddens both.
const memoryParkFixtureDir = "../../../fixtures/memory-park"

var (
	typicalRunID    = uuid.MustParse("00001001-0000-4000-8000-000000001001")
	typicalWorkerID = uuid.MustParse("00002002-0000-4000-8000-000000002002")
)

func readParkFixture(t *testing.T, name string) ([]byte, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(memoryParkFixtureDir, name+".json")) //nolint:gosec // G304: a fixed fixtures/memory-park file named by this test's own literals
	if err != nil {
		t.Fatal(err)
	}
	sum, err := os.ReadFile(filepath.Join(memoryParkFixtureDir, name+".json.sha256")) //nolint:gosec // G304: a fixed fixtures/memory-park file named by this test's own literals
	if err != nil {
		t.Fatal(err)
	}
	return raw, strings.TrimSpace(string(sum))
}

func typicalParkMap(t *testing.T) map[string]any {
	t.Helper()
	raw, _ := readParkFixture(t, "settlement-typical")
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func mapAt(m map[string]any, keys ...string) map[string]any {
	for _, k := range keys {
		m = m[k].(map[string]any)
	}
	return m
}

func sourcesOf(m map[string]any) []any { return mapAt(m, "settlement")["sources"].([]any) }

func marshalPark(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMemoryParkFixturesDecodeWithMatchingSHA(t *testing.T) {
	for _, name := range []string{"settlement-typical", "settlement-max-shape"} {
		raw, want := readParkFixture(t, name)
		sum := sha256.Sum256(raw)
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Fatalf("%s: sha256 %s, fixture records %s", name, got, want)
		}
		if len(raw) >= MaxMemoryParkSettlementBytes {
			t.Fatalf("%s is %d bytes, not under the %d cap", name, len(raw), MaxMemoryParkSettlementBytes)
		}
		rec, err := decodeMemoryParkRecord(raw)
		if err != nil {
			t.Fatalf("%s rejected: %v", name, err)
		}
		if rec.Settlement == nil {
			t.Fatalf("%s decoded without a settlement", name)
		}
	}
	raw, _ := readParkFixture(t, "settlement-max-shape")
	rec, _ := decodeMemoryParkRecord(raw)
	if n := len(rec.Settlement.Sources); n != 16 {
		t.Fatalf("max-shape sources = %d, want 16", n)
	}
	if n := len(rec.Settlement.Evidence.InterventionIDs); n != 64 {
		t.Fatalf("max-shape intervention ids = %d, want 64", n)
	}
	for _, s := range rec.Settlement.Sources {
		if utf16Len(s.Path) != 4096 {
			t.Fatalf("max-shape path is %d code units, want 4096", utf16Len(s.Path))
		}
	}
	// The receipt rules are stricter than the schema: 128-unit ids are not UUIDs, and the max-shape
	// binding names no real run.
	if _, _, err := decodeMemoryParkReceipt(raw, typicalRunID, typicalWorkerID); err == nil {
		t.Fatal("max-shape accepted as a receipt")
	}
}

func TestMemoryParkTypicalIsAValidReceipt(t *testing.T) {
	raw, _ := readParkFixture(t, "settlement-typical")
	rec, ids, err := decodeMemoryParkReceipt(raw, typicalRunID, typicalWorkerID)
	if err != nil || len(ids) != 2 || rec.Binding.ClaimGeneration != 3 {
		t.Fatalf("typical receipt: rec=%+v ids=%v err=%v", rec.Binding, ids, err)
	}
}

func TestMemoryParkDecoderRejectsEveryViolation(t *testing.T) {
	ev := func(m map[string]any) map[string]any { return mapAt(m, "settlement", "evidence") }
	src0 := func(m map[string]any) map[string]any { return sourcesOf(m)[0].(map[string]any) }
	src1 := func(m map[string]any) map[string]any { return sourcesOf(m)[1].(map[string]any) }
	long := func(n int) string { return strings.Repeat("x", n) }
	cases := map[string]func(m map[string]any){
		"extra top key":          func(m map[string]any) { m["extra"] = 1 },
		"missing ack":            func(m map[string]any) { delete(m, "ack") },
		"version 2":              func(m map[string]any) { m["version"] = 2 },
		"unknown ack":            func(m map[string]any) { m["ack"] = "maybe" },
		"case-different key":     func(m map[string]any) { m["Ack"] = m["ack"]; delete(m, "ack") },
		"settlement null":        func(m map[string]any) { m["settlement"] = nil },
		"settlement w/o confirm": func(m map[string]any) { m["ack"] = "sent_unconfirmed" },
		"binding extra":          func(m map[string]any) { mapAt(m, "binding")["x"] = 1 },
		"generation 0":           func(m map[string]any) { mapAt(m, "binding")["claimGeneration"] = 0 },
		"episode -1":             func(m map[string]any) { mapAt(m, "binding")["memoryEpisode"] = -1 },
		"generation fraction":    func(m map[string]any) { mapAt(m, "binding")["claimGeneration"] = 1.5 },
		"generation 2^53":        func(m map[string]any) { mapAt(m, "binding")["claimGeneration"] = float64(1 << 53) },
		"generation string":      func(m map[string]any) { mapAt(m, "binding")["claimGeneration"] = "3" },
		"empty nonce":            func(m map[string]any) { mapAt(m, "binding")["registerNonce"] = "" },
		"nonce 257 units":        func(m map[string]any) { mapAt(m, "binding")["registerNonce"] = long(257) },
		"run id 129 units":       func(m map[string]any) { mapAt(m, "binding")["runId"] = long(129) },
		"nonce with NUL":         func(m map[string]any) { mapAt(m, "binding")["registerNonce"] = "a\x00b" },
		"flight upper-case":      func(m map[string]any) { mapAt(m, "origin")["flightId"] = "0000A003-0000-4000-8000-000000003003" },
		"attempt malformed":      func(m map[string]any) { mapAt(m, "origin")["attemptId"] = "nope" },
		"incarnation malformed":  func(m map[string]any) { mapAt(m, "origin")["executorIncarnation"] = "has space" },
		"authority null":         func(m map[string]any) { mapAt(m, "origin")["authorityIncarnation"] = nil },
		"origin extra":           func(m map[string]any) { mapAt(m, "origin")["x"] = 1 },
		"settlement version 2":   func(m map[string]any) { mapAt(m, "settlement")["version"] = 2 },
		"confirmed string":       func(m map[string]any) { mapAt(m, "settlement")["confirmed"] = "true" },
		"no sources":             func(m map[string]any) { mapAt(m, "settlement")["sources"] = []any{} },
		"17 sources": func(m map[string]any) {
			var all []any
			for i := 0; i < 17; i++ {
				s := map[string]any{"path": "/p/" + string(rune('a'+i)), "authorityKey": strings.Repeat(string(rune('a'+i%6)), 63) + string(rune('0'+i%10)),
					"attemptId": nil, "epoch": src0(m)["epoch"], "seq": 1}
				all = append(all, s)
			}
			mapAt(m, "settlement")["sources"] = all
		},
		"relative path":     func(m map[string]any) { src0(m)["path"] = "data/x" },
		"dot-dot path":      func(m map[string]any) { src0(m)["path"] = "/data/../x" },
		"trailing slash":    func(m map[string]any) { src0(m)["path"] = "/data/x/" },
		"double slash":      func(m map[string]any) { src0(m)["path"] = "/data//x" },
		"path with NUL":     func(m map[string]any) { src0(m)["path"] = "/data/\x00x" },
		"path 4097 units":   func(m map[string]any) { src0(m)["path"] = "/" + long(4096) },
		"source extra key":  func(m map[string]any) { src0(m)["x"] = 1 },
		"authority upper":   func(m map[string]any) { src0(m)["authorityKey"] = strings.Repeat("A", 64) },
		"authority short":   func(m map[string]any) { src0(m)["authorityKey"] = "ab" },
		"duplicate authKey": func(m map[string]any) { src1(m)["authorityKey"] = src0(m)["authorityKey"] },
		"seq 0":             func(m map[string]any) { src0(m)["seq"] = 0 },
		"epoch not uuid":    func(m map[string]any) { src0(m)["epoch"] = "x" },
		"paths unordered": func(m map[string]any) {
			a, b := src0(m)["path"], src1(m)["path"]
			src0(m)["path"], src1(m)["path"] = b, a
		},
		"paths equal":       func(m map[string]any) { src1(m)["path"] = src0(m)["path"] },
		"parkAck differs":   func(m map[string]any) { mapAt(ev(m), "parkAck")["claimGeneration"] = 99 },
		"parkAck extra":     func(m map[string]any) { mapAt(ev(m), "parkAck")["x"] = 1 },
		"evidence extra":    func(m map[string]any) { ev(m)["x"] = 1 },
		"processJoin enum":  func(m map[string]any) { ev(m)["processJoin"] = "other" },
		"commands enum":     func(m map[string]any) { ev(m)["commands"] = "skipped" },
		"steering enum":     func(m map[string]any) { ev(m)["steering"] = "none" },
		"auxiliaries enum":  func(m map[string]any) { ev(m)["auxiliaries"] = "left" },
		"feedback enum":     func(m map[string]any) { ev(m)["feedback"] = "pending" },
		"sinkReceipts enum": func(m map[string]any) { ev(m)["sinkReceipts"] = "none" },
		"parkReap enum":     func(m map[string]any) { ev(m)["parkReap"] = "partial" },
		"65 ids": func(m map[string]any) {
			ids := make([]any, 65)
			for i := range ids {
				ids[i] = uuid.New().String()
			}
			ev(m)["interventionIds"] = ids
		},
		"duplicate ids": func(m map[string]any) { ev(m)["interventionIds"] = []any{"a", "a"} },
		"id 129 units":  func(m map[string]any) { ev(m)["interventionIds"] = []any{long(129)} },
		"id not string": func(m map[string]any) { ev(m)["interventionIds"] = []any{1} },
		"ids null":      func(m map[string]any) { ev(m)["interventionIds"] = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := typicalParkMap(t)
			mutate(m)
			if _, err := decodeMemoryParkRecord(marshalPark(t, m)); err == nil {
				t.Fatalf("accepted: %s", marshalPark(t, m))
			}
		})
	}
}

func TestMemoryParkDecoderRejectsMalformedBytes(t *testing.T) {
	typical, _ := readParkFixture(t, "settlement-typical")
	s := string(typical)
	pathText := "/data/runner/org+repo/issue-3639"
	idText := "00007007-0000-4000-8000-000000007007"
	cases := map[string]string{
		"empty":                "",
		"trailing value":       s + s,
		"trailing garbage":     s + "x",
		"duplicate key":        strings.Replace(s, `"ack":"confirmed"`, `"ack":"confirmed","ack":"confirmed"`, 1),
		"duplicate nested key": strings.Replace(s, `"seq":7`, `"seq":7,"seq":7`, 1),
		// The surrogate cases target an intervention id, a field with no cross-check in the strict decode,
		// so only the explicit surrogate rejection in str() can fail them.
		"lone high surrogate": strings.Replace(s, idText+`"`, idText+`\ud800"`, 1),
		"lone low surrogate":  strings.Replace(s, idText+`"`, idText+`\udc00"`, 1),
		"high then non-low":   strings.Replace(s, idText+`"`, idText+`\ud800A"`, 1),
		"low then high":       strings.Replace(s, idText+`"`, idText+`\udc00\ud800"`, 1),
		"high at string end":  strings.Replace(s, idText+`"`, idText+`\uD83D"`, 1),
		"escaped NUL":         strings.Replace(s, pathText+`"`, pathText+`\u0000"`, 1),
		"invalid utf-8":       strings.Replace(s, pathText+`"`, pathText+"\xff\"", 1),
		"bad escape":          strings.Replace(s, pathText+`"`, pathText+`\x41"`, 1),
		"raw control char":    strings.Replace(s, pathText+`"`, pathText+"\n\"", 1),
		"exponent integer":    strings.Replace(s, `"seq":7`, `"seq":7e0`, 1),
		"fraction integer":    strings.Replace(s, `"seq":7`, `"seq":7.0`, 1),
		"negative zero":       strings.Replace(s, `"memoryEpisode":1`, `"memoryEpisode":-0`, 1),
		"leading zero":        strings.Replace(s, `"seq":7`, `"seq":07`, 1),
		"truncated":           s[:len(s)/2],
		"array root":          "[" + s + "]",
		"too deep":            strings.Repeat("[", 40) + strings.Repeat("]", 40),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if in == s {
				t.Fatal("mutation did not apply")
			}
			if _, err := decodeMemoryParkRecord([]byte(in)); err == nil {
				t.Fatalf("accepted %q", in)
			}
		})
	}
}

// The control for the surrogate cases above: the same field with a well-formed escape decodes, so the
// surrogate is the only reason those cases fail.
func TestMemoryParkDecoderAcceptsEscapeInInterventionID(t *testing.T) {
	typical, _ := readParkFixture(t, "settlement-typical")
	s := string(typical)
	idText := "00007007-0000-4000-8000-000000007007"
	in := strings.Replace(s, idText+`"`, idText+`\u0041"`, 1)
	if in == s {
		t.Fatal("mutation did not apply")
	}
	if _, err := decodeMemoryParkRecord([]byte(in)); err != nil {
		t.Fatalf("control rejected: %v", err)
	}
}

// Parse amplification: a body of many tiny values must be refused by the value cap before the generic
// tree is built, not after. The 512 KiB body below allocates tens of MiB without the cap.
func TestMemoryParkDecoderValueCapBoundsAllocation(t *testing.T) {
	flat := func(n int) []byte {
		return []byte(`{"a":[` + strings.TrimSuffix(strings.Repeat("{},", n), ",") + `]}`)
	}
	amplifiers := map[string][]byte{
		"empty objects to the byte cap": flat((MaxMemoryParkSettlementBytes - 8) / 3),
		"just over the value cap":       flat(memoryParkMaxValues),
		"empty arrays":                  []byte(`{"a":[` + strings.TrimSuffix(strings.Repeat("[],", 100000), ",") + `]}`),
	}
	for name, body := range amplifiers {
		if len(body) > MaxMemoryParkSettlementBytes {
			t.Fatalf("%s: body %d exceeds the byte cap, so it would not test the value cap", name, len(body))
		}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := decodeMemoryParkRecord(body)
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Fatalf("%s accepted", name)
		}
		if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
			t.Fatalf("%s allocated %d bytes before rejection", name, grew)
		}
	}
	// The cap trips on the value count itself: the walk of exactly the cap succeeds structurally.
	p := &pjParser{b: flat(memoryParkMaxValues - 2)} // object + array + n elements = cap
	if _, err := p.value(0); err != nil {
		t.Fatalf("walk of exactly %d values failed: %v", memoryParkMaxValues, err)
	}
	p = &pjParser{b: flat(memoryParkMaxValues - 1)}
	if _, err := p.value(0); err == nil {
		t.Fatalf("walk of %d values was not refused", memoryParkMaxValues+1)
	}
}

func TestMemoryParkDecoderAcceptsPairedSurrogateEscapes(t *testing.T) {
	typical, _ := readParkFixture(t, "settlement-typical")
	s := string(typical)
	attemptPath := `.attempt-20261031T130139Z-g1-18690f69af1bc841"`
	in := strings.Replace(s, attemptPath, `.attempt-20261031T130139Z-g1-18690f69af1bc841😀"`, 1)
	if in == s {
		t.Fatal("mutation did not apply")
	}
	rec, err := decodeMemoryParkRecord([]byte(in))
	if err != nil {
		t.Fatalf("a valid surrogate pair was rejected: %v", err)
	}
	if got := rec.Settlement.Sources[1].Path; !strings.HasSuffix(got, "\U0001F600") {
		t.Fatalf("pair decoded as %q", got)
	}
}

// U+FF01 sorts before U+1F600 in UTF-8 byte order, but after it in UTF-16 code-unit order (the high
// surrogate 0xD83D is below 0xFF01): the worker sorts by code unit, so the server must too.
func TestMemoryParkPathOrderIsUTF16CodeUnitOrder(t *testing.T) {
	build := func(first, second string) []byte {
		m := typicalParkMap(t)
		srcs := sourcesOf(m)
		srcs[0].(map[string]any)["path"] = first
		srcs[1].(map[string]any)["path"] = second
		return marshalPark(t, m)
	}
	emoji, fullwidth := "/\U0001F600x", "/！x"
	if strings.Compare(fullwidth, emoji) >= 0 {
		t.Fatal("test premise: byte order should put U+FF01 first")
	}
	if _, err := decodeMemoryParkRecord(build(emoji, fullwidth)); err != nil {
		t.Fatalf("code-unit ascending order rejected: %v", err)
	}
	if _, err := decodeMemoryParkRecord(build(fullwidth, emoji)); err == nil {
		t.Fatal("byte-order-ascending but code-unit-descending order was accepted")
	}
}

func TestMemoryParkReceiptRules(t *testing.T) {
	cases := map[string]struct {
		mutate func(m map[string]any)
		run    uuid.UUID
		worker uuid.UUID
	}{
		"other run":        {func(m map[string]any) {}, uuid.New(), typicalWorkerID},
		"other worker":     {func(m map[string]any) {}, typicalRunID, uuid.New()},
		"ack not confirm":  {func(m map[string]any) { m["ack"] = "sent_unconfirmed"; delete(m, "settlement") }, typicalRunID, typicalWorkerID},
		"no settlement":    {func(m map[string]any) { delete(m, "settlement") }, typicalRunID, typicalWorkerID},
		"confirmed false":  {func(m map[string]any) { mapAt(m, "settlement")["confirmed"] = false }, typicalRunID, typicalWorkerID},
		"epoch mismatch":   {func(m map[string]any) { mapAt(m, "origin")["sourceEpoch"] = "00000000-0000-4000-8000-00000000ffff" }, typicalRunID, typicalWorkerID},
		"no origin source": {func(m map[string]any) { mapAt(m, "origin")["attemptId"] = "20261031T130139Z-g2-0123456789abcdef" }, typicalRunID, typicalWorkerID},
		"null origin w/ none": {func(m map[string]any) {
			mapAt(m, "origin")["attemptId"] = nil
			sourcesOf(m)[0].(map[string]any)["attemptId"] = "20261031T130139Z-g3-0123456789abcdef"
		}, typicalRunID, typicalWorkerID},
		"two matching sources": {func(m map[string]any) {
			mapAt(m, "origin")["attemptId"] = nil
			sourcesOf(m)[1].(map[string]any)["attemptId"] = nil
		}, typicalRunID, typicalWorkerID},
		"non-uuid intervention": {func(m map[string]any) { mapAt(m, "settlement", "evidence")["interventionIds"] = []any{"not-a-uuid"} }, typicalRunID, typicalWorkerID},
		"nonce over 128": {func(m map[string]any) {
			n := strings.Repeat("n", 129)
			mapAt(m, "binding")["registerNonce"] = n
			mapAt(m, "settlement", "evidence", "parkAck")["registerNonce"] = n
		}, typicalRunID, typicalWorkerID},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			m := typicalParkMap(t)
			c.mutate(m)
			if _, _, err := decodeMemoryParkReceipt(marshalPark(t, m), c.run, c.worker); err == nil {
				t.Fatal("receipt accepted")
			}
		})
	}
}
