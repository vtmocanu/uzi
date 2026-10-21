package forge

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var ciProviders = []Type{TypeGitHub, TypeGitLab, TypeForgejo}

// ciWireDriver exercises the real SDK, auth, guarded transport and redaction.
func ciWireDriver(t *testing.T, provider Type, handler http.HandlerFunc) Forge {
	t.Helper()
	return ciWireDriverTimeout(t, provider, 5*time.Second, handler)
}

// ciWireDriverTimeout is ciWireDriver with an explicit client timeout, for tables whose
// multi-MiB fixtures can outlast the 5s default under -race (#3190).
func ciWireDriverTimeout(t *testing.T, provider Type, timeout time.Duration, handler http.HandlerFunc) Forge {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repositories/7") {
			_, _ = io.WriteString(w, `{"id":7,"name":"widgets","owner":{"login":"acme"}}`)
			return
		}
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	d, err := NewWithGitLabBackoff(provider, srv.URL, "ci-fixture-credential", timeout,
		func(_, _ time.Duration, _ int, _ *http.Response) time.Duration { return 0 })
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func ciRunJSON(provider Type, key string) string {
	switch provider {
	case TypeGitHub:
		return `{"id":1,"workflow_id":` + key + `,"name":"display","head_branch":"main"}`
	case TypeGitLab:
		data, _ := json.Marshal(map[string]any{"id": 1, "name": key, "ref": "main"})
		return string(data)
	default:
		data, _ := json.Marshal(map[string]any{"id": 1, "path": key, "head_branch": "main"})
		return string(data)
	}
}

func ciRunsJSON(provider Type, item string) string {
	if provider == TypeGitLab {
		return "[" + item + "]"
	}
	return `{"workflow_runs":[` + item + "]}"
}

func ciBody(provider Type, size int) string {
	key := " workflow.yml "
	if provider == TypeGitHub {
		key = "42"
	}
	body := ciRunsJSON(provider, ciRunJSON(provider, key))
	return body + strings.Repeat(" ", size-len(body))
}

func requireCIBudget(t *testing.T, err error) {
	t.Helper()
	var typed *CIReadBudgetError
	if !errors.Is(err, ErrCIReadBudget) || !errors.As(err, &typed) {
		t.Fatalf("want typed CI read budget error, got %T: %v", err, err)
	}
	if strings.Contains(err.Error(), "ci-fixture-credential") {
		t.Fatal("credential leaked")
	}
}

func TestCIWorkflowHTTPMappingsAndLimits(t *testing.T) {
	for _, provider := range ciProviders {
		t.Run(string(provider), func(t *testing.T) {
			key, pageSize := " workflow.yml ", "100"
			if provider == TypeGitHub {
				key = "42"
			}
			if provider == TypeForgejo {
				pageSize = "50"
			}
			var lists atomic.Int32
			d := ciWireDriver(t, provider, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/1") {
					_, _ = io.WriteString(w, ciRunJSON(provider, key))
					return
				}
				lists.Add(1)
				qkey := "per_page"
				if provider == TypeForgejo {
					qkey = "limit"
				}
				if r.URL.Query().Get(qkey) != pageSize {
					t.Errorf("query = %s", r.URL.RawQuery)
				}
				_, _ = io.WriteString(w, ciRunsJSON(provider, ciRunJSON(provider, key)))
			})
			ctx := WithCIRepairReadLimits(context.Background())
			list, err := d.ListWorkflowRuns(ctx, 7, ListWorkflowRunsOptions{Limit: 100})
			if err != nil || len(list) != 1 || list[0].WorkflowKey != key {
				t.Fatalf("list = %+v, %v", list, err)
			}
			detail, err := d.GetWorkflowRun(ctx, 7, 1)
			if err != nil || detail.WorkflowKey != key {
				t.Fatalf("detail = %+v, %v", detail, err)
			}
			if lists.Load() != 1 {
				t.Fatalf("list made %d pages", lists.Load())
			}
		})
	}
}

