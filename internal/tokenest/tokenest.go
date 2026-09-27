// Package tokenest provides a provider/model tokenizer registry and a bounded
// conservative fallback estimator. Estimates are for quota reservation only;
// final billing always uses validated provider usage.
package tokenest

import (
	"errors"
	"strings"
	"sync"
)

// Estimator estimates tokens from a request representation.
type Estimator interface {
	Estimate(input []byte) (int, error)
}

// Registry resolves model-specific tokenizers with a conservative fallback.
type Registry struct {
	mu       sync.RWMutex
	byModel  map[string]Estimator
	fallback Estimator
}

// NewRegistry creates a registry. A nil fallback uses Conservative.
func NewRegistry(fallback Estimator) *Registry {
	if fallback == nil {
		fallback = Conservative{MaxBytes: 4 << 20}
	}
	return &Registry{byModel: map[string]Estimator{}, fallback: fallback}
}

// Register binds a provider model key to an estimator.
func (r *Registry) Register(modelKey string, estimator Estimator) {
	if modelKey == "" || estimator == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byModel[modelKey] = estimator
}

// Estimate uses a registered model tokenizer when present, otherwise fallback.
// The bool reports whether a model-specific tokenizer was used.
func (r *Registry) Estimate(modelKey string, input []byte) (tokens int, modelSpecific bool, err error) {
	r.mu.RLock()
	estimator := r.byModel[modelKey]
	r.mu.RUnlock()
	if estimator == nil {
		estimator = r.fallback
	} else {
		modelSpecific = true
	}
	tokens, err = estimator.Estimate(input)
	if err == nil && tokens < 0 {
		return 0, modelSpecific, errors.New("token estimator returned a negative count")
	}
	return tokens, modelSpecific, err
}

// Conservative estimates roughly 1 token per 3 weighted runes, counting
// non-ASCII runes as two. It rejects oversized input rather than overflowing.
type Conservative struct{ MaxBytes int }

func (c Conservative) Estimate(input []byte) (int, error) {
	max := c.MaxBytes
	if max <= 0 {
		max = 4 << 20
	}
	if len(input) > max {
		return 0, errors.New("token estimator input exceeds configured byte limit")
	}
	weighted := 0
	for _, r := range string(input) {
		if r < 128 {
			weighted++
		} else {
			weighted += 2
		}
	}
	if weighted == 0 {
		return 0, nil
	}
	return (weighted + 2) / 3, nil
}

var Default = NewRegistry(Conservative{MaxBytes: 4 << 20})

// JoinMessages creates a bounded role/content representation without logging
// or retaining the request body.
func JoinMessages(parts ...string) []byte { return []byte(strings.Join(parts, "\n")) }
