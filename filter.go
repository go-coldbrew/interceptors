package interceptors

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
)

// If it returns false, the given request will not be traced.
type FilterFunc func(ctx context.Context, fullMethodName string) bool

var (
	// Deprecated: FilterMethods is the list of methods that are filtered by default.
	// Use SetFilterMethods instead. Only some direct mutations (replacing the slice
	// or changing the first element) are detected by internal change detection;
	// other in-place changes may not invalidate caches correctly.
	FilterMethods = []string{"healthcheck", "readycheck", "serverreflectioninfo"}
)

// filterState holds pre-computed filter data and a per-instance cache.
// A new filterState is created whenever FilterMethods changes, which
// atomically invalidates the old cache.
type filterState struct {
	methods     []string // lowercased filter substrings
	cache       sync.Map // map[string]bool
	sourceLen   int      // len(FilterMethods) when this state was built
	sourceFirst string   // FilterMethods[0] when built (fast mutation check)
}

var currentFilter atomic.Pointer[filterState]

func init() {
	currentFilter.Store(buildFilterState())
}

func buildFilterState() *filterState {
	lower := make([]string, len(FilterMethods))
	for i, m := range FilterMethods {
		lower[i] = strings.ToLower(m)
	}
	s := &filterState{
		methods:   lower,
		sourceLen: len(FilterMethods),
	}
	if len(FilterMethods) > 0 {
		s.sourceFirst = FilterMethods[0]
	}
	return s
}

// changed reports whether the deprecated FilterMethods variable
// has been mutated since this filterState was built.
func (s *filterState) changed() bool {
	if len(FilterMethods) != s.sourceLen {
		return true
	}
	if s.sourceLen > 0 && FilterMethods[0] != s.sourceFirst {
		return true
	}
	return false
}

// SetFilterMethods sets the list of method substrings to exclude from tracing/logging.
// It rebuilds the internal cache. Must be called during initialization, before
// the server starts. Not safe for concurrent use.
func SetFilterMethods(ctx context.Context, methods []string) {
	// Defensive copy to prevent aliasing: if the caller later mutates
	// their slice, it won't silently affect filtering.
	cp := make([]string, len(methods))
	copy(cp, methods)
	FilterMethods = cp
	currentFilter.Store(buildFilterState())
}

// isGRPCRequest returns true if the context is a gRPC server context.
// Uses grpc.Method(ctx) which is a single context value lookup with zero
// allocations. HTTP handlers pass plain contexts where this returns false.
// This is used to decide whether to cache filter decisions — gRPC method
// names are stable and finite, while HTTP paths can be high-cardinality.
func isGRPCRequest(ctx context.Context) bool {
	_, ok := grpc.Method(ctx)
	return ok
}

// FilterMethodsFunc is the default implementation of Filter function
func FilterMethodsFunc(ctx context.Context, fullMethodName string) bool {
	f := currentFilter.Load()
	// Auto-detect direct mutation of the deprecated FilterMethods variable.
	if f.changed() {
		f = buildFilterState()
		currentFilter.Store(f)
	}
	cacheable := isGRPCRequest(ctx)
	if cacheable {
		if v, ok := f.cache.Load(fullMethodName); ok {
			return v.(bool)
		}
	}
	lowerMethod := strings.ToLower(fullMethodName)
	result := true
	for _, name := range f.methods {
		if strings.Contains(lowerMethod, name) {
			result = false
			break
		}
	}
	if cacheable {
		f.cache.Store(fullMethodName, result)
	}
	return result
}
