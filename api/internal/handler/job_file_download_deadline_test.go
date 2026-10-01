package handler

import (
	"math"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJobFileDownloadDeadlineBounds(t *testing.T) {
	for _, c := range []struct {
		name     string
		bytes    int64
		duration time.Duration
	}{
		{"small", 1, v1DownloadDeadlineFloor},
		{"large", 25 << 20, v1DownloadDeadlineFloor + 256*time.Second},
		{"capped", math.MaxInt64, v1DownloadMaxDeadline},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := &workerDownloadDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			before := time.Now()
			setJobFileDownloadDeadline(rec, c.bytes)
			after := time.Now()
			if rec.writeDeadline.Before(before.Add(c.duration)) || rec.writeDeadline.After(after.Add(c.duration)) {
				t.Fatalf("deadline=%v, want allowance %s", rec.writeDeadline, c.duration)
			}
		})
	}
}
