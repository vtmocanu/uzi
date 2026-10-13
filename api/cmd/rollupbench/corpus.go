package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunMeta describes one run of the corpus. Run ids are fresh uuids for every corpus label, so a
// results file never carries an id that exists anywhere else.
type RunMeta struct {
	Idx      int
	ID       uuid.UUID
	Status   string
	Kind     string // issue | chat (chat is assigned so ListChatRunsForUser has runs to list)
	Terminal bool
	Count    int
}

// Row is one message in load order.
type Row struct {
	RunIdx int
	Rec    Record
}

// Source produces a corpus deterministically (synthetic) or from an existing export.
type Source interface {
	Label() string
	Note() string
	Runs() []RunMeta
	// Stream emits every row in created_at order across runs. It may be called more than once
	// and must emit the same rows each time.
	Stream(ctx context.Context, emit func(*Row) error) error
}

var terminalStatuses = []string{"completed", "failed", "cancelled"}

const maxChatRuns = 50

// newRNG centralises the seeded PRNG: the corpus must be reproducible from --seed, and nothing
// here is security-sensitive.
func newRNG(seed uint64, stream uint64) *rand.Rand {
	return rand.New(rand.NewPCG(seed, stream)) //nolint:gosec // G404: deterministic corpus generation, not a security context.
}

// assignChat marks up to maxChatRuns terminal runs that are not large as chat runs, spread
// evenly, so the chat-list shape has one user with that many conversations.
func assignChat(runs []RunMeta, largeMin int) {
	var cand []int
	for i := range runs {
		if runs[i].Terminal && runs[i].Count < largeMin && runs[i].Count > 0 {
			cand = append(cand, i)
		}
	}
	n := min(maxChatRuns, len(cand)/3)
	for k := 0; k < n; k++ {
		runs[cand[k*len(cand)/n]].Kind = "chat"
	}
}

// ---- synthetic -------------------------------------------------------------------------------

// textPool is the body text for tool results: this repository's own tracked text files, read once.
type textPool struct {
	files  []string
	digest string
}

func loadTextPool(root string) *textPool {
	out, err := exec.Command("git", "-C", root, "ls-files").Output() //nolint:gosec // G204: fixed argv, root is the operator's checkout.
	var names []string
	if err == nil {
		for _, n := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			switch strings.ToLower(filepath.Ext(n)) {
			case ".go", ".ts", ".md", ".sql", ".yml", ".yaml", ".sh", ".json", ".tsx", ".css":
				names = append(names, n)
			}
		}
	}
	sort.Strings(names)
	p := &textPool{}
	h := sha256.New()
	total := 0
	for _, n := range names {
		b, rerr := os.ReadFile(filepath.Join(root, n)) //nolint:gosec // G304: path comes from git ls-files of the operator's checkout.
		if rerr != nil || len(b) < 200 || len(b) > 256<<10 {
			continue
		}
		p.files = append(p.files, strings.ToValidUTF8(string(b), "?"))
		h.Write(b)
		total += len(b)
		if total > 24<<20 {
			break
		}
	}
	if len(p.files) == 0 {
		// No checkout to read (for example a test run outside git): a deterministic filler.
		var sb strings.Builder
		for i := 0; sb.Len() < 128<<10; i++ {
			fmt.Fprintf(&sb, "func handler%d(ctx context.Context) error { return store.Do(ctx, %d) } // line %d\n", i, i*7919, i)
		}
		p.files = []string{sb.String()}
		h.Write([]byte(p.files[0]))
	}
	p.digest = hex.EncodeToString(h.Sum(nil))[:16]
	return p
}

// slice returns about n bytes of text starting at a random offset of a random file.
func (p *textPool) slice(rng *rand.Rand, n int) string {
	f := p.files[rng.IntN(len(p.files))]
	if n >= len(f) {
		n = len(f)
	}
	start := 0
	if len(f) > n {
		start = rng.IntN(len(f) - n)
	}
	return strings.ToValidUTF8(f[start:start+n], "?")
}

// big builds a text of at least n bytes by concatenating slices.
func (p *textPool) big(rng *rand.Rand, n int) string {
	var sb strings.Builder
	for sb.Len() < n {
		sb.WriteString(p.slice(rng, 64<<10))
	}
	return sb.String()
}

// stream turns a non-negative index into a PRNG stream number.
func stream(i int) uint64 {
	if i < 0 {
		return 0
	}
	return uint64(i)
}

