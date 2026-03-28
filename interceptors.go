// Package interceptors provides gRPC server and client interceptors for the ColdBrew framework.
//
// Interceptor configuration functions (AddUnaryServerInterceptor, SetFilterFunc, etc.)
// must be called during program initialization, before the gRPC server starts.
// They are not safe for concurrent use.
package interceptors

import (
	"context"
	stdError "errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/afex/hystrix-go/hystrix"
	"github.com/go-coldbrew/errors"
	"github.com/go-coldbrew/errors/notifier"
	"github.com/go-coldbrew/log"
	"github.com/go-coldbrew/log/loggers"
	"github.com/go-coldbrew/options"
	nrutil "github.com/go-coldbrew/tracing/newrelic"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/newrelic/go-agent/v3/integrations/nrgrpc"
	newrelic "github.com/newrelic/go-agent/v3/newrelic"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// SupportPackageIsVersion1 is a compile-time assertion constant.
// Downstream packages (e.g. core) reference this constant to enforce
// version compatibility. When interceptors makes a breaking change,
// export a new constant and remove this one to force coordinated updates.
const SupportPackageIsVersion1 = true

// Compile-time version compatibility check.
var _ = errors.SupportPackageIsVersion1

var (
	//FilterMethods is the list of methods that are filtered by default
	FilterMethods            = []string{"healthcheck", "readycheck", "serverreflectioninfo"}
	defaultFilterFunc        = FilterMethodsFunc
	unaryServerInterceptors  = []grpc.UnaryServerInterceptor{}
	streamServerInterceptors = []grpc.StreamServerInterceptor{}
	useCBServerInterceptors  = true
	unaryClientInterceptors  = []grpc.UnaryClientInterceptor{}
	streamClientInterceptors = []grpc.StreamClientInterceptor{}
	useCBClientInterceptors  = true
	responseTimeLogLevel     loggers.Level = loggers.InfoLevel
	srvMetricsOpts           []grpcprom.ServerMetricsOption
	cltMetricsOpts           []grpcprom.ClientMetricsOption
	srvMetricsOnce           sync.Once
	srvMetrics               *grpcprom.ServerMetrics
	cltMetricsOnce           sync.Once
	cltMetrics               *grpcprom.ClientMetrics
)

// SetResponseTimeLogLevel sets the log level for response time logging.
// Default is InfoLevel. Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetResponseTimeLogLevel(ctx context.Context, level loggers.Level) {
	responseTimeLogLevel = level
}

// If it returns false, the given request will not be traced.
type FilterFunc func(ctx context.Context, fullMethodName string) bool

// FilterMethodsFunc is the default implementation of Filter function
func FilterMethodsFunc(ctx context.Context, fullMethodName string) bool {
	lowerMethod := strings.ToLower(fullMethodName)
	for _, name := range FilterMethods {
		if strings.Contains(lowerMethod, name) {
			return false
		}
	}
	return true
}

// SetFilterFunc sets the default filter function to be used by interceptors.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetFilterFunc(ctx context.Context, ff FilterFunc) {
	if ff != nil {
		defaultFilterFunc = ff
	}
}

// AddUnaryServerInterceptor adds a server interceptor to default server interceptors.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func AddUnaryServerInterceptor(ctx context.Context, i ...grpc.UnaryServerInterceptor) {
	unaryServerInterceptors = append(unaryServerInterceptors, i...)
}

// AddStreamServerInterceptor adds a server interceptor to default server interceptors.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func AddStreamServerInterceptor(ctx context.Context, i ...grpc.StreamServerInterceptor) {
	streamServerInterceptors = append(streamServerInterceptors, i...)
}

// UseColdBrewServerInterceptors allows enabling/disabling coldbrew server interceptors.
// When set to false, the coldbrew server interceptors will not be used.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func UseColdBrewServerInterceptors(ctx context.Context, flag bool) {
	useCBServerInterceptors = flag
}

// AddUnaryClientInterceptor adds a client interceptor to default client interceptors.
// Must be called during initialization, before any RPCs are made. Not safe for concurrent use.
func AddUnaryClientInterceptor(ctx context.Context, i ...grpc.UnaryClientInterceptor) {
	unaryClientInterceptors = append(unaryClientInterceptors, i...)
}

// AddStreamClientInterceptor adds a client stream interceptor to default client stream interceptors.
// Must be called during initialization, before any RPCs are made. Not safe for concurrent use.
func AddStreamClientInterceptor(ctx context.Context, i ...grpc.StreamClientInterceptor) {
	streamClientInterceptors = append(streamClientInterceptors, i...)
}

