package analytics

import (
	"archive/zip"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// zoneNames enumerates IANA zone names from the first available source: the
// Go toolchain's zoneinfo.zip, then a system zoneinfo directory.
func zoneNames(t *testing.T) []string {
	t.Helper()
	var names []string
	if out, err := exec.Command("go", "env", "GOROOT").Output(); err == nil {
		zp := filepath.Join(strings.TrimSpace(string(out)), "lib", "time", "zoneinfo.zip")
		if zr, err := zip.OpenReader(zp); err == nil {
			defer func() { _ = zr.Close() }()
			for _, f := range zr.File {
				if !f.FileInfo().IsDir() {
					names = append(names, f.Name)
				}
			}
		}
	}
	if len(names) == 0 {
		root := "/usr/share/zoneinfo"
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(root, p)
			if strings.HasPrefix(rel, "posix/") || strings.HasPrefix(rel, "right/") || strings.Contains(rel, ".") {
				return nil
			}
			if _, e := os.Stat(p); e == nil {
				names = append(names, rel)
			}
			return nil
		})
	}
	sort.Strings(names)
	return names
}

// yearTransitions lists the zone transitions inside year by iterating
// ZoneBounds, stepping over stretches where Go reports an end that is not
// after the queried instant.
func yearTransitions(loc *time.Location, year int) []time.Time {
	t := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(year+1, 1, 1, 0, 0, 0, 0, time.UTC)
	var out []time.Time
	for t.Before(end) {
		_, e := t.In(loc).ZoneBounds()
		if e.IsZero() || !e.Before(end) {
			break
		}
		if !e.After(t) {
			t = t.Add(6 * time.Hour)
			continue
		}
		out = append(out, e)
		t = e
	}
	return out
}

func farFutureSignature(loc *time.Location) string {
	var sb strings.Builder
	end := time.Date(2046, 1, 1, 0, 0, 0, 0, time.UTC)
	for t := time.Date(2036, 1, 1, 13, 0, 0, 0, time.UTC); t.Before(end); t = t.AddDate(0, 0, 7) {
		_, o := t.In(loc).Zone()
		sb.WriteString(strconv.Itoa(o))
		sb.WriteByte(',')
	}
	return sb.String()
}

// Thinned from the plan's every-fifth-year list to keep the -race runtime near 30s.
var tilingYears = []int{1970, 1990, 2010, 2020, 2035, 2037, 2040, 2041, 2044}

func TestTilingEveryZone(t *testing.T) {
	names := zoneNames(t)
	if len(names) == 0 {
		t.Skip("no zone source found")
	}
	start := time.Now()
	samples, windows := 0, 0
	seenSig := map[string]bool{}
	perYear := map[int]time.Duration{}
	for _, name := range names {
		loc, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		// Far-future behaviour depends only on the zone's final rule, so the
		// expensive 2040/2044 year-end samples run once per distinct
		// weekly offset signature over 2036-2045 (every zone is still
		// sampled in all other years).
		sig := farFutureSignature(loc)
		firstOfSignature := !seenSig[sig]
		seenSig[sig] = true
		extra := false // 30d and all, once per zone
		for _, year := range tilingYears {
			yStart := time.Now()
			// Years from 2037 on hit Go's year-end ZoneBounds defect in many
			// zones, where each dayStart can take ~1e5 fallback steps. They
			// are thinned: noon everywhere, and the
			// year-end midnights only for 2040 and 2044 (7d window only).
			type sample struct {
				now     time.Time
				windows []Window
			}
			both := []Window{Window7d, Window90d}
			var samples1 []sample
			trs := yearTransitions(loc, year)
			if year >= 2037 {
				trs = nil
			}
			for _, tr := range trs {
				samples1 = append(samples1, sample{tr.Add(-time.Second), both}, sample{tr.Add(time.Second), both})
			}
			samples1 = append(samples1, sample{time.Date(year, 6, 15, 12, 0, 0, 0, time.UTC), both})
			if year < 2037 || ((year == 2040 || year == 2044) && firstOfSignature) {
				ws := both
				if year >= 2037 {
					ws = []Window{Window7d}
				}
				for _, d := range []Date{{year, 12, 31}, {year + 1, 1, 1}} {
					s, err := DayStart(d, loc)
					if err != nil {
						t.Fatalf("%s DayStart(%s): %v", name, d, err)
					}
					samples1 = append(samples1, sample{s, ws})
				}
			}
			for _, sm := range samples1 {
				now := sm.now
				ws := sm.windows
				if !extra && year == 2020 {
					ws = append(append([]Window{}, ws...), Window30d, WindowAll)
					extra = true
				}
				for _, w := range ws {
					var earliest *time.Time
					if w == WindowAll || w == Window30d {
						e := now.AddDate(0, 0, -400)
						earliest = &e
					}
					b, err := ComputeBounds(now, loc, w, earliest)
					if err != nil {
						t.Fatalf("%s %s now=%s: %v", name, w, now.UTC().Format(time.RFC3339), err)
					}
					if err := tilingError(b, loc); err != nil {
						t.Fatalf("%s %s now=%s: %v", name, w, now.UTC().Format(time.RFC3339), err)
					}
					windows++
				}
				samples++
			}
			perYear[year] += time.Since(yStart)
		}
	}
	t.Logf("per-year elapsed: %v", perYear)
	t.Logf("distinct far-future signatures (2040/2044 year-ends sampled): %d", len(seenSig))
	t.Logf("zones=%d samples=%d window computations=%d elapsed=%s", len(names), samples, windows, time.Since(start))
}
