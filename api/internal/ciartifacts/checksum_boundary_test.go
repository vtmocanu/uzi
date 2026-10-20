package ciartifacts

import (
	"bytes"
	"context"
	"testing"
)

func TestCollectorChecksumDisagreementKeepsUnrelatedEvidence(t *testing.T) {
	for _, header := range []string{"central", "descriptor"} {
		t.Run(header, func(t *testing.T) {
			data := zipFixture(t, zipItem{name: "bad.txt", text: "bad input"}, zipItem{name: "good.txt", text: "good evidence"})
			central := bytes.Index(data, []byte{'P', 'K', 1, 2})
			at := central + 16
			if header == "descriptor" {
				at = bytes.Index(data[:central], []byte{'P', 'K', 7, 8}) + 4
			}
			data[at] ^= 1
			c := NewCollector(Limits{}, nil)
			c.Add(context.Background(), "42", "logs", data)
			m, inputs := c.Result()
			if len(inputs) != 1 || string(inputs[0].Content) != "good evidence" || len(m.Entries) != 2 || m.Entries[0].Reason != "invalid_entry" {
				t.Fatalf("checksum failure discarded unrelated safe evidence: files=%d manifest=%+v", len(inputs), m)
			}
			if c.inflated != int64(len("bad input")+len("good evidence")) {
				t.Fatalf("checksum-rejected bytes escaped work accounting: %d", c.inflated)
			}
		})
	}
}
