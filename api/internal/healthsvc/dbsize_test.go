package healthsvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func dbSizeSvc(capacity int64, probe func(context.Context, bool) (store.DatabaseSize, error)) *Service {
	s := newSvc(&fakeStore{}, &fakeSettings{})
	s.cfg.DBStorageCapacityBytes = capacity
	s.probeDBSize = probe
	return s
}

func sizeProbe(size int64, rels ...store.RelationSize) func(context.Context, bool) (store.DatabaseSize, error) {
	return func(context.Context, bool) (store.DatabaseSize, error) {
		return store.DatabaseSize{SizeBytes: size, Largest: rels}, nil
	}
}

func evidenceValue(c apitypes.HealthCheckDTO, label string) (string, bool) {
	for _, e := range c.Evidence {
		if e.Label == label {
			return e.Value, true
		}
	}
	return "", false
}

func TestCheckDBSizeBands(t *testing.T) {
	const capacity = int64(1000)
	for _, tc := range []struct {
		name string
		size int64
		want string
	}{
		{"74 percent", 740, sevOK},
		{"just under 75", 749, sevOK},
		{"75 percent", 750, sevWarn},
		{"84 percent", 840, sevWarn},
		{"just under 85", 849, sevWarn},
		{"85 percent", 850, sevDanger},
		{"over 100 percent", 1500, sevDanger},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dbSizeSvc(capacity, sizeProbe(tc.size)).checkDBSize(context.Background(), fixedNow)
			if c.Severity != tc.want {
				t.Fatalf("severity = %q, want %q (%+v)", c.Severity, tc.want, c)
			}
			if (tc.want == sevOK) != (c.Action == nil) {
				t.Errorf("action presence = %v for severity %q", c.Action != nil, c.Severity)
			}
		})
	}
}

func TestCheckDBSizeNoOverflowAtBound(t *testing.T) {
	const capacity = int64(1) << 60
	c := dbSizeSvc(capacity, sizeProbe(capacity)).checkDBSize(context.Background(), fixedNow)
	if c.Severity != sevDanger {
		t.Fatalf("size == 2^60 capacity: severity = %q, want danger", c.Severity)
	}
	c = dbSizeSvc(capacity, sizeProbe(capacity/100*74)).checkDBSize(context.Background(), fixedNow)
	if c.Severity != sevOK {
		t.Fatalf("74%% of 2^60: severity = %q, want ok", c.Severity)
	}
}

func TestCheckDBSizeSummaryAndEvidence(t *testing.T) {
	c := dbSizeSvc(1000, sizeProbe(860,
		store.RelationSize{Name: "runs", SizeBytes: 500},
		store.RelationSize{Name: "evil\x1b[31m\nname", SizeBytes: 2048},
		store.RelationSize{Name: "c", SizeBytes: 3},
		store.RelationSize{Name: "dropped", SizeBytes: 1},
	)).checkDBSize(context.Background(), fixedNow)
	if c.Summary != "Database is using 86% of its configured storage capacity." {
		t.Errorf("summary = %q", c.Summary)
	}
	if v, _ := evidenceValue(c, "Used"); v != "86.0%" {
		t.Errorf("Used = %q", v)
	}
	if _, ok := evidenceValue(c, "Size"); !ok {
		t.Error("missing Size evidence")
	}
	if _, ok := evidenceValue(c, "Capacity"); !ok {
		t.Error("missing Capacity evidence")
	}
	rels := 0
	for _, e := range c.Evidence {
		if e.Label != "Largest relation" {
			continue
		}
		rels++
		if strings.ContainsAny(e.Value, "\x1b\n") {
			t.Errorf("relation evidence carries control bytes: %q", e.Value)
		}
	}
	if rels != 3 {
		t.Errorf("largest relation rows = %d, want 3", rels)
	}
	if c.Doc == nil || *c.Doc != "admin-health" {
		t.Errorf("doc = %v", c.Doc)
	}
}

