package forge

import (
	"context"
	"errors"
	"sync"
)

// AuthorEligibility is repository-specific author eligibility.
// Unknown always accompanies an error; neither zero values nor errors authorize.
type AuthorEligibility uint8

const (
	AuthorUnknown AuthorEligibility = iota
	AuthorEligible
	AuthorNotEligible
)

var ErrAuthorUnknown = errors.New("repository author eligibility unknown")

type assessmentKey struct{}
type evidenceKey struct {
	driver  any
	project int64
	kind    string
}
type evidenceResult struct {
	value any
	err   error
}
type authorEvidence struct {
	ctx     context.Context
	mu      sync.Mutex
	entries map[evidenceKey]evidenceResult
}

// BeginAuthorAssessment scopes driver evidence caches to one assessment. The
// caller supplies its deadline and must never reuse this context across operations.
// It does not impose a decoded-memory bound or change SDK retries.
func BeginAuthorAssessment(ctx context.Context) context.Context {
	return context.WithValue(ctx, assessmentKey{}, &authorEvidence{ctx: ctx, entries: make(map[evidenceKey]evidenceResult)})
}

// assessmentEvidence serializes repository evidence reads within an operation.
// Each read runs once; a failed read does not discard completed independent reads.
func assessmentEvidence[T any](ctx context.Context, key evidenceKey, fetch func(context.Context) (T, error)) (T, error) {
	state, _ := ctx.Value(assessmentKey{}).(*authorEvidence)
	if state == nil {
		return fetch(ctx)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if v, ok := state.entries[key]; ok {
		return v.value.(T), v.err
	}
	v, err := fetch(state.ctx)
	state.entries[key] = evidenceResult{v, err}
	return v, err
}
