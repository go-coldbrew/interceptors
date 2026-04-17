package interceptors

import (
	"context"
	stdError "errors"
	"fmt"
	"slices"

	"github.com/afex/hystrix-go/hystrix"
	"github.com/go-coldbrew/errors"
	"github.com/go-coldbrew/errors/notifier"
	"github.com/go-coldbrew/log"
	nrutil "github.com/go-coldbrew/tracing/newrelic"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	"github.com/newrelic/go-agent/v3/integrations/nrgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// DefaultClientInterceptors are the set of default interceptors that should be applied to all client calls
func DefaultClientInterceptors(defaultOpts ...any) []grpc.UnaryClientInterceptor {
	ints := []grpc.UnaryClientInterceptor{}
	if len(defaultConfig.unaryClientInterceptors) > 0 {
		ints = append(ints, defaultConfig.unaryClientInterceptors...)
	}
	if defaultConfig.useCBClientInterceptors {
		callOptions := make([]grpc.CallOption, 0)
		for _, opt := range defaultOpts {
			if opt == nil {
				continue
			}
			if o, ok := opt.(grpc.CallOption); ok {
				callOptions = append(callOptions, o)
			}
		}
		if defaultConfig.defaultExecutor != nil {
			ints = append(ints, ExecutorClientInterceptor(callOptions...))
		} else {
			ints = append(ints, HystrixClientInterceptor(callOptions...))
		}
		ints = append(ints,
			grpc_retry.UnaryClientInterceptor(),
			NewRelicClientInterceptor(),
			getClientMetrics().UnaryClientInterceptor(),
		)
	}
	return ints
}

// DefaultClientStreamInterceptors are the set of default interceptors that should be applied to all stream client calls
func DefaultClientStreamInterceptors(defaultOpts ...any) []grpc.StreamClientInterceptor {
	ints := []grpc.StreamClientInterceptor{}
	if len(defaultConfig.streamClientInterceptors) > 0 {
		ints = append(ints, defaultConfig.streamClientInterceptors...)
	}
	if defaultConfig.useCBClientInterceptors {
		if nrutil.GetNewRelicApp() != nil {
			ints = append(ints, nrgrpc.StreamClientInterceptor)
		}
		ints = append(ints, getClientMetrics().StreamClientInterceptor())
	}
	return ints
}

// DefaultClientInterceptor are the set of default interceptors that should be applied to all client calls
func DefaultClientInterceptor(defaultOpts ...any) grpc.UnaryClientInterceptor {
	return chainUnaryClient(DefaultClientInterceptors(defaultOpts...))
}

// DefaultClientStreamInterceptor are the set of default interceptors that should be applied to all stream client calls
func DefaultClientStreamInterceptor(defaultOpts ...any) grpc.StreamClientInterceptor {
	return chainStreamClient(DefaultClientStreamInterceptors(defaultOpts...))
}

// NewRelicClientInterceptor intercepts all client actions and reports them to newrelic.
// When NewRelic app is nil (no license key configured), returns a pass-through
// interceptor to avoid overhead.
func NewRelicClientInterceptor() grpc.UnaryClientInterceptor {
	app := nrutil.GetNewRelicApp()
	if app == nil {
		return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if defaultConfig.filterFunc(ctx, method) {
			return nrgrpc.UnaryClientInterceptor(ctx, method, req, reply, cc, invoker, opts...)
		} else {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}
}

// Deprecated: GRPCClientInterceptor is no longer needed. gRPC tracing is now handled
// by google.golang.org/grpc/stats/opentelemetry, configured via opentelemetry.DialOption()
// at the client level. This function is retained for backwards compatibility but returns
// a no-op interceptor.
func GRPCClientInterceptor(_ ...any) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// ExecutorClientInterceptor returns a unary client interceptor that wraps each
// RPC in an [Executor]. The executor provides resilience logic such as circuit
// breaking, retries, or bulkheading.
//
// If no executor is configured (neither via [SetDefaultExecutor] nor per-call
// [WithExecutor]), the RPC is invoked directly as a passthrough.
//
// Excluded errors and codes (set via [WithExcludedErrors] / [WithExcludedCodes])
// are reported as nil to the executor, preventing them from tripping circuit
// breakers or retry logic. The original error is still returned to the caller.
func ExecutorClientInterceptor(defaultOpts ...grpc.CallOption) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		o := clientOptions{}
		for _, opt := range defaultOpts {
			if opt != nil {
				if co, ok := opt.(clientOption); ok {
					co.process(&o)
				}
			}
		}
		for _, opt := range opts {
			if opt != nil {
				if co, ok := opt.(clientOption); ok {
					co.process(&o)
				}
			}
		}

		// Resolve executor: per-call > global > nil (passthrough)
		exec := defaultConfig.defaultExecutor
		if o.hasExecutor {
			exec = o.executor
		}
		if exec == nil {
			return invoker(ctx, method, req, reply, cc, opts...)
		}

		var invokerErr error
		executorErr := exec(ctx, method, func(execCtx context.Context) (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = errors.Wrap(fmt.Errorf("panic in executor method: %s, req: %v, reply: %v", method, req, reply), "Executor")
					log.Error(execCtx, "panic", r, "method", method, "req", req, "reply", reply)
				}
			}()
			defer notifier.NotifyOnPanic(execCtx, method)
			invokerErr = invoker(execCtx, method, req, reply, cc, opts...)
			for _, excludedErr := range o.excludedErrors {
				if stdError.Is(invokerErr, excludedErr) {
					return nil
				}
			}
			if st, ok := status.FromError(invokerErr); ok {
				if slices.Contains(o.excludedCodes, st.Code()) {
					return nil
				}
			}
			return invokerErr
		})
		if invokerErr != nil {
			return invokerErr
		}
		return executorErr
	}
}

// Deprecated: HystrixClientInterceptor wraps the unmaintained hystrix-go library.
// Use [SetDefaultExecutor] with a failsafe-go executor instead. Will be removed in v1.
//
// See [ExecutorClientInterceptor] for the replacement.
func HystrixClientInterceptor(defaultOpts ...grpc.CallOption) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		options := clientOptions{
			hystrixName: method,
		}
		for _, opt := range defaultOpts {
			if opt != nil {
				if o, ok := opt.(clientOption); ok {
					o.process(&options)
				}
			}
		}
		for _, opt := range opts {
			if opt != nil {
				if o, ok := opt.(clientOption); ok {
					o.process(&options)
				}
			}
		}
		if options.disableHystrix {
			// short circuit if hystrix is disabled
			return invoker(ctx, method, req, reply, cc, opts...)
		}
		newCtx, cancel := context.WithCancel(ctx)
		defer cancel()

		var invokerErr error
		hystrixErr := hystrix.Do(options.hystrixName, func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = errors.Wrap(fmt.Errorf("panic inside hystrix method: %s, req: %v, reply: %v", method, req, reply), "Hystrix")
					log.Error(ctx, "panic", r, "method", method, "req", req, "reply", reply)
				}
			}()
			defer notifier.NotifyOnPanic(newCtx, method)
			invokerErr = invoker(newCtx, method, req, reply, cc, opts...)
			for _, excludedErr := range options.excludedErrors {
				if stdError.Is(invokerErr, excludedErr) {
					return nil
				}
			}
			if st, ok := status.FromError(invokerErr); ok {
				if slices.Contains(options.excludedCodes, st.Code()) {
					return nil
				}
			}
			return invokerErr
		}, nil)
		if invokerErr != nil {
			return invokerErr
		}
		return hystrixErr
	}
}