func TestCheckDBSizeNA(t *testing.T) {
	for _, capacity := range []int64{0, -1} {
		c := dbSizeSvc(capacity, sizeProbe(5000)).checkDBSize(context.Background(), fixedNow)
		if c.Severity != sevNA || !strings.Contains(c.Summary, "DB_STORAGE_CAPACITY_BYTES") {
			t.Fatalf("capacity %d: %+v", capacity, c)
		}
		if _, ok := evidenceValue(c, "Size"); !ok {
			t.Errorf("capacity %d: na with a successful read should carry Size evidence", capacity)
		}
	}
	failing := func(context.Context, bool) (store.DatabaseSize, error) {
		return store.DatabaseSize{}, errors.New("boom")
	}
	for name, probe := range map[string]func(context.Context, bool) (store.DatabaseSize, error){"read error": failing, "nil probe": nil} {
		c := dbSizeSvc(0, probe).checkDBSize(context.Background(), fixedNow)
		if c.Severity != sevNA || len(c.Evidence) != 0 {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

func TestCheckDBSizeUnknown(t *testing.T) {
	failing := func(context.Context, bool) (store.DatabaseSize, error) {
		return store.DatabaseSize{}, errors.New("secret detail")
	}
	c := dbSizeSvc(1000, failing).checkDBSize(context.Background(), fixedNow)
	if c.Severity != sevUnknown || strings.Contains(c.Summary, "secret detail") {
		t.Errorf("read error: %+v", c)
	}
	c = dbSizeSvc(1000, nil).checkDBSize(context.Background(), fixedNow)
	if c.Severity != sevUnknown {
		t.Errorf("nil probe: %+v", c)
	}
}

func TestCheckDBSizeCache(t *testing.T) {
	calls := 0
	size := int64(100)
	probe := func(context.Context, bool) (store.DatabaseSize, error) {
		calls++
		return store.DatabaseSize{SizeBytes: size}, nil
	}
	s := dbSizeSvc(1000, probe)
	ctx := context.Background()
	if c := s.checkDBSize(ctx, fixedNow); c.Severity != sevOK {
		t.Fatalf("first: %+v", c)
	}
	size = 900
	if c := s.checkDBSize(ctx, fixedNow.Add(59*time.Second)); c.Severity != sevOK || calls != 1 {
		t.Fatalf("within TTL: severity %q calls %d", c.Severity, calls)
	}
	if c := s.checkDBSize(ctx, fixedNow.Add(60*time.Second)); c.Severity != sevDanger || calls != 2 {
		t.Fatalf("after TTL: severity %q calls %d", c.Severity, calls)
	}

	// Failures are cached for the same interval.
	fcalls := 0
	fs := dbSizeSvc(1000, func(context.Context, bool) (store.DatabaseSize, error) {
		fcalls++
		return store.DatabaseSize{}, errors.New("x")
	})
	fs.checkDBSize(ctx, fixedNow)
	fs.checkDBSize(ctx, fixedNow.Add(30*time.Second))
	if fcalls != 1 {
		t.Errorf("failure re-probed within TTL: %d calls", fcalls)
	}
}

func TestHumanBytesNeverShows1024(t *testing.T) {
	cases := map[int64]string{
		1023:               "1023 B",
		1024:               "1.0 KiB",
		1024*1024 - 1:      "1.0 MiB",
		1024*1024 - 52:     "1023.9 KiB",
		3 << 29:            "1.5 GiB",
		int64(1)<<30 - 100: "1.0 GiB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestCheckDBSizeRelationsUnavailableEvidence(t *testing.T) {
	probe := func(context.Context, bool) (store.DatabaseSize, error) {
		return store.DatabaseSize{SizeBytes: 500, RelationsUnavailable: true}, nil
	}
	c := dbSizeSvc(1000, probe).checkDBSize(context.Background(), fixedNow)
	if c.Severity != sevOK {
		t.Fatalf("severity = %q, want ok", c.Severity)
	}
	if v, ok := evidenceValue(c, "Largest relations"); !ok || v != "Unavailable (the relation-size query gave up; the size above is still current)" {
		t.Errorf("missing unavailable-relations evidence: %+v", c.Evidence)
	}
}

// A caller whose context is already cancelled must not make the probe fail, and the
// healthy result is what the next reader gets from the cache.
func TestCheckDBSizeProbeDetachedFromCallerContext(t *testing.T) {
	calls := 0
	probe := func(ctx context.Context, _ bool) (store.DatabaseSize, error) {
		calls++
		if err := ctx.Err(); err != nil {
			return store.DatabaseSize{}, err
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("probe context has no deadline")
		}
		return store.DatabaseSize{SizeBytes: 100}, nil
	}
	s := dbSizeSvc(1000, probe)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if c := s.checkDBSize(cancelled, fixedNow); c.Severity != sevOK {
		t.Fatalf("cancelled caller: %+v", c)
	}
	if c := s.checkDBSize(context.Background(), fixedNow.Add(time.Second)); c.Severity != sevOK || calls != 1 {
		t.Fatalf("next reader: severity %q calls %d", c.Severity, calls)
	}
}

func TestCheckDBSizeSkipsRelationsWhenNA(t *testing.T) {
	for capacity, want := range map[int64]bool{0: false, -1: false, 1000: true} {
		var got *bool
		probe := func(_ context.Context, withRelations bool) (store.DatabaseSize, error) {
			got = &withRelations
			return store.DatabaseSize{SizeBytes: 1}, nil
		}
		dbSizeSvc(capacity, probe).checkDBSize(context.Background(), fixedNow)
		if got == nil || *got != want {
			t.Errorf("capacity %d: withRelations = %v, want %v", capacity, got, want)
		}
	}
}

// A failed largest-relations query is logged once per probe refresh with the error as a
// field, and the error text never reaches the health text.
func TestCheckDBSizeLogsRelationsErr(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	relErr := errors.New("lock timeout RELERR-MARKER")
	s := dbSizeSvc(1000, func(context.Context, bool) (store.DatabaseSize, error) {
		return store.DatabaseSize{SizeBytes: 500, RelationsUnavailable: true, RelationsErr: relErr}, nil
	})
	c := s.checkDBSize(context.Background(), fixedNow)
	s.checkDBSize(context.Background(), fixedNow.Add(time.Second)) // cached: no second probe, no second log

	if n := strings.Count(buf.String(), "level=WARN"); n != 1 {
		t.Fatalf("warn lines = %d, want 1: %q", n, buf.String())
	}
	if !strings.Contains(buf.String(), "RELERR-MARKER") {
		t.Errorf("log lacks the relations error: %q", buf.String())
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "RELERR-MARKER") {
		t.Errorf("relations error leaked into the check: %s", raw)
	}
}
