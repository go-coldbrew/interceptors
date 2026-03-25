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
	grpc_opentracing "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/newrelic/go-agent/v3/integrations/nrgrpc"
	newrelic "github.com/newrelic/go-agent/v3/newrelic"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

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

	srvMetrics         = grpcprom.NewServerMetrics()
	cltMetrics         = grpcprom.NewClientMetrics()
	srvInterceptorOpts []grpcprom.Option
	cltInterceptorOpts []grpcprom.Option
)

func init() {
	prometheus.MustRegister(srvMetrics)
	prometheus.MustRegister(cltMetrics)
}

// registerMetrics unregisters old metrics and registers new ones with the default Prometheus registerer.
func registerMetrics(old, replacement prometheus.Collector) {
	prometheus.Unregister(old)
	prometheus.MustRegister(replacement)
}

// EnablePrometheusHandlingTimeHistogram re-creates the server metrics with handling time histogram enabled.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func EnablePrometheusHandlingTimeHistogram(buckets []float64) {
	old := srvMetrics
	if len(buckets) > 0 {
		srvMetrics = grpcprom.NewServerMetrics(
			grpcprom.WithServerHandlingTimeHistogram(
				grpcprom.WithHistogramBuckets(buckets),
			),
		)
	} else {
		srvMetrics = grpcprom.NewServerMetrics(
			grpcprom.WithServerHandlingTimeHistogram(),
		)
	}
	registerMetrics(old, srvMetrics)
}

// SetServerInterceptorOptions sets options applied to server-side Prometheus gRPC interceptors
// (e.g. WithExemplarFromContext, WithLabelsFromContext).
// Must be called during initialization, before the server starts serving. Not safe for concurrent use.
func SetServerInterceptorOptions(opts ...grpcprom.Option) {
	srvInterceptorOpts = opts
}

// SetClientInterceptorOptions sets options applied to client-side Prometheus gRPC interceptors
// (e.g. WithExemplarFromContext).
// Note: WithLabelsFromContext is a no-op for client interceptors in the current provider version.
// Must be called during initialization, before the first client RPC. Not safe for concurrent use.
func SetClientInterceptorOptions(opts ...grpcprom.Option) {
	cltInterceptorOpts = opts
}

// SetServerMetrics sets custom server metrics for gRPC Prometheus instrumentation.
// The new metrics are automatically registered with the default Prometheus registerer.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetServerMetrics(m *grpcprom.ServerMetrics) {
	if m != nil {
		registerMetrics(srvMetrics, m)
		srvMetrics = m
	}
}

// SetClientMetrics sets custom client metrics for gRPC Prometheus instrumentation.
// The new metrics are automatically registered with the default Prometheus registerer.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetClientMetrics(m *grpcprom.ClientMetrics) {
	if m != nil {
		registerMetrics(cltMetrics, m)
		cltMetrics = m
	}
}

// GetServerMetrics returns the current server metrics instance.
func GetServerMetrics() *grpcprom.ServerMetrics {
	return srvMetrics
}

