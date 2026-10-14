package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/controller/internal/preset"
)

func TestWarnSharedCPU(t *testing.T) {
	resolver, err := preset.NewResolver("registry.example.com/uzi", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	sizes := preset.SizeNames()
	sort.Strings(sizes)
	if !reflect.DeepEqual(sizes, []string{"l", "m", "s"}) {
		t.Fatalf("available sizes = %v; update explicit CPU expectations", sizes)
	}
	limits := map[string]string{"s": "1", "m": "4", "l": "4"}
	for _, slots := range []int{0, 1, 2, 256} {
		t.Run(fmt.Sprintf("slots=%d", slots), func(t *testing.T) {
			var output bytes.Buffer
			warnSharedCPU(slog.New(slog.NewJSONHandler(&output, nil)), slots, resolver)
			if slots <= 1 {
				if output.Len() != 0 {
					t.Fatalf("single-slot configuration must be silent: %s", &output)
				}
				return
			}
			decoder := json.NewDecoder(&output)
			for _, size := range sizes {
				var entry struct {
					Level    string `json:"level"`
					Message  string `json:"msg"`
					Size     string `json:"size"`
					Slots    int    `json:"configured_run_slots"`
					Limit    string `json:"cpu_limit"`
					Advisory string `json:"advisory"`
				}
				if err := decoder.Decode(&entry); err != nil {
					t.Fatalf("missing warning for %s: %v", size, err)
				}
				if entry.Level != "WARN" || entry.Message != "hosted-worker-shared-cpu" ||
					entry.Size != size || entry.Slots != slots || entry.Limit != limits[size] {
					t.Errorf("warning for %s = %+v; want WARN token, slots %d, limit %s", size, entry, slots, limits[size])
				}
				spec, err := resolver.Resolve("base", size)
				if err != nil {
					t.Fatal(err)
				}
				if entry.Limit != spec.Size.CPULimit.String() {
					t.Errorf("logged CPU limit %q differs from current resolver %s", entry.Limit, spec.Size.CPULimit.String())
				}
				for _, text := range []string{"Available hosted-worker configuration", "concurrent runs share a fixed CPU quota", "measure workload performance or lower concurrency", "not actual fleet or active occupancy"} {
					if !strings.Contains(entry.Advisory, text) {
						t.Errorf("advisory %q missing %q", entry.Advisory, text)
					}
				}
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Fatalf("unexpected extra log: %v, %v", extra, err)
			}
		})
	}
}
