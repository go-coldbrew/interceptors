package interceptors

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// Executor wraps an RPC invocation with resilience logic (circuit breaking,
// retries, bulkheading, etc.). The method parameter is the full gRPC method
// name (e.g., "/package.Service/Method"), enabling per-method state such as
// per-method circuit breakers. The fn parameter performs the actual RPC; the
// executor must call it exactly once.
type Executor func(ctx context.Context, method string, fn func(ctx context.Context) error) error

type clientOption interface {
	grpc.CallOption
	process(*clientOptions)
}

type clientOptions struct {
	hystrixName    string
	disableHystrix bool
	executor       Executor
	hasExecutor    bool
	excludedErrors []error
	excludedCodes  []codes.Code
}

type optionCarrier struct {
	grpc.EmptyCallOption
	processor func(*clientOptions)
}

func (h *optionCarrier) process(co *clientOptions) {
	h.processor(co)
}

// Deprecated: WithHystrixName sets the Hystrix command name. Use [WithExecutor] with a
// per-method executor instead. Will be removed in v1.
func WithHystrixName(name string) clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			if name != "" {
				co.hystrixName = name
			}
		},
	}
}

// Deprecated: WithoutHystrix disables hystrix and the executor.
// Use [WithoutExecutor] instead. Will be removed in v1.
func WithoutHystrix() clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.disableHystrix = true
			co.hasExecutor = true
			co.executor = nil
		},
	}
}

// Deprecated: WithHystrix enables hystrix. The executor is enabled by default
// when configured via [SetDefaultExecutor]. Will be removed in v1.
func WithHystrix() clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.disableHystrix = false
		},
	}
}

// Deprecated: WithHystrixExcludedErrors adds errors excluded from the Hystrix circuit breaker.
// Use [WithExcludedErrors] instead. Will be removed in v1.
//
//go:fix inline
func WithHystrixExcludedErrors(errors ...error) clientOption {
	return WithExcludedErrors(errors...)
}

// Deprecated: WithHystrixExcludedCodes appends gRPC codes excluded from the Hystrix circuit breaker.
// Use [WithExcludedCodes] instead. Will be removed in v1.
//
//go:fix inline
func WithHystrixExcludedCodes(codes ...codes.Code) clientOption {
	return WithExcludedCodes(codes...)
}

// WithExecutor returns a clientOption that sets a custom [Executor] for resilience
// logic (circuit breaking, retries, etc.). The executor wraps the RPC invocation.
// This overrides the global executor set via [SetDefaultExecutor] for this call.
func WithExecutor(e Executor) clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.executor = e
			co.hasExecutor = true
		},
	}
}

// WithoutExecutor returns a clientOption that disables the executor for this call,
// even if a global executor is set via [SetDefaultExecutor]. The RPC is invoked
// directly as a passthrough.
func WithoutExecutor() clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.executor = nil
			co.hasExecutor = true
		},
	}
}

// WithExcludedErrors returns a clientOption that adds the provided errors to the
// exclusion list. Excluded errors are reported as success to the executor (not
// tripping circuit breakers), but the original error is still returned to the caller.
func WithExcludedErrors(errors ...error) clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.excludedErrors = append(co.excludedErrors, errors...)
		},
	}
}

// WithExcludedCodes returns a clientOption that appends the provided gRPC status
// codes to the exclusion list. Excluded codes are reported as success to the
// executor (not tripping circuit breakers), but the original error is still
// returned to the caller.
func WithExcludedCodes(codes ...codes.Code) clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.excludedCodes = append(co.excludedCodes, codes...)
		},
	}
}