// UseColdBrewClientInterceptors allows enabling/disabling coldbrew client interceptors.
// When set to false, the coldbrew client interceptors will not be used.
// Must be called during initialization, before any RPCs are made. Not safe for concurrent use.
func UseColdBrewClientInterceptors(ctx context.Context, flag bool) {
	useCBClientInterceptors = flag
}

// SetServerMetricsOptions appends gRPC server metrics options (histogram, labels, namespace, etc.).
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetServerMetricsOptions(opts ...grpcprom.ServerMetricsOption) {
	srvMetricsOpts = append(srvMetricsOpts, opts...)
}

// SetClientMetricsOptions appends gRPC client metrics options.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetClientMetricsOptions(opts ...grpcprom.ClientMetricsOption) {
	cltMetricsOpts = append(cltMetricsOpts, opts...)
}

func registerCollector(c prometheus.Collector) {
	if err := prometheus.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if stdError.As(err, &are) {
			prometheus.Unregister(are.ExistingCollector)
			if err := prometheus.Register(c); err != nil {
				log.Warn(context.Background(), "msg", "failed to re-register gRPC metrics with Prometheus", "err", err)
			}
			return
		}
		log.Error(context.Background(), "msg", "gRPC Prometheus metrics registration failed. If you are using github.com/go-coldbrew/core, it may need to be updated to the latest version.", "err", err)
	}
}

func getServerMetrics() *grpcprom.ServerMetrics {
	srvMetricsOnce.Do(func() {
		srvMetrics = grpcprom.NewServerMetrics(srvMetricsOpts...)
		registerCollector(srvMetrics)
	})
	return srvMetrics
}

func getClientMetrics() *grpcprom.ClientMetrics {
	cltMetricsOnce.Do(func() {
		cltMetrics = grpcprom.NewClientMetrics(cltMetricsOpts...)
		registerCollector(cltMetrics)
	})
	return cltMetrics
}

// chainUnaryServer chains multiple unary server interceptors into one.
func chainUnaryServer(interceptors []grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		chain := handler
		for i := len(interceptors) - 1; i >= 0; i-- {
			interceptor := interceptors[i]
			next := chain
			chain = func(ctx context.Context, req interface{}) (interface{}, error) {
				return interceptor(ctx, req, info, next)
			}
		}
		return chain(ctx, req)
	}
}

// chainUnaryClient chains multiple unary client interceptors into one.
func chainUnaryClient(interceptors []grpc.UnaryClientInterceptor) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		chain := invoker
		for i := len(interceptors) - 1; i >= 0; i-- {
			interceptor := interceptors[i]
			next := chain
			chain = func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
				return interceptor(ctx, method, req, reply, cc, next, opts...)
			}
		}
		return chain(ctx, method, req, reply, cc, opts...)
	}
}

// chainStreamClient chains multiple stream client interceptors into one.
func chainStreamClient(interceptors []grpc.StreamClientInterceptor) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		chain := streamer
		for i := len(interceptors) - 1; i >= 0; i-- {
			interceptor := interceptors[i]
			next := chain
			chain = func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
				return interceptor(ctx, desc, cc, method, next, opts...)
			}
		}
		return chain(ctx, desc, cc, method, opts...)
	}
}

// DoHTTPtoGRPC allows calling the interceptors when you use the Register<svc-name>HandlerServer in grpc-gateway.
// The interceptor chain is cached on first invocation. All interceptor configuration
// (AddUnaryServerInterceptor, SetFilterFunc, etc.) must be finalized before the first call.
// See example below for reference
//
//	func (s *svc) Echo(ctx context.Context, req *proto.EchoRequest) (*proto.EchoResponse, error) {
//	    handler := func(ctx context.Context, req interface{}) (interface{}, error) {
//	        return s.echo(ctx, req.(*proto.EchoRequest))
//	    }
//	    r, e := doHTTPtoGRPC(ctx, s, handler, req)
//	    if e != nil {
//	        return nil, e.(error)
//	    }
//	    return r.(*proto.EchoResponse), nil
//	}
//
//	func (s *svc) echo(ctx context.Context, req *proto.EchoRequest) (*proto.EchoResponse, error) {
//	       .... implementation ....
//	}
var (
	httpToGRPCOnce        sync.Once
	httpToGRPCInterceptor grpc.UnaryServerInterceptor
)

func getHTTPtoGRPCInterceptor() grpc.UnaryServerInterceptor {
	httpToGRPCOnce.Do(func() {
		httpToGRPCInterceptor = chainUnaryServer(DefaultInterceptors())
	})
	return httpToGRPCInterceptor
}

