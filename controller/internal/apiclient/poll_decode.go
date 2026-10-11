package apiclient

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/vtmocanu/uzi/controller/internal/protocol"
)

const maxPollBytes = 4 << 20
const maxPollWorkers = 512

// decodePoll bounds actual decompressed bytes, independent of ContentLength.
// Every rejected snapshot returns no workers and a payload-free error.
func decodePoll(resp *http.Response) (protocol.PollResponse, error) {
	reject := func() (protocol.PollResponse, error) {
		return protocol.PollResponse{}, fmt.Errorf("apiclient: invalid or oversized poll response")
	}
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(reader)
		if err != nil {
			return reject()
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxPollBytes+1))
	if err != nil || len(data) > maxPollBytes {
		return reject()
	}
	raw, err := scanPollWorkers(data)
	if err != nil {
		return reject()
	}
	out := protocol.PollResponse{Workers: make([]protocol.DesiredWorker, 0, len(raw))}
	seen := make(map[string]bool, len(raw))
	for _, worker := range raw {
		// Bound strings and capability arrays before allocating the typed worker.
		if !boundedWorker(worker) {
			return reject()
		}
		var w protocol.DesiredWorker
		if json.Unmarshal(worker, &w) != nil || !validUUID(w.ID) {
			return reject()
		}
		id := strings.ToLower(w.ID)
		if seen[id] {
			return reject()
		}
		seen[id] = true
		if w.DrainingReason != nil && *w.DrainingReason != "image_roll" {
			return reject()
		}
		if e := w.RollEvidence; e != nil {
			if e.Version != 1 || !validUUID(e.OwnerID) || e.EvaluatedAt.IsZero() || e.ValidUntil.IsZero() ||
				!validSet(e.SchedulingCapabilities) || !validSet(e.ProtocolCapabilities) {
				return reject()
			}
			if e.PodUID != nil && !validUUID(*e.PodUID) {
				return reject()
			}
			if e.RegisterNonce != nil && (len(*e.RegisterNonce) == 0 || len(*e.RegisterNonce) > 200) {
				return reject()
			}
			if e.RegisteredVersion != nil && len(*e.RegisteredVersion) > 200 {
				return reject()
			}
			if e.Registered != (e.RegisterNonce != nil) ||
				(!e.Registered && (e.PodUID != nil || e.HeartbeatAt != nil)) {
				return reject()
			}
		}
		out.Workers = append(out.Workers, w)
	}
	return out, nil
}

// scanPollWorkers reads at most 512 RawMessages. In particular it does not
// unmarshal an untrusted workers array into thousands of entries first.
// Unknown top-level fields remain forward compatible and are byte bounded.
func scanPollWorkers(data []byte) ([]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("object required")
	}
	var workers []json.RawMessage
	found := false
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if key != "workers" {
			var ignored json.RawMessage
			if err := dec.Decode(&ignored); err != nil {
				return nil, err
			}
			continue
		}
		if found {
			return nil, fmt.Errorf("duplicate workers")
		}
		found = true
		token, err = dec.Token()
		if err != nil || token != json.Delim('[') {
			return nil, fmt.Errorf("array required")
		}
		for dec.More() {
			if len(workers) == maxPollWorkers {
				return nil, fmt.Errorf("fleet overflow")
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return nil, err
			}
			if len(raw) == 0 || raw[0] != '{' {
				return nil, fmt.Errorf("worker object required")
			}
			workers = append(workers, raw)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("workers absent")
	}
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing input")
	}
	return workers, nil
}

func boundedWorker(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Token scanning holds one string at a time; the body cap bounds attempts.
	for {
		token, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return false
		}
		if s, ok := token.(string); ok && len(s) > 4096 {
			return false
		}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	if evidence, ok := fields["roll_evidence"]; ok && string(evidence) != "null" {
		var e map[string]json.RawMessage
		if json.Unmarshal(evidence, &e) != nil {
			return false
		}
		for _, key := range []string{"scheduling_capabilities", "protocol_capabilities"} {
			dec := json.NewDecoder(bytes.NewReader(e[key]))
			token, err := dec.Token()
			if err != nil || token != json.Delim('[') {
				return false
			}
			count := 0
			for dec.More() {
				if count == 64 {
					return false
				}
				var s string
				if dec.Decode(&s) != nil || len(s) > 200 {
					return false
				}
				count++
			}
		}
	}
	return true
}

func validUUID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	return err == nil && s != "00000000-0000-0000-0000-000000000000"
}

func validSet(set []string) bool {
	if len(set) > 64 {
		return false
	}
	seen := make(map[string]bool, len(set))
	for _, s := range set {
		if s == "" || len(s) > 200 || seen[s] {
			return false
		}
		for _, r := range s {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
				return false
			}
		}
		seen[s] = true
	}
	return true
}