// GetClientMetrics returns the current client metrics instance.
func GetClientMetrics() *grpcprom.ClientMetrics {
	return cltMetrics
}

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
			grpc_opentracing.UnaryServerInterceptor(grpc_opentracing.WithFilterFunc(defaultFilterFunc)),
			srvMetrics.UnaryServerInterceptor(srvInterceptorOpts...),
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
		opentracingOpt := make([]grpc_opentracing.Option, 0)
		for _, opt := range defaultOpts {
			if opt == nil {
				continue
			}
			if o, ok := opt.(grpc.CallOption); ok {
				hystrixOptions = append(hystrixOptions, o)
			}
			if o, ok := opt.(grpc_opentracing.Option); ok {
				opentracingOpt = append(opentracingOpt, o)
			}
		}
		ints = append(ints,
			HystrixClientInterceptor(hystrixOptions...),
			grpc_retry.UnaryClientInterceptor(),
			GRPCClientInterceptor(opentracingOpt...),
			NewRelicClientInterceptor(),
			cltMetrics.UnaryClientInterceptor(cltInterceptorOpts...),
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
		opentracingOpt := make([]grpc_opentracing.Option, 0)
		for _, opt := range defaultOpts {
			if opt == nil {
				continue
			}
			if o, ok := opt.(grpc_opentracing.Option); ok {
				opentracingOpt = append(opentracingOpt, o)
			}
		}
		ints = append(ints,
			grpc_opentracing.StreamClientInterceptor(opentracingOpt...),
			nrgrpc.StreamClientInterceptor,
			cltMetrics.StreamClientInterceptor(cltInterceptorOpts...),
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
			grpc_opentracing.StreamServerInterceptor(),
			srvMetrics.StreamServerInterceptor(srvInterceptorOpts...),
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

// GRPCClientInterceptor is the interceptor that intercepts all cleint requests and adds tracing info to them
func GRPCClientInterceptor(options ...grpc_opentracing.Option) grpc.UnaryClientInterceptor {
	return grpc_opentracing.UnaryClientInterceptor(options...)
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

// wrappedServerStream embeds grpc.ServerStream and overrides Context()
// to return a derived context with additional values (e.g. trace ID, log fields).
type wrappedServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context {
	return w.ctx
}

// ResponseTimeLoggingStreamInterceptor logs response time for stream RPCs.
func ResponseTimeLoggingStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		ctx := stream.Context()
		ctx = options.AddToOptions(ctx, "", "")
		ctx = loggers.AddToLogContext(ctx, "", "")
		ctx = loggers.AddToLogContext(ctx, "grpcMethod", info.FullMethod)
		defer func(begin time.Time) {
			logArgs := []any{"method", info.FullMethod, "error", err, "took", time.Since(begin)}
			if err != nil {
				logArgs = append(logArgs, "grpcCode", status.Code(err))
			}
			log.GetLogger().Log(ctx, responseTimeLogLevel, 1, logArgs...)
		}(time.Now())
		err = handler(srv, &wrappedServerStream{ServerStream: stream, ctx: ctx})
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
		err = handler(srv, &wrappedServerStream{ServerStream: stream, ctx: ctx})
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

// filterNilServerInterceptors returns a new slice with nil entries removed.
func filterNilServerInterceptors(s []grpc.UnaryServerInterceptor) []grpc.UnaryServerInterceptor {
	out := make([]grpc.UnaryServerInterceptor, 0, len(s))
	for _, v := range s {
		if v != nil {
			out = append(out, v)
		}
	}
	return out
}

// filterNilClientInterceptors returns a new slice with nil entries removed.
func filterNilClientInterceptors(s []grpc.UnaryClientInterceptor) []grpc.UnaryClientInterceptor {
	out := make([]grpc.UnaryClientInterceptor, 0, len(s))
	for _, v := range s {
		if v != nil {
			out = append(out, v)
		}
	}
	return out
}

// filterNilStreamClientInterceptors returns a new slice with nil entries removed.
func filterNilStreamClientInterceptors(s []grpc.StreamClientInterceptor) []grpc.StreamClientInterceptor {
	out := make([]grpc.StreamClientInterceptor, 0, len(s))
	for _, v := range s {
		if v != nil {
			out = append(out, v)
		}
	}
	return out
}

// chainUnaryServer chains multiple unary server interceptors into a single interceptor.
// Nil interceptors are filtered out.
func chainUnaryServer(interceptors []grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	interceptors = filterNilServerInterceptors(interceptors)
	switch len(interceptors) {
	case 0:
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req)
		}
	case 1:
		return interceptors[0]
	default:
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			currHandler := handler
			for i := len(interceptors) - 1; i > 0; i-- {
				innerHandler := currHandler
				interceptor := interceptors[i]
				currHandler = func(currentCtx context.Context, currentReq interface{}) (interface{}, error) {
					return interceptor(currentCtx, currentReq, info, innerHandler)
				}
			}
			return interceptors[0](ctx, req, info, currHandler)
		}
	}
}

// chainUnaryClient chains multiple unary client interceptors into a single interceptor.
// Nil interceptors are filtered out.
func chainUnaryClient(interceptors []grpc.UnaryClientInterceptor) grpc.UnaryClientInterceptor {
	interceptors = filterNilClientInterceptors(interceptors)
	switch len(interceptors) {
	case 0:
		return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	case 1:
		return interceptors[0]
	default:
		return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			currInvoker := invoker
			for i := len(interceptors) - 1; i > 0; i-- {
				innerInvoker := currInvoker
				interceptor := interceptors[i]
				currInvoker = func(currentCtx context.Context, currentMethod string, currentReq, currentReply interface{}, currentConn *grpc.ClientConn, currentOpts ...grpc.CallOption) error {
					return interceptor(currentCtx, currentMethod, currentReq, currentReply, currentConn, innerInvoker, currentOpts...)
				}
			}
			return interceptors[0](ctx, method, req, reply, cc, currInvoker, opts...)
		}
	}
}

// chainStreamClient chains multiple stream client interceptors into a single interceptor.
// Nil interceptors are filtered out.
func chainStreamClient(interceptors []grpc.StreamClientInterceptor) grpc.StreamClientInterceptor {
	interceptors = filterNilStreamClientInterceptors(interceptors)
	switch len(interceptors) {
	case 0:
		return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(ctx, desc, cc, method, opts...)
		}
	case 1:
		return interceptors[0]
	default:
		return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			currStreamer := streamer
			for i := len(interceptors) - 1; i > 0; i-- {
				innerStreamer := currStreamer
				interceptor := interceptors[i]
				currStreamer = func(currentCtx context.Context, currentDesc *grpc.StreamDesc, currentConn *grpc.ClientConn, currentMethod string, currentOpts ...grpc.CallOption) (grpc.ClientStream, error) {
					return interceptor(currentCtx, currentDesc, currentConn, currentMethod, innerStreamer, currentOpts...)
				}
			}
			return interceptors[0](ctx, desc, cc, method, currStreamer, opts...)
		}
	}
}