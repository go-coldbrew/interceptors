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
		hystrixOptions := make([]grpc.CallOption, 0)
		for _, opt := range defaultOpts {
			if opt == nil {
				continue
			}
			if o, ok := opt.(grpc.CallOption); ok {
				hystrixOptions = append(hystrixOptions, o)
			}
		}
		ints = append(ints,
			HystrixClientInterceptor(hystrixOptions...),
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

// HystrixClientInterceptor returns a unary client interceptor that executes the RPC inside a Hystrix command.
//
// Note: This interceptor wraps github.com/afex/hystrix-go which has been unmaintained since 2018.
// Consider migrating to github.com/failsafe-go/failsafe-go for circuit breaker functionality.
//
// The interceptor applies provided default and per-call client options to configure Hystrix behavior (for example the command name, disabled flag, excluded errors, and excluded gRPC status codes).
// If Hystrix is disabled via options, the RPC is invoked directly. If the underlying RPC returns an error that matches any configured excluded error or whose gRPC status code matches any configured excluded code, Hystrix fallback is skipped and the RPC error is returned.
// Panics raised during the RPC invocation are captured and reported to the notifier before being converted into an error. If the RPC itself returns an error, that error is returned; otherwise any error produced by Hystrix is returned.
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