func TestCIWorkflowHTTPInvalidIdentity(t *testing.T) {
	for _, provider := range ciProviders {
		for _, key := range []string{"", strings.Repeat("é", 101)} {
			if provider == TypeGitHub {
				key = "0"
			}
			t.Run(string(provider)+strconv.Itoa(len(key)), func(t *testing.T) {
				d := ciWireDriver(t, provider, func(w http.ResponseWriter, r *http.Request) {
					body := ciRunJSON(provider, key)
					if provider == TypeGitLab && key == "" {
						body = strings.ReplaceAll(body, `"ref":"main"`, `"ref":""`)
					}
					if !strings.HasSuffix(r.URL.Path, "/1") {
						body = ciRunsJSON(provider, body)
					}
					_, _ = io.WriteString(w, body)
				})
				runs, err := d.ListWorkflowRuns(WithCIRepairReadLimits(context.Background()), 7, ListWorkflowRunsOptions{Limit: 100})
				requireCIBudget(t, err)
				if runs != nil {
					t.Fatal("partial list escaped")
				}
				run, err := d.GetWorkflowRun(WithCIRepairReadLimits(context.Background()), 7, 1)
				requireCIBudget(t, err)
				if run != (WorkflowRun{}) {
					t.Fatal("partial detail escaped")
				}
				if _, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{Limit: 100}); err != nil {
					t.Fatalf("unmarked behavior changed: %v", err)
				}
			})
		}
	}
	for _, key := range []string{strings.Repeat("é", 100), " ", " exact identity "} {
		if err := validateCIWorkflow(WithCIRepairReadLimits(context.Background()), WorkflowRun{ID: 1, WorkflowKey: key}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCIWorkflowHTTPExecutionIdentity(t *testing.T) {
	for _, provider := range ciProviders {
		for _, mode := range []string{"missing", "zero", "negative"} {
			t.Run(string(provider)+"/"+mode, func(t *testing.T) {
				key := "workflow.yml"
				if provider == TypeGitHub {
					key = "42"
				}
				d := ciWireDriver(t, provider, func(w http.ResponseWriter, r *http.Request) {
					body := ciRunJSON(provider, key)
					switch mode {
					case "missing":
						body = strings.ReplaceAll(body, `"id":1,`, "")
					case "zero":
						body = strings.ReplaceAll(body, `"id":1`, `"id":0`)
					case "negative":
						body = strings.ReplaceAll(body, `"id":1`, `"id":-1`)
					}
					if !strings.HasSuffix(r.URL.Path, "/1") {
						body = ciRunsJSON(provider, body)
					}
					_, _ = io.WriteString(w, body)
				})
				runs, err := d.ListWorkflowRuns(WithCIRepairReadLimits(context.Background()), 7, ListWorkflowRunsOptions{Limit: 100})
				requireCIBudget(t, err)
				if runs != nil {
					t.Fatal("invalid execution escaped in list")
				}
				run, err := d.GetWorkflowRun(WithCIRepairReadLimits(context.Background()), 7, 1)
				requireCIBudget(t, err)
				if run != (WorkflowRun{}) {
					t.Fatal("invalid execution escaped in detail")
				}
				if runs, err := d.ListWorkflowRuns(context.Background(), 7, ListWorkflowRunsOptions{Limit: 100}); err != nil || len(runs) != 1 {
					t.Fatalf("unmarked behavior changed: %+v, %v", runs, err)
				}
			})
		}
	}
}

func TestCIWorkflowHTTPBodyBudgets(t *testing.T) {
	for _, provider := range ciProviders {
		for _, mode := range []string{"exact", "plus-one", "gzip", "gzip-overflow", "unsupported", "error-overflow", "huge-item", "stream"} {
			t.Run(string(provider)+"/"+mode, func(t *testing.T) {
				// Build the multi-MiB body before the timed request, not inside the handler.
				var body string
				switch mode {
				case "huge-item":
					body = ciRunsJSON(provider, `{"id":1,"name":"`+strings.Repeat("x", ciRepairJSONBytes)+`"}`)
				case "plus-one", "gzip-overflow", "error-overflow":
					body = ciBody(provider, ciRepairJSONBytes) + " "
				case "stream":
				default:
					body = ciBody(provider, ciRepairJSONBytes)
				}
				d := ciWireDriverTimeout(t, provider, 30*time.Second, func(w http.ResponseWriter, _ *http.Request) {
					switch mode {
					case "unsupported":
						w.Header().Set("Content-Encoding", "br")
					case "stream":
						w.(http.Flusher).Flush()
						for i := 0; i < 128; i++ {
							if _, err := io.WriteString(w, strings.Repeat(" ", 64<<10)); err != nil {
								break
							}
						}
						return
					}
					if mode == "error-overflow" {
						w.WriteHeader(http.StatusUnauthorized)
					}
					if strings.HasPrefix(mode, "gzip") {
						w.Header().Set("Content-Encoding", "gzip")
						z := gzip.NewWriter(w)
						_, _ = io.WriteString(z, body)
						_ = z.Close()
					} else {
						_, _ = io.WriteString(w, body)
					}
				})
				runs, err := d.ListWorkflowRuns(WithCIRepairReadLimits(context.Background()), 7, ListWorkflowRunsOptions{Limit: 100})
				if mode == "exact" || mode == "gzip" {
					if err != nil || len(runs) != 1 {
						t.Fatalf("exact body: %v", err)
					}
				} else {
					requireCIBudget(t, err)
					if runs != nil {
						t.Fatal("partial result escaped")
					}
				}
			})
		}
	}
}

func TestCICumulativeAcrossWorkflowAndJobSnapshots(t *testing.T) {
	for _, provider := range ciProviders {
		t.Run(string(provider), func(t *testing.T) {
			d := ciWireDriver(t, provider, func(w http.ResponseWriter, r *http.Request) {
				body := ciBody(provider, ciRepairJSONBytes)
				if strings.HasSuffix(r.URL.Path, "/jobs") {
					body = ciJobsJSON(provider, 1)
					body += strings.Repeat(" ", ciRepairJSONBytes-len(body))
				}
				_, _ = io.WriteString(w, body)
			})
			ctx := WithCIRepairReadLimits(context.Background())
			for i := 0; i < 2; i++ {
				if _, err := d.ListWorkflowRuns(ctx, 7, ListWorkflowRunsOptions{Limit: 100}); err != nil {
					t.Fatal(err)
				}
				if _, err := d.ListPipelineJobs(ctx, 7, 1); err != nil {
					t.Fatal(err)
				}
			}
			runs, err := d.ListWorkflowRuns(ctx, 7, ListWorkflowRunsOptions{Limit: 100})
			requireCIBudget(t, err)
			if runs != nil {
				t.Fatal("partial result")
			}
			if _, err := d.ListWorkflowRuns(WithCIRepairReadLimits(context.Background()), 7, ListWorkflowRunsOptions{Limit: 100}); err != nil {
				t.Fatal("new fire did not reset:", err)
			}
		})
	}
}

func ciJobsJSON(provider Type, count int) string {
	items := make([]map[string]any, count)
	for i := range items {
		items[i] = map[string]any{"id": i + 1, "name": "test", "status": "success"}
	}
	data, _ := json.Marshal(items)
	if provider == TypeGitLab {
		return string(data)
	}
	return `{"jobs":` + string(data) + "}"
}

func TestCIJobHTTPPagination(t *testing.T) {
	for _, provider := range ciProviders {
		for _, mode := range []string{"terminal-ten", "eleven", "loop", "empty", "oversized", "oversized-total", "invalid-id"} {
			t.Run(string(provider)+"/"+mode, func(t *testing.T) {
				var calls atomic.Int32
				pageSize := 100
				if provider == TypeForgejo {
					pageSize = 50
				}
				d := ciWireDriver(t, provider, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					page, _ := strconv.Atoi(r.URL.Query().Get("page"))
					if page == 0 {
						page = 1
					}
					next := page + 1
					if mode == "terminal-ten" && page == 10 {
						next = 0
					}
					if mode == "loop" {
						next = page
					}
					if mode == "oversized" || mode == "oversized-total" || mode == "invalid-id" {
						next = 0
					}
					if next > 0 {
						w.Header().Set("X-Next-Page", strconv.Itoa(next))
						w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=%d>; rel=\"next\"", r.Host, r.URL.Path, next))
					}
					count := pageSize
					switch mode {
					case "empty":
						count = 0
					case "oversized":
						count++
					case "oversized-total":
						count = 1001
					}
					body := ciJobsJSON(provider, count)
					if mode == "invalid-id" {
						body = strings.ReplaceAll(body, `"id":1,`, `"id":0,`)
					}
					_, _ = io.WriteString(w, body)
				})
				jobs, err := d.ListPipelineJobs(WithCIRepairReadLimits(context.Background()), 7, 1)
				if mode == "terminal-ten" {
					if err != nil || len(jobs) != 10*pageSize || calls.Load() != 10 {
						t.Fatalf("terminal snapshot: %d jobs, %d calls, %v", len(jobs), calls.Load(), err)
					}
				} else {
					requireCIBudget(t, err)
					if jobs != nil {
						t.Fatal("partial jobs escaped")
					}
					wantCalls := int32(1)
					if mode == "eleven" {
						wantCalls = 10
					}
					if calls.Load() != wantCalls {
						t.Fatalf("calls=%d want %d", calls.Load(), wantCalls)
					}
				}
			})
		}
	}
}