func lognormal(rng *rand.Rand, median float64, sigma float64, lo, hi int) int {
	v := int(median * math.Exp(rng.NormFloat64()*sigma))
	return max(lo, min(hi, v))
}

type runGen struct {
	rng     *rand.Rand
	seq     int
	pending string // tool_use id awaiting its tool_result
	bigAt   map[int]int
	n       int
}

var agentNames = []string{"lead", "coder", "reviewer", "tester"}

func (g *runGen) toolID() string {
	return fmt.Sprintf("toolu_%016x", g.rng.Uint64())
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (g *runGen) usage() string {
	in, out := 200+g.rng.IntN(60000), 50+g.rng.IntN(8000)
	return fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}`,
		in, out, g.rng.IntN(150000), g.rng.IntN(20000))
}

// next produces the next message of the run: kind, agent columns and the JSON payload.
func (g *runGen) next(p *textPool) (string, []byte) {
	g.seq++
	if size, ok := g.bigAt[g.seq]; ok {
		g.pending = ""
		return "tool_result", []byte(fmt.Sprintf(`{"tool_use_id":%s,"content":%s,"is_error":false}`, jsonString(g.toolID()), jsonString(p.big(g.rng, size))))
	}
	if g.seq == 1 {
		return "status", []byte(fmt.Sprintf(`{"event":"init","session_id":"sess-%08x","model":"claude-sonnet-5-5","cwd":"/work/repo"}`, g.rng.Uint32()))
	}
	if g.pending != "" && g.rng.IntN(100) < 88 {
		id := g.pending
		g.pending = ""
		var body string
		if g.rng.IntN(100) < 30 {
			body = "ok"
		} else {
			body = p.slice(g.rng, lognormal(g.rng, 3500, 1.3, 40, 120<<10))
		}
		return "tool_result", []byte(fmt.Sprintf(`{"tool_use_id":%s,"content":%s,"is_error":%t}`, jsonString(id), jsonString(body), g.rng.IntN(100) < 4))
	}
	switch r := g.rng.IntN(100); {
	case r < 36:
		id := g.toolID()
		g.pending = id
		tools := []string{"Bash", "Read", "Edit", "Grep", "Write"}
		name := tools[g.rng.IntN(len(tools))]
		var in string
		switch name {
		case "Bash":
			in = fmt.Sprintf(`{"command":%s,"description":"run a command"}`, jsonString("cd api && go test ./internal/store/... -run "+p.slice(g.rng, 20+g.rng.IntN(60))))
		case "Edit":
			in = fmt.Sprintf(`{"file_path":"api/internal/x/y.go","old_string":%s,"new_string":%s}`, jsonString(p.slice(g.rng, lognormal(g.rng, 300, 0.8, 20, 4000))), jsonString(p.slice(g.rng, lognormal(g.rng, 350, 0.8, 20, 4000))))
		default:
			in = fmt.Sprintf(`{"file_path":%s}`, jsonString("api/internal/"+p.slice(g.rng, 30)))
		}
		return "tool_use", []byte(fmt.Sprintf(`{"id":%s,"name":%s,"input":%s}`, jsonString(id), jsonString(name), in))
	case r < 80:
		return "text", []byte(fmt.Sprintf(`{"text":%s}`, jsonString(p.slice(g.rng, lognormal(g.rng, 400, 1.0, 20, 8000)))))
	case r < 86:
		if g.rng.IntN(3) == 0 {
			return "status", []byte(fmt.Sprintf(`{"event":"result","subtype":"success","is_error":false,"duration_ms":%d,"total_cost_usd":%.4f,"usage":%s,"modelUsage":{"claude-sonnet-5-5":%s}}`,
				g.rng.IntN(600000), g.rng.Float64()*3, g.usage(), g.usage()))
		}
		return "status", []byte(fmt.Sprintf(`{"event":"init","session_id":"sess-%08x","model":"claude-sonnet-5-5"}`, g.rng.Uint32()))
	case r < 90:
		return "progress_note", []byte(fmt.Sprintf(`{"note":%s,"usage":%s}`, jsonString(p.slice(g.rng, 120)), g.usage()))
	case r < 92:
		return "plan", []byte(fmt.Sprintf(`{"plan_md":%s}`, jsonString(p.slice(g.rng, lognormal(g.rng, 2500, 0.6, 200, 20000)))))
	case r < 94:
		return "summary_usage", []byte(fmt.Sprintf(`{"kind":"intent","usage":%s}`, g.usage()))
	case r < 97:
		return "error", []byte(fmt.Sprintf(`{"text":%s,"code":"turn_failed","usage":%s}`, jsonString(p.slice(g.rng, 200)), g.usage()))
	default:
		return "thinking", []byte(fmt.Sprintf(`{"text":%s}`, jsonString(p.slice(g.rng, lognormal(g.rng, 900, 0.9, 40, 12000)))))
	}
}

type synthSource struct {
	seed     uint64
	pool     *textPool
	runs     []RunMeta
	order    []int // run indexes in start order
	bigAt    map[int]map[int]int
	window   int
	largeMin int
}

type synthParams struct {
	Seed      uint64
	Runs      int
	Large     int
	LargeMin  int
	TargetMiB int
	Window    int
	TextRoot  string `json:"-"` // a filesystem path: never serialised into results
	BigMiB    int
}

func newSynthSource(p synthParams) *synthSource {
	pool := loadTextPool(p.TextRoot)
	// Estimate the mean payload so the message budget matches the byte target.
	g := &runGen{rng: newRNG(p.Seed, 9999)}
	sample, sampleBytes := 4000, 0
	for i := 0; i < sample; i++ {
		_, pl := g.next(pool)
		sampleBytes += len(pl)
	}
	mean := float64(sampleBytes) / float64(sample)
	totalMsgs := max(int(float64(p.TargetMiB)*(1<<20)/mean), p.Large*p.LargeMin+p.Runs*5)

	rng := newRNG(p.Seed, 1)
	counts := make([]int, p.Runs)
	remaining := totalMsgs
	for i := 0; i < p.Large; i++ {
		counts[i] = p.LargeMin + rng.IntN(p.LargeMin/2+1)
		remaining -= counts[i]
	}
	rest := p.Runs - p.Large
	weights := make([]float64, rest)
	var wsum float64
	for i := range weights {
		weights[i] = math.Exp(rng.NormFloat64() * 1.5)
		wsum += weights[i]
	}
	for i := range weights {
		c := max(5, int(float64(max(remaining, 0))*weights[i]/wsum))
		counts[p.Large+i] = min(c, p.LargeMin-1)
	}
	// Shuffle so large runs start spread across the load, not first.
	idx := rng.Perm(p.Runs)
	s := &synthSource{seed: p.Seed, pool: pool, window: p.Window, largeMin: p.LargeMin, bigAt: map[int]map[int]int{}}
	s.runs = make([]RunMeta, p.Runs)
	s.order = idx
	for i := 0; i < p.Runs; i++ {
		s.runs[i] = RunMeta{Idx: i, Count: counts[i], Kind: "issue"}
		var idb [16]byte
		binary.LittleEndian.PutUint64(idb[:8], rng.Uint64())
		binary.LittleEndian.PutUint64(idb[8:], rng.Uint64())
		s.runs[i].ID = uuid.UUID(idb)
	}
	// ~20% of the non-large runs stay live; large runs are terminal except one when there are many.
	live := 0
	for i := range s.runs {
		isLarge := i < p.Large
		switch {
		case isLarge && (p.Large < 4 || i != p.Large-1):
			s.runs[i].Terminal = true
		case !isLarge && rng.IntN(100) < 20:
			s.runs[i].Terminal = false
		case isLarge:
			s.runs[i].Terminal = false
		default:
			s.runs[i].Terminal = true
		}
		if s.runs[i].Terminal {
			s.runs[i].Status = terminalStatuses[[]int{0, 0, 0, 0, 0, 0, 0, 1, 1, 2}[rng.IntN(10)]]
		} else {
			s.runs[i].Status = []string{"running", "awaiting_approval", "limit_wait"}[rng.IntN(3)]
			live++
		}
	}
	if live == 0 && p.Runs > p.Large {
		// A corpus with no live run could not show the live-reader comparison; keep at least one.
		last := &s.runs[p.Runs-1]
		last.Terminal, last.Status = false, "running"
	}
	// One oversized message (bigger than the largest chunk bound) in the first terminal mid-size
	// run, and a second in another; each at ~40% of the run.
	placed := 0
	for i := p.Large; i < p.Runs && placed < 2; i++ {
		if s.runs[i].Terminal && s.runs[i].Count >= 20 {
			s.bigAt[i] = map[int]int{max(2, s.runs[i].Count*2/5): p.BigMiB << 20 / (1 + placed)}
			placed++
		}
	}
	if placed == 0 { // degenerate tiny corpora: put it in run 0
		s.bigAt[0] = map[int]int{max(2, s.runs[0].Count/2): p.BigMiB << 20}
	}
	assignChat(s.runs, p.LargeMin)
	return s
}

func (s *synthSource) Label() string { return "synthetic" }
func (s *synthSource) Note() string {
	return fmt.Sprintf("seeded synthetic corpus; tool_result bodies are slices of this repo's tracked text files (pool digest %s)", s.pool.digest)
}
func (s *synthSource) Runs() []RunMeta { return s.runs }

func (s *synthSource) Stream(ctx context.Context, emit func(*Row) error) error {
	pick := newRNG(s.seed, 2)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixMicro()
	type live struct {
		idx  int
		gen  *runGen
		left int
	}
	queue := append([]int(nil), s.order...)
	var active []live
	start := func() {
		for len(active) < s.window && len(queue) > 0 {
			i := queue[0]
			queue = queue[1:]
			active = append(active, live{idx: i, gen: &runGen{rng: newRNG(s.seed, stream(100+i)), bigAt: s.bigAt[i], n: s.runs[i].Count}, left: s.runs[i].Count})
		}
	}
	start()
	for len(active) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		k := pick.IntN(len(active))
		a := &active[k]
		kind, payload := a.gen.next(s.pool)
		clock += int64(20000 + pick.IntN(400000))
		r := Row{RunIdx: a.idx, Rec: Record{Seq: a.gen.seq, Kind: kind, Payload: payload, CreatedAtMicro: clock}}
		g := a.gen.rng
		if g.IntN(100) < 80 {
			ag := agentNames[g.IntN(len(agentNames))]
			r.Rec.Agent = &ag
		}
		if g.IntN(100) < 30 {
			in := fmt.Sprintf("%s#%d", agentNames[g.IntN(len(agentNames))], 1+g.IntN(3))
			r.Rec.AgentInstance = &in
		}
		if g.IntN(100) < 10 {
			lb := fmt.Sprintf("m%d", 1+g.IntN(5))
			r.Rec.AgentLabel = &lb
		}
		if g.IntN(100) < 70 {
			cg := int64(1 + g.IntN(3))
			r.Rec.ClaimGeneration = &cg
		}
		if err := emit(&r); err != nil {
			return err
		}
		a.left--
		if a.left == 0 {
			active = append(active[:k], active[k+1:]...)
			start()
		}
	}
	return nil
}

// ---- ndjson ----------------------------------------------------------------------------------

type ndjsonDTO struct {
	Seq           *int            `json:"seq"`
	Kind          string          `json:"kind"`
	Agent         *string         `json:"agent"`
	AgentInstance *string         `json:"agent_instance"`
	AgentLabel    *string         `json:"agent_label"`
	Payload       json.RawMessage `json:"payload"`
	CreatedAt     string          `json:"created_at"`
}

type ndjsonSource struct {
	runs []RunMeta
	rows []Row
}

// newNDJSONSource reads one file per run (the `uzi run logs <run> --json` line format). File
// names are never recorded: each file becomes a run with a fresh uuid. An error names only the
// file ordinal and line number, never content, because the payloads are real.
func newNDJSONSource(dir string, largeMin int) (*ndjsonSource, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.New("ndjson: cannot read the corpus directory")
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, errors.New("ndjson: the corpus directory has no files")
	}
	s := &ndjsonSource{}
	for fi, name := range names {
		n, err := s.readFile(filepath.Join(dir, name), fi+1)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		s.runs[len(s.runs)-1].Count = n
	}
	sort.SliceStable(s.rows, func(i, j int) bool {
		if s.rows[i].Rec.CreatedAtMicro != s.rows[j].Rec.CreatedAtMicro {
			return s.rows[i].Rec.CreatedAtMicro < s.rows[j].Rec.CreatedAtMicro
		}
		if s.rows[i].RunIdx != s.rows[j].RunIdx {
			return s.rows[i].RunIdx < s.rows[j].RunIdx
		}
		return s.rows[i].Rec.Seq < s.rows[j].Rec.Seq
	})
	assignChat(s.runs, largeMin)
	return s, nil
}

func (s *ndjsonSource) readFile(path string, ordinal int) (int, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator's own --corpus-ndjson directory.
	if err != nil {
		return 0, fmt.Errorf("ndjson file #%d: cannot open", ordinal)
	}
	defer func() { _ = f.Close() }()
	idx := len(s.runs)
	s.runs = append(s.runs, RunMeta{Idx: idx, ID: uuid.New(), Status: "completed", Kind: "issue", Terminal: true})
	br := bufio.NewReaderSize(f, 1<<20)
	seen := map[int]bool{}
	line, count := 0, 0
	for {
		b, rerr := br.ReadBytes('\n')
		if len(bytes.TrimSpace(b)) > 0 {
			line++
			var d ndjsonDTO
			if json.Unmarshal(b, &d) != nil || d.Seq == nil || d.Kind == "" || len(d.Payload) == 0 || string(d.Payload) == "null" {
				return 0, fmt.Errorf("ndjson file #%d line %d: malformed line", ordinal, line)
			}
			ts, perr := time.Parse(time.RFC3339Nano, d.CreatedAt)
			if perr != nil {
				return 0, fmt.Errorf("ndjson file #%d line %d: malformed created_at", ordinal, line)
			}
			if seen[*d.Seq] {
				return 0, fmt.Errorf("ndjson file #%d line %d: duplicate seq", ordinal, line)
			}
			seen[*d.Seq] = true
			s.rows = append(s.rows, Row{RunIdx: idx, Rec: Record{
				Seq: *d.Seq, Kind: d.Kind, Agent: d.Agent, AgentInstance: d.AgentInstance, AgentLabel: d.AgentLabel,
				Payload: append([]byte(nil), d.Payload...), CreatedAtMicro: ts.UnixMicro(),
			}})
			count++
		} else if rerr == nil {
			line++
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return 0, fmt.Errorf("ndjson file #%d: read error", ordinal)
		}
	}
	if count == 0 {
		s.runs = s.runs[:idx]
	}
	return count, nil
}

func (s *ndjsonSource) Label() string { return "ndjson" }
func (s *ndjsonSource) Note() string {
	return "uzi run logs --json exports; ids and claim_generation are not in that format (ids = load order, claim_generation NULL); every run loaded as terminal"
}
func (s *ndjsonSource) Runs() []RunMeta { return s.runs }
func (s *ndjsonSource) Stream(ctx context.Context, emit func(*Row) error) error {
	for i := range s.rows {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if err := emit(&s.rows[i]); err != nil {
			return err
		}
	}
	return nil
}

// ---- snapshot --------------------------------------------------------------------------------

type snapshotSource struct {
	dsn  string
	runs []RunMeta
	ids  map[uuid.UUID]int
}

func newSnapshotSource(ctx context.Context, dsn string, largeMin int) (*snapshotSource, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, errors.New("snapshot: cannot connect to --source-dsn")
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, errors.New("snapshot: cannot open a read-only transaction")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `SELECT m.run_id, r.status, count(*) FROM run_messages m JOIN runs r ON r.id = m.run_id
		WHERE r.status IN ('completed','failed','cancelled') GROUP BY m.run_id, r.status, r.created_at ORDER BY r.created_at, m.run_id`)
	if err != nil {
		return nil, errors.New("snapshot: cannot list terminal runs")
	}
	s := &snapshotSource{dsn: dsn, ids: map[uuid.UUID]int{}}
	for rows.Next() {
		var src uuid.UUID
		var status string
		var n int
		if err := rows.Scan(&src, &status, &n); err != nil {
			return nil, errors.New("snapshot: cannot read run list")
		}
		i := len(s.runs)
		s.ids[src] = i
		s.runs = append(s.runs, RunMeta{Idx: i, ID: uuid.New(), Status: status, Kind: "issue", Terminal: true, Count: n})
	}
	if rows.Err() != nil || len(s.runs) == 0 {
		return nil, errors.New("snapshot: no terminal runs with messages")
	}
	assignChat(s.runs, largeMin)
	return s, nil
}

func (s *snapshotSource) Label() string { return "snapshot" }
func (s *snapshotSource) Note() string {
	return "terminal runs copied from a restored snapshot in a READ ONLY transaction, run ids remapped"
}
func (s *snapshotSource) Runs() []RunMeta { return s.runs }
func (s *snapshotSource) Stream(ctx context.Context, emit func(*Row) error) error {
	conn, err := pgx.Connect(ctx, s.dsn)
	if err != nil {
		return errors.New("snapshot: cannot connect to --source-dsn")
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return errors.New("snapshot: cannot open a read-only transaction")
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ids := make([]uuid.UUID, 0, len(s.ids))
	for id := range s.ids {
		ids = append(ids, id)
	}
	rows, err := tx.Query(ctx, `SELECT run_id, seq, kind, agent, payload::text, created_at, agent_instance, agent_label, claim_generation
		FROM run_messages WHERE run_id = ANY($1) ORDER BY created_at, id`, ids)
	if err != nil {
		return errors.New("snapshot: cannot read messages")
	}
	defer rows.Close()
	for rows.Next() {
		var src uuid.UUID
		var r Row
		var payload string
		var ts time.Time
		if err := rows.Scan(&src, &r.Rec.Seq, &r.Rec.Kind, &r.Rec.Agent, &payload, &ts, &r.Rec.AgentInstance, &r.Rec.AgentLabel, &r.Rec.ClaimGeneration); err != nil {
			return errors.New("snapshot: cannot decode a message row")
		}
		r.RunIdx = s.ids[src]
		r.Rec.Payload = []byte(payload)
		r.Rec.CreatedAtMicro = ts.UnixMicro()
		if err := emit(&r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ---- loading ---------------------------------------------------------------------------------

type rowSource struct {
	ch   chan *Row
	cur  *Row
	done chan error
}

func (r *rowSource) Next() bool {
	row, ok := <-r.ch
	r.cur = row
	return ok
}

func (r *rowSource) values() []any {
	rec := &r.cur.Rec
	return []any{r.cur.RunIdx, rec.Seq, rec.Kind, rec.Agent, rec.Payload, time.UnixMicro(rec.CreatedAtMicro).UTC(),
		rec.AgentInstance, rec.AgentLabel, rec.ClaimGeneration}
}

func (r *rowSource) Err() error { return nil }

// loadCorpus creates the parent rows and COPYs every message in load order. It is used for the
// template database and (with a lz4 pool) for the per-message-lz4 reference.
func loadCorpus(ctx context.Context, pool *pgxpool.Pool, src Source) error {
	runs := src.Runs()
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email) VALUES ($1, 'bench@example.invalid')`, userID); err != nil {
		return fmt.Errorf("seed user: %w", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'github', 'https://example.invalid', 'bench', 1, '\x00')`, connID, userID); err != nil {
		return fmt.Errorf("seed connection: %w", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url)
		VALUES ($1, $2, 1, 'bench/bench', 'https://example.invalid/bench')`, repoID, connID); err != nil {
		return fmt.Errorf("seed repo: %w", err)
	}
	for i := range runs {
		r := &runs[i]
		fin := "NULL"
		if r.Terminal {
			fin = "now()"
		}
		var err error
		if r.Kind == "chat" {
			_, err = pool.Exec(ctx, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, finished_at)
				VALUES ($1, $2, 'chat', 'bench', 'bench', $3, `+fin+`)`, r.ID, userID, r.Status)
		} else {
			_, err = pool.Exec(ctx, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, finished_at)
				VALUES ($1, $2, $3, $4, 'bench', 'bench', $5, `+fin+`)`, r.ID, userID, repoID, i+1, r.Status)
		}
		if err != nil {
			return fmt.Errorf("seed run %d: %w", i, err)
		}
	}
	ids := make([]uuid.UUID, len(runs))
	for i := range runs {
		ids[i] = runs[i].ID
	}
	rs := &rowSource{ch: make(chan *Row, 4096), done: make(chan error, 1)}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		err := src.Stream(cctx, func(r *Row) error {
			select {
			case rs.ch <- r:
				return nil
			case <-cctx.Done():
				return cctx.Err()
			}
		})
		close(rs.ch)
		rs.done <- err
	}()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	// run index -> uuid mapping is applied by a wrapper so the source can stay id-free.
	wrapped := &idMapSource{inner: rs, ids: ids}
	if _, err := conn.Conn().CopyFrom(ctx, pgx.Identifier{"run_messages"},
		[]string{"run_id", "seq", "kind", "agent", "payload", "created_at", "agent_instance", "agent_label", "claim_generation"}, wrapped); err != nil {
		cancel()
		<-rs.done
		return fmt.Errorf("copy run_messages: %w", err)
	}
	if err := <-rs.done; err != nil {
		return err
	}
	_, err = pool.Exec(ctx, `ANALYZE`)
	return err
}

type idMapSource struct {
	inner *rowSource
	ids   []uuid.UUID
}

func (m *idMapSource) Next() bool { return m.inner.Next() }
func (m *idMapSource) Err() error { return m.inner.Err() }
func (m *idMapSource) Values() ([]any, error) {
	v := m.inner.values()
	v[0] = m.ids[v[0].(int)]
	return v, nil
}
