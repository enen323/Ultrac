package ai

import (
	"fmt"
	"sort"
	"sync"
)

// ApiProvider is one provider adapter registered under an Api dialect.
//
// pi: packages/ai/src/compat.ts:85-100 (ApiProvider / registerApiProvider).
// The registry is keyed by Api, not by provider id: one adapter (e.g.
// anthropic-messages) serves many providers (anthropic, deepseek's
// anthropic-compatible endpoint, bedrock, …), exactly as in pi.
type ApiProvider struct {
	API    Api
	Stream StreamFn
}

var (
	registryMu sync.RWMutex
	registry   = make(map[Api]StreamFn)
)

// Register adds an adapter for its Api. Call it from a provider package's
// init(), so the only wiring a host needs is a blank import — the same
// init()-registration pattern the old internal/contract/provider used and
// database/sql popularized.
//
// It panics on a duplicate Api or a nil Stream: both are programming errors
// that would otherwise surface much later as a confusing runtime failure.
func Register(p ApiProvider) {
	if p.API == "" {
		panic("ai: Register called with an empty Api")
	}
	if p.Stream == nil {
		panic(fmt.Sprintf("ai: Register called with a nil Stream for api %q", p.API))
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[p.API]; dup {
		panic(fmt.Sprintf("ai: duplicate registration for api %q", p.API))
	}
	registry[p.API] = p.Stream
}

func lookup(api Api) (StreamFn, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	fn, ok := registry[api]
	return fn, ok
}

// Registered reports whether an adapter exists for api.
func Registered(api Api) bool {
	_, ok := lookup(api)
	return ok
}

// Apis lists the registered Api dialects in lexical order.
func Apis() []Api {
	registryMu.RLock()
	defer registryMu.RUnlock()
	apis := make([]Api, 0, len(registry))
	for a := range registry {
		apis = append(apis, a)
	}
	sort.Slice(apis, func(i, j int) bool { return apis[i] < apis[j] })
	return apis
}