func DoHTTPtoGRPC(ctx context.Context, svr interface{}, handler func(ctx context.Context, req interface{}) (interface{}, error), in interface{}) (interface{}, error) {
	method, ok := runtime.RPCMethod(ctx)
	if ok {
		interceptor := getHTTPtoGRPCInterceptor()
		info := &grpc.UnaryServerInfo{
			Server:     svr,
			FullMethod: method,
		}
		return interceptor(ctx, in, info, handler)
	}
	return handler(ctx, in)
}

// DefaultInterceptors are the set of default interceptors that are applied to all coldbrew methods
func DefaultInterceptors() []grpc.UnaryServerInterceptor {
	ints := []grpc.UnaryServerInterceptor{}
	if len(unaryServerInterceptors) > 0 {
		ints = append(ints, unaryServerInterceptors...)
	}
	if useCBServerInterceptors {
		ints = append(ints,
			ResponseTimeLoggingInterceptor(defaultFilterFunc),
			TraceIdInterceptor(),
			getServerMetrics().UnaryServerInterceptor(),
			ServerErrorInterceptor(),
			NewRelicInterceptor(),
			PanicRecoveryInterceptor(),
		)
	}
	return ints
}

// DefaultClientInterceptors are the set of default interceptors that should be applied to all client calls
func DefaultClientInterceptors(defaultOpts ...interface{}) []grpc.UnaryClientInterceptor {
	ints := []grpc.UnaryClientInterceptor{}
	if len(unaryClientInterceptors) > 0 {
		ints = append(ints, unaryClientInterceptors...)
	}
	if useCBClientInterceptors {
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
func DefaultClientStreamInterceptors(defaultOpts ...interface{}) []grpc.StreamClientInterceptor {
	ints := []grpc.StreamClientInterceptor{}
	if len(streamClientInterceptors) > 0 {
		ints = append(ints, streamClientInterceptors...)
	}
	if useCBClientInterceptors {
		ints = append(ints,
			nrgrpc.StreamClientInterceptor,
			getClientMetrics().StreamClientInterceptor(),
		)
	}
	return ints
}

// DefaultStreamInterceptors are the set of default interceptors that should be applied to all coldbrew streams
func DefaultStreamInterceptors() []grpc.StreamServerInterceptor {
	ints := []grpc.StreamServerInterceptor{}
	if len(streamServerInterceptors) > 0 {
		ints = append(ints, streamServerInterceptors...)
	}
	if useCBServerInterceptors {
		ints = append(ints,
			ResponseTimeLoggingStreamInterceptor(),
			getServerMetrics().StreamServerInterceptor(),
			ServerErrorStreamInterceptor(),
		)
	}
	return ints
}

// DefaultClientInterceptor are the set of default interceptors that should be applied to all client calls
func DefaultClientInterceptor(defaultOpts ...interface{}) grpc.UnaryClientInterceptor {
	return chainUnaryClient(DefaultClientInterceptors(defaultOpts...))
}

// DefaultClientStreamInterceptor are the set of default interceptors that should be applied to all stream client calls
func DefaultClientStreamInterceptor(defaultOpts ...interface{}) grpc.StreamClientInterceptor {
	return chainStreamClient(DefaultClientStreamInterceptors(defaultOpts...))
}

// DebugLoggingInterceptor is the interceptor that logs all request/response from a handler
func DebugLoggingInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		log.Debug(ctx, "method", info.FullMethod, "request", req)
		resp, err := handler(ctx, req)
		log.Debug(ctx, "method", info.FullMethod, "response", resp, "err", err)
		return resp, err
	}
}

// ResponseTimeLoggingInterceptor logs response time for each request on server
func ResponseTimeLoggingInterceptor(ff FilterFunc) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		ctx = options.AddToOptions(ctx, "", "")
		ctx = loggers.AddToLogContext(ctx, "", "")
		ctx = loggers.AddToLogContext(ctx, "grpcMethod", info.FullMethod)
		defer func(ctx context.Context, method string, begin time.Time) {
			if ff != nil && !ff(ctx, method) {
				return
			}
			logArgs := []any{"error", err, "took", time.Since(begin)}
			if err != nil {
				logArgs = append(logArgs, "grpcCode", status.Code(err))
			}
			log.GetLogger().Log(ctx, responseTimeLogLevel, 1, logArgs...)
		}(ctx, info.FullMethod, time.Now())
		resp, err = handler(ctx, req)
		return resp, err
	}
}

func OptionsInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		ctx = options.AddToOptions(ctx, "", "")
		//loggers.AddToLogContext(ctx, "transport", "gRPC")
		return handler(ctx, req)
	}
}

// NewRelicInterceptor intercepts all server actions and reports them to newrelic
func NewRelicInterceptor() grpc.UnaryServerInterceptor {
	nrh := nrgrpc.UnaryServerInterceptor(nrutil.GetNewRelicApp())
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		if defaultFilterFunc(ctx, info.FullMethod) {
			return nrh(ctx, req, info, handler)
		} else {
			return handler(ctx, req)
		}
	}
}

