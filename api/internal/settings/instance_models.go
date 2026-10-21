package settings

// This file holds the instance model-setting accessor and the trim-at-storage
// normalizer (PRD #3245). A model key's stored row may be "" (an explicit reset
// to the built-in), so the accessor reports the raw tier value and leaves the
// built-in fallback to its caller.

import (
	"context"
	"fmt"
	"strings"
)

// instanceModelKeys is the set of instance model keys whose values are trimmed
// at storage and read through InstanceModels. Later model keys join this set.
var instanceModelKeys = map[string]struct{}{
	KeyJudgeModel: {},
}

// NormalizeForStorage returns the value to validate and store for key: the
// instance model keys are trimmed so a whitespace-only submission stores
// exactly "" (which reads back as the default). Every other key is returned
// verbatim.
func NormalizeForStorage(key, value string) string {
	if _, ok := instanceModelKeys[key]; ok {
		return strings.TrimSpace(value)
	}
	return value
}

// InstanceModels returns, for each requested non-secret key, the instance tier
// value from one cache snapshot: the ENV override over the stored row, trimmed,
// and "" when neither is set. The compiled-in default is NOT substituted, so the
// caller can tell an unset instance tier from a pinned one. A secret key in the
// request returns an error and no values.
func (c *Cache) InstanceModels(ctx context.Context, keys []string) (map[string]string, error) {
	for _, k := range keys {
		if IsSecret(k) {
			return nil, fmt.Errorf("settings: %s is a secret key and has no instance model value", k)
		}
	}
	m, err := c.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := c.env[k]; ok && v != "" {
			out[k] = strings.TrimSpace(v)
			continue
		}
		out[k] = strings.TrimSpace(m[k])
	}
	return out, nil
}

// InstanceModel is the one-key form of InstanceModels, with the same rules.
func (c *Cache) InstanceModel(ctx context.Context, key string) (string, error) {
	vals, err := c.InstanceModels(ctx, []string{key})
	if err != nil {
		return "", err
	}
	return vals[key], nil
}