func TestCIGitLabRetriesConsumeErrorBodyBudget(t *testing.T) {
	var calls atomic.Int32
	d := ciWireDriver(t, TypeGitLab, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, strings.Repeat(" ", ciRepairJSONBytes))
	})
	runs, err := d.ListWorkflowRuns(WithCIRepairReadLimits(context.Background()), 7, ListWorkflowRunsOptions{Limit: 100})
	requireCIBudget(t, err)
	if runs != nil || calls.Load() != 5 {
		t.Fatalf("runs=%v requests=%d", runs, calls.Load())
	}
}

func TestCILogTailsExcluded(t *testing.T) {
	for _, provider := range []Type{TypeGitLab, TypeForgejo} {
		t.Run(string(provider), func(t *testing.T) {
			d := ciWireDriver(t, provider, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "log tail") })
			ctx := WithCIRepairReadLimits(context.Background())
			ciBudget(ctx).remaining = 0
			log, err := d.JobLogTail(ctx, 7, 1, 100)
			if err != nil || log != "log tail" {
				t.Fatalf("log=%q error=%v", log, err)
			}
			if ciBudget(ctx).remaining != 0 {
				t.Fatal("log consumed discovery budget")
			}
		})
	}
}

// Explicit gzip decoding also covers transports that do not auto-decompress.
func TestCITransportExplicitGzipAndClose(t *testing.T) {
	var compressed bytes.Buffer
	z := gzip.NewWriter(&compressed)
	_, _ = io.WriteString(z, strings.Repeat(" ", ciRepairJSONBytes+1))
	_ = z.Close()
	body := &ciCloseReader{Reader: bytes.NewReader(compressed.Bytes())}
	base := ciRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": []string{"gzip"}}, Body: body}, nil
	})
	req, _ := http.NewRequestWithContext(markCIJSON(WithCIRepairReadLimits(context.Background())), "GET", "https://example.test", nil)
	_, err := (ciRepairTransport{base: base}).RoundTrip(req)
	requireCIBudget(t, err)
	if !body.closed {
		t.Fatal("overflow did not close original body")
	}
}

type ciRoundTripper func(*http.Request) (*http.Response, error)

func (f ciRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type ciCloseReader struct {
	io.Reader
	closed bool
}

func (r *ciCloseReader) Close() error { r.closed = true; return nil }
