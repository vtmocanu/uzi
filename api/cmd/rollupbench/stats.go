package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"runtime"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Latency summarises one cell of samples. Percentiles are nearest-rank; p99 over about a
// thousand samples is indicative only (it is the 10th worst sample).
type Latency struct {
	N      int     `json:"n"`
	MeanMs float64 `json:"mean_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MaxMs  float64 `json:"max_ms"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func summarize(d []time.Duration) Latency {
	if len(d) == 0 {
		return Latency{}
	}
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum time.Duration
	for _, v := range s {
		sum += v
	}
	rank := func(p float64) time.Duration {
		i := int(p*float64(len(s))+0.999999) - 1
		return s[max(0, min(len(s)-1, i))]
	}
	return Latency{N: len(s), MeanMs: ms(sum) / float64(len(s)), P50Ms: ms(rank(0.50)), P95Ms: ms(rank(0.95)), P99Ms: ms(rank(0.99)), MaxMs: ms(s[len(s)-1])}
}

// cpuSeconds is this process's user+system CPU time.
func cpuSeconds() float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return float64(ru.Utime.Sec) + float64(ru.Utime.Usec)/1e6 + float64(ru.Stime.Sec) + float64(ru.Stime.Usec)/1e6
}

// heapSampler records the peak HeapInuse while running.
type heapSampler struct {
	stop chan struct{}
	done sync.WaitGroup
	peak uint64
}

func startHeapSampler() *heapSampler {
	h := &heapSampler{stop: make(chan struct{})}
	h.done.Add(1)
	go func() {
		defer h.done.Done()
		t := time.NewTicker(25 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-h.stop:
				return
			case <-t.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				h.peak = max(h.peak, m.HeapInuse)
			}
		}
	}()
	return h
}

func (h *heapSampler) finish() uint64 {
	close(h.stop)
	h.done.Wait()
	return h.peak
}

var (
	uuidRe = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	dsnRe  = regexp.MustCompile(`postgres(?:ql)?://[^\s"']+`)
)

// scrub removes anything that looks like a run id or a DSN from text bound for a results file.
func scrub(s string) string {
	return dsnRe.ReplaceAllString(uuidRe.ReplaceAllString(s, "<uuid>"), "<dsn>")
}

func digestRecords(recs []Record) string {
	h := sha256.New()
	var buf []byte
	for i := range recs {
		buf = appendRecord(buf[:0], &recs[i])
		h.Write(buf)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func digestChats(rows []ChatRow) string {
	h := sha256.New()
	for _, r := range rows {
		_, _ = fmt.Fprintf(h, "%s|", r.ID)
		if r.LastMsg != nil {
			_, _ = fmt.Fprintf(h, "%d", r.LastMsg.UnixMicro())
		}
		_, _ = fmt.Fprint(h, ";")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// logger prints progress lines to stderr with elapsed time.
type logger struct{ start time.Time }

func (l *logger) f(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[%7.1fs] %s\n", time.Since(l.start).Seconds(), fmt.Sprintf(format, a...))
}

// loopWorkers runs fn(worker, i) for n total iterations spread over workers goroutines and
// returns every call's duration. A non-nil error from any call is counted, and the first one
// is returned alongside the samples.
func loopWorkers(ctx context.Context, workers, n int, fn func(worker, i int) error) ([]time.Duration, int, error) {
	per := (n + workers - 1) / workers
	out := make([][]time.Duration, workers)
	errs := make([]int, workers)
	firsts := make([]error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < per && ctx.Err() == nil; i++ {
				t0 := time.Now()
				if err := fn(w, i); err != nil {
					errs[w]++
					if firsts[w] == nil {
						firsts[w] = err
					}
					continue
				}
				out[w] = append(out[w], time.Since(t0))
			}
		}()
	}
	wg.Wait()
	var all []time.Duration
	var first error
	nerr := 0
	for w := range out {
		all = append(all, out[w]...)
		nerr += errs[w]
		if first == nil {
			first = firsts[w]
		}
	}
	return all, nerr, first
}
