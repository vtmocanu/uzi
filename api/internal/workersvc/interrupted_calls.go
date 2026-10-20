package workersvc

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// interruptedCallsTimeout bounds the read; tests scale it so they do not wait a real second.
var interruptedCallsTimeout = time.Second

type interruptedCallsReader interface {
	InterruptedCallsWindow(context.Context, store.InterruptedCallsWindowParams) ([]store.InterruptedCallsWindowRow, error)
}

// InterruptedCalls returns bounded JSON for the handler's JSON encoder.
// One query, including connection acquisition and waits, has a one-second deadline.
func (s *Service) InterruptedCalls(ctx context.Context, w store.Worker, run uuid.UUID, nonce string, generation int64) ([]byte, error) {
	if run == uuid.Nil || nonce == "" || len(nonce) > 128 || !utf8.ValidString(nonce) || generation < 2 {
		return nil, ErrInvalidState
	}
	ctx, cancel := context.WithTimeout(ctx, interruptedCallsTimeout)
	defer cancel()
	q, ok := s.q.(interruptedCallsReader)
	if !ok {
		return nil, ErrRunNotOwned
	}
	rows, err := q.InterruptedCallsWindow(ctx, store.InterruptedCallsWindowParams{RunID: run, WorkerID: w.ID, OwnerID: w.UserID, RegisterNonce: nonce, ClaimGeneration: generation})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrRunNotOwned
	}
	return interruptedCallsJSON(run, w.ID, nonce, generation, rows)
}

func interruptedKey(s string) bool {
	if s == "" || len(s) > 1024 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	return true
}

func interruptedDisplay(s string) (string, bool) {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > 128 {
		return "[omitted]", true
	}
	// Normalize single-line metadata before scrubbing the complete string.
	s = ScrubUntrustedText(sanitizeMemoryField(s, false))
	if s == "" || utf8.RuneCountInString(s) > 128 {
		return "[omitted]", true
	}
	return s, false
}

func interruptedCallsJSON(run, worker uuid.UUID, nonce string, generation int64, rows []store.InterruptedCallsWindowRow) ([]byte, error) {
	out := apitypes.InterruptedCallsResponse{Format: 1, RunID: run.String(), WorkerID: worker.String(), RegisterNonce: nonce, ClaimGeneration: generation, HistoryIncomplete: true, Entries: []apitypes.InterruptedCall{}}
	if len(rows) > 0 {
		out.InspectedCount = rows[0].InspectedCount
		if rows[0].SourceGeneration > 0 {
			p := rows[0].SourceGeneration
			out.SourceGeneration = &p
		}
	}
	type key struct {
		id, lane string
		null     bool
	}
	type group struct {
		uses, results []store.InterruptedCallsWindowRow
		agent         string
		ambiguous     bool
	}
	groups := map[key]*group{}
	// At most 256 independent rows; a malformed row does not block siblings.
	for _, r := range rows {
		if !r.Seq.Valid {
			continue
		}
		if !interruptedKey(r.CallID) || (!r.NullLane && (!interruptedKey(r.AgentInstance) || utf8.RuneCountInString(r.AgentInstance) >= 128)) || (r.NullLane && !r.ExplicitLead) {
			out.Unavailable = true
			continue
		}
		k := key{r.CallID, r.AgentInstance, r.NullLane}
		g := groups[k]
		if g == nil {
			g = &group{agent: r.Agent}
			groups[k] = g
		}
		// Attribution may reject a persisted key, but never changes its identity.
		g.ambiguous = g.ambiguous || r.AgentOmitted || g.agent != r.Agent
		if r.Kind.String == "tool_use" {
			g.uses = append(g.uses, r)
		} else {
			g.results = append(g.results, r)
		}
	}
	for _, g := range groups {
		if g.ambiguous || len(g.uses) != 1 || len(g.results) > 1 {
			out.Unavailable = true
			continue
		}
		r := g.uses[0]
		if len(g.results) == 1 {
			if g.results[0].Seq.Int32 <= r.Seq.Int32 {
				out.Unavailable = true
			}
			continue
		}
		if r.ToolName != "Bash" || r.Command == "" || r.CommandOmitted {
			out.Unavailable = true
			out.Truncated = out.Truncated || r.CommandOmitted
			continue
		}
		command := ScrubUntrustedText(r.Command)
		if !utf8.ValidString(command) || len(command) > 2048 || utf8.RuneCountInString(command) > 2048 || strings.TrimSpace(command) == "" {
			out.Unavailable = true
			out.Truncated = true
			continue
		}
		agent, omitted := interruptedDisplay(r.Agent)
		name, nameOmitted := interruptedDisplay(r.ToolName)
		entry := apitypes.InterruptedCall{Seq: r.Seq.Int32, SourceGeneration: r.SourceGeneration, Agent: agent, ToolName: name, Command: command, DisplayOmitted: omitted || nameOmitted || r.AgentLabelOmitted}
		if !r.NullLane {
			// Pair on the full key above; sanitize a separate display value.
			v, missing := interruptedDisplay(r.AgentInstance)
			entry.AgentInstance = &v
			entry.DisplayOmitted = entry.DisplayOmitted || missing
		}
		if r.AgentLabel != "" {
			v, missing := interruptedDisplay(r.AgentLabel)
			entry.AgentLabel = &v
			entry.DisplayOmitted = entry.DisplayOmitted || missing
		}
		out.Entries = append(out.Entries, entry)
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Seq > out.Entries[j].Seq })
	if len(out.Entries) > 3 {
		out.Entries = out.Entries[:3]
		out.Truncated = true
	}
	// Each pass removes one whole oldest entry, bounded by three attempts.
	for {
		raw, err := json.Marshal(out)
		if err != nil {
			return nil, err
		}
		// The standard HTTP JSON encoder appends one newline.
		if len(raw)+1 <= 16*1024 {
			return raw, nil
		}
		if len(out.Entries) == 0 {
			return nil, ErrInvalidState
		}
		out.Entries = out.Entries[:len(out.Entries)-1]
		out.Truncated = true
	}
}