// ServerErrorInterceptor intercepts all server actions and reports them to error notifier
func ServerErrorInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		// set trace id if not set
		ctx = notifier.SetTraceId(ctx)

		traceID := notifier.GetTraceId(ctx)
		ctx = loggers.AddToLogContext(ctx, "trace", traceID)
		start := time.Now()
		resp, err = handler(ctx, req)
		if err != nil && defaultFilterFunc(ctx, info.FullMethod) {
			_ = notifier.NotifyAsync(err, ctx, notifier.Tags{
				"grpcMethod": info.FullMethod,
				"duration":   time.Since(start).Truncate(time.Millisecond).String(),
			})
		}
		return resp, err
	}
}

func PanicRecoveryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func(ctx context.Context) {
			// panic handler
			if r := recover(); r != nil {
				stack := string(debug.Stack())
				log.Error(ctx, "panic", r, "method", info.FullMethod, "stack", stack)
				if e, ok := r.(error); ok {
					err = e
				} else {
					err = errors.New(fmt.Sprintf("panic: %s", r))
				}
				nrutil.FinishNRTransaction(ctx, err)
				_ = notifier.NotifyWithLevel(err, "critical", info.FullMethod, ctx, stack)
			}
		}(ctx)

		resp, err = handler(ctx, req)
		return resp, err
	}
}

// NewRelicClientInterceptor intercepts all client actions and reports them to newrelic
func NewRelicClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if defaultFilterFunc(ctx, method) {
			return nrgrpc.UnaryClientInterceptor(ctx, method, req, reply, cc, invoker, opts...)
		} else {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}
}

// Deprecated: GRPCClientInterceptor is no longer needed. gRPC tracing is now handled
// by otelgrpc.NewClientHandler stats handler configured at the client level.
// This function is retained for backwards compatibility but returns a no-op interceptor.
func GRPCClientInterceptor(_ ...interface{}) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
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
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
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
				for _, code := range options.excludedCodes {
					if st.Code() == code {
						return nil
					}
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

// ResponseTimeLoggingStreamInterceptor logs response time for stream RPCs.
func ResponseTimeLoggingStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func(begin time.Time) {
			logArgs := []any{"method", info.FullMethod, "error", err, "took", time.Since(begin)}
			if err != nil {
				logArgs = append(logArgs, "grpcCode", status.Code(err))
			}
			log.GetLogger().Log(stream.Context(), responseTimeLogLevel, 1, logArgs...)
		}(time.Now())
		err = handler(srv, stream)
		return err
	}
}

// ServerErrorStreamInterceptor intercepts server errors for stream RPCs and
// reports them to the error notifier.
func ServerErrorStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		ctx := stream.Context()
		ctx = notifier.SetTraceId(ctx)
		traceID := notifier.GetTraceId(ctx)
		ctx = loggers.AddToLogContext(ctx, "trace", traceID)
		start := time.Now()
		err = handler(srv, stream)
		if err != nil && defaultFilterFunc(ctx, info.FullMethod) {
			_ = notifier.NotifyAsync(err, ctx, notifier.Tags{
				"grpcMethod": info.FullMethod,
				"duration":   time.Since(start).Truncate(time.Millisecond).String(),
			})
		}
		return err

	}
}

// NRHttpTracer adds newrelic tracing to this http function
func NRHttpTracer(pattern string, h http.HandlerFunc) (string, http.HandlerFunc) {
	app := nrutil.GetNewRelicApp()
	if app == nil {
		return pattern, h
	}
	if pattern != "" {
		return newrelic.WrapHandleFunc(app, pattern, h)
	}
	return pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// filter functions we do not need
		if defaultFilterFunc(context.Background(), r.URL.Path) {
			txn := app.StartTransaction(r.Method + " " + r.URL.Path)
			defer txn.End()
			w = txn.SetWebResponse(w)
			txn.SetWebRequestHTTP(r)
			r = newrelic.RequestWithTransactionContext(r, txn)
		}
		h.ServeHTTP(w, r)
	})
}

// TraceIdInterceptor allows injecting trace id from request objects
func TraceIdInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		if req != nil {
			// fetch and update trace id from request
			if r, ok := req.(interface{ GetTraceId() string }); ok {
				ctx = notifier.UpdateTraceId(ctx, r.GetTraceId())
			} else if r, ok := req.(interface{ GetTraceID() string }); ok {
				ctx = notifier.UpdateTraceId(ctx, r.GetTraceID())
			}
		}
		return handler(ctx, req)
	}
}