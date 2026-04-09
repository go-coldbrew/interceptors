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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"buf.build/go/protovalidate"
	"github.com/afex/hystrix-go/hystrix"
	"github.com/go-coldbrew/errors"
	"github.com/go-coldbrew/errors/notifier"
	"github.com/go-coldbrew/log"
	"github.com/go-coldbrew/log/loggers"
	"github.com/go-coldbrew/options"
	nrutil "github.com/go-coldbrew/tracing/newrelic"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	ratelimit_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/ratelimit"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/newrelic/go-agent/v3/integrations/nrgrpc"
	newrelic "github.com/newrelic/go-agent/v3/newrelic"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
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
	// Deprecated: FilterMethods is the list of methods that are filtered by default.
	// Use SetFilterMethods instead. Only some direct mutations (replacing the slice
	// or changing the first element) are detected by internal change detection;
	// other in-place changes may not invalidate caches correctly.
	FilterMethods                            = []string{"healthcheck", "readycheck", "serverreflectioninfo"}
	defaultFilterFunc                        = FilterMethodsFunc
	unaryServerInterceptors                  = []grpc.UnaryServerInterceptor{}
	streamServerInterceptors                 = []grpc.StreamServerInterceptor{}
	useCBServerInterceptors                  = true
	unaryClientInterceptors                  = []grpc.UnaryClientInterceptor{}
	streamClientInterceptors                 = []grpc.StreamClientInterceptor{}
	useCBClientInterceptors                  = true
	responseTimeLogLevel       loggers.Level = loggers.InfoLevel
	responseTimeLogErrorOnly   bool
	defaultTimeout             time.Duration = 60 * time.Second // 0 disables
	protoValidateOpts          []protovalidate.ValidatorOption
	disableProtoValidate       bool
	srvMetricsOpts             []grpcprom.ServerMetricsOption
	cltMetricsOpts             []grpcprom.ClientMetricsOption
	srvMetricsOnce             sync.Once
	srvMetrics                 *grpcprom.ServerMetrics
	cltMetricsOnce             sync.Once
	cltMetrics                 *grpcprom.ClientMetrics
	disableDebugLogInterceptor bool
	debugLogHeaderName         = "x-debug-log-level"
	disableRateLimit           bool
	rateLimiter                ratelimit_middleware.Limiter
	rateLimiterOnce            sync.Once
	rateLimiterVal             ratelimit_middleware.Limiter
	defaultRateLimit           rate.Limit = rate.Inf
	defaultRateBurst           int
)

// SetResponseTimeLogLevel sets the log level for response time logging.
// Default is InfoLevel. Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetResponseTimeLogLevel(ctx context.Context, level loggers.Level) {
	responseTimeLogLevel = level
}

// SetResponseTimeLogErrorOnly when set to true, only logs response time when
// the request returns an error. Successful requests are not logged.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetResponseTimeLogErrorOnly(errorOnly bool) {
	responseTimeLogErrorOnly = errorOnly
}

// SetDefaultTimeout sets the default timeout applied to incoming unary RPCs
// that arrive without a deadline. When set to <= 0, the timeout interceptor is
// disabled (pass-through). Default is 60s.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetDefaultTimeout(d time.Duration) {
	defaultTimeout = d
}

// If it returns false, the given request will not be traced.
type FilterFunc func(ctx context.Context, fullMethodName string) bool

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

// SetProtoValidateOptions configures custom protovalidate options (e.g.,
// custom constraints). Must be called during init() — not safe for
// concurrent use. Follows ColdBrew's init-only config pattern.
func SetProtoValidateOptions(opts ...protovalidate.ValidatorOption) {
	protoValidateOpts = append(protoValidateOpts, opts...)
}

// SetDisableProtoValidate disables the protovalidate interceptor in the
// default chain. Must be called during init() — not safe for concurrent use.
func SetDisableProtoValidate(disable bool) {
	disableProtoValidate = disable
}

// ProtoValidateInterceptor returns a unary server interceptor that validates
// incoming messages using protovalidate annotations. Returns InvalidArgument
// on validation failure. Uses GlobalValidator by default; if custom options
// are set via SetProtoValidateOptions, creates a new validator with those options.
func ProtoValidateInterceptor() grpc.UnaryServerInterceptor {
	return protovalidate_middleware.UnaryServerInterceptor(getProtoValidator())
}

// ProtoValidateStreamInterceptor returns a stream server interceptor that
// validates incoming messages using protovalidate annotations.
func ProtoValidateStreamInterceptor() grpc.StreamServerInterceptor {
	return protovalidate_middleware.StreamServerInterceptor(getProtoValidator())
}

var (
	protoValidatorOnce sync.Once
	protoValidatorVal  protovalidate.Validator
)

// getProtoValidator returns a cached protovalidate.Validator configured with
// custom options if set, falling back to GlobalValidator.
func getProtoValidator() protovalidate.Validator {
	protoValidatorOnce.Do(func() {
		if len(protoValidateOpts) > 0 {
			v, err := protovalidate.New(protoValidateOpts...)
			if err != nil {
				log.Error(context.Background(), "msg", "failed to create protovalidate validator with custom options, falling back to global", "err", err)
				protoValidatorVal = protovalidate.GlobalValidator
				return
			}
			protoValidatorVal = v
			return
		}
		protoValidatorVal = protovalidate.GlobalValidator
	})
	return protoValidatorVal
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
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		chain := handler
		for i := len(interceptors) - 1; i >= 0; i-- {
			interceptor := interceptors[i]
			next := chain
			chain = func(ctx context.Context, req any) (any, error) {
				return interceptor(ctx, req, info, next)
			}
		}
		return chain(ctx, req)
	}
}

// chainUnaryClient chains multiple unary client interceptors into one.
func chainUnaryClient(interceptors []grpc.UnaryClientInterceptor) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		chain := invoker
		for i := len(interceptors) - 1; i >= 0; i-- {
			interceptor := interceptors[i]
			next := chain
			chain = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
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

// DoHTTPtoGRPC allows calling the interceptors when you use the Register<svc-name>HandlerServer in grpc-gateway.
// This enables in-process HTTP-to-gRPC calls with the full interceptor chain (logging, tracing, metrics,
// panic recovery) without a network hop — the fastest option for gateway performance.
// The interceptor chain is cached on first invocation. All interceptor configuration
// (AddUnaryServerInterceptor, SetFilterFunc, etc.) must be finalized before the first call.
// See example below for reference.
//
//	func (s *svc) Echo(ctx context.Context, req *proto.EchoRequest) (*proto.EchoResponse, error) {
//	    handler := func(ctx context.Context, req interface{}) (interface{}, error) {
//	        return s.echo(ctx, req.(*proto.EchoRequest))
//	    }
//	    r, err := DoHTTPtoGRPC(ctx, s, handler, req)
//	    if err != nil {
//	        return nil, err
//	    }
//	    return r.(*proto.EchoResponse), nil
//	}
//
//	func (s *svc) echo(ctx context.Context, req *proto.EchoRequest) (*proto.EchoResponse, error) {
//	       .... implementation ....
//	}
func DoHTTPtoGRPC(ctx context.Context, svr any, handler func(ctx context.Context, req any) (any, error), in any) (any, error) {
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
		ints = append(ints, DefaultTimeoutInterceptor())
		if !disableRateLimit {
			if limiter := getRateLimiter(); limiter != nil {
				ints = append(ints, ratelimit_middleware.UnaryServerInterceptor(limiter))
			}
		}
		ints = append(ints,
			ResponseTimeLoggingInterceptor(defaultFilterFunc),
			TraceIdInterceptor(),
		)
		if !disableDebugLogInterceptor {
			ints = append(ints, DebugLogInterceptor())
		}
		if !disableProtoValidate {
			ints = append(ints, ProtoValidateInterceptor())
		}
		ints = append(ints,
			getServerMetrics().UnaryServerInterceptor(),
			ServerErrorInterceptor(),
			NewRelicInterceptor(),
			PanicRecoveryInterceptor(),
		)
	}
	return ints
}

// DefaultClientInterceptors are the set of default interceptors that should be applied to all client calls
func DefaultClientInterceptors(defaultOpts ...any) []grpc.UnaryClientInterceptor {
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
func DefaultClientStreamInterceptors(defaultOpts ...any) []grpc.StreamClientInterceptor {
	ints := []grpc.StreamClientInterceptor{}
	if len(streamClientInterceptors) > 0 {
		ints = append(ints, streamClientInterceptors...)
	}
	if useCBClientInterceptors {
		if nrutil.GetNewRelicApp() != nil {
			ints = append(ints, nrgrpc.StreamClientInterceptor)
		}
		ints = append(ints, getClientMetrics().StreamClientInterceptor())
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
		if !disableRateLimit {
			if limiter := getRateLimiter(); limiter != nil {
				ints = append(ints, ratelimit_middleware.StreamServerInterceptor(limiter))
			}
		}
		ints = append(ints,
			ResponseTimeLoggingStreamInterceptor(),
		)
		if !disableProtoValidate {
			ints = append(ints, ProtoValidateStreamInterceptor())
		}
		ints = append(ints,
			getServerMetrics().StreamServerInterceptor(),
			ServerErrorStreamInterceptor(),
		)
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

// DebugLoggingInterceptor is the interceptor that logs all request/response from a handler
func DebugLoggingInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		log.Debug(ctx, "method", info.FullMethod, "request", req)
		resp, err := handler(ctx, req)
		log.Debug(ctx, "method", info.FullMethod, "response", resp, "err", err)
		return resp, err
	}
}

// DefaultTimeoutInterceptor returns a unary server interceptor that applies a
// default deadline to incoming requests that have no deadline set. If the
// incoming context already has a deadline (regardless of duration), it is left
// unchanged. When defaultTimeout is <= 0, the interceptor is a no-op pass-through.
func DefaultTimeoutInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if defaultTimeout <= 0 {
			return handler(ctx, req)
		}
		if _, ok := ctx.Deadline(); ok {
			return handler(ctx, req)
		}
		ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
		return handler(ctx, req)
	}
}

// ResponseTimeLoggingInterceptor logs response time for each request on server
func ResponseTimeLoggingInterceptor(ff FilterFunc) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		ctx = loggers.AddToLogContext(ctx, "grpcMethod", info.FullMethod)
		defer func(ctx context.Context, method string, begin time.Time) {
			if ff != nil && !ff(ctx, method) {
				return
			}
			if responseTimeLogErrorOnly && err == nil {
				return
			}
			logArgs := make([]any, 0, 6)
			logArgs = append(logArgs, "error", err, "took", time.Since(begin))
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
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = options.AddToOptions(ctx, "", "")
		// loggers.AddToLogContext(ctx, "transport", "gRPC")
		return handler(ctx, req)
	}
}

// NewRelicInterceptor intercepts all server actions and reports them to newrelic.
// When NewRelic app is nil (no license key configured), returns a pass-through
// interceptor to avoid overhead.
func NewRelicInterceptor() grpc.UnaryServerInterceptor {
	app := nrutil.GetNewRelicApp()
	if app == nil {
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(ctx, req)
		}
	}
	nrh := nrgrpc.UnaryServerInterceptor(app)
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		if defaultFilterFunc(ctx, info.FullMethod) {
			return nrh(ctx, req, info, handler)
		} else {
			return handler(ctx, req)
		}
	}
}

// ServerErrorInterceptor intercepts all server actions and reports them to error notifier
func ServerErrorInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		// set trace id if not set
		ctx, _ = notifier.SetTraceIdWithValue(ctx)
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
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
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
		if defaultFilterFunc(ctx, method) {
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

// ResponseTimeLoggingStreamInterceptor logs response time for stream RPCs.
func ResponseTimeLoggingStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func(begin time.Time) {
			if responseTimeLogErrorOnly && err == nil {
				return
			}
			logArgs := make([]any, 0, 8)
			logArgs = append(logArgs, "method", info.FullMethod, "error", err, "took", time.Since(begin))
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
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		ctx := stream.Context()
		ctx, _ = notifier.SetTraceIdWithValue(ctx)
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
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
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

// SetDisableDebugLogInterceptor disables the DebugLogInterceptor in the default
// interceptor chain. Must be called during initialization, before the server starts.
func SetDisableDebugLogInterceptor(disable bool) {
	disableDebugLogInterceptor = disable
}

// SetDebugLogHeaderName sets the gRPC metadata header name that triggers
// per-request log level override. Default is "x-debug-log-level". The header
// value should be a valid log level (e.g., "debug"). Empty names are ignored.
// Must be called during initialization.
func SetDebugLogHeaderName(name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return
	}
	debugLogHeaderName = name
}

// GetDebugLogHeaderName returns the current debug log header name.
func GetDebugLogHeaderName() string {
	return debugLogHeaderName
}

// DebugLogInterceptor enables per-request log level override based on a proto
// field or gRPC metadata header. It checks (in order):
//  1. Proto field: GetDebug() bool or GetEnableDebug() bool — always sets DebugLevel
//  2. Metadata header: configurable via SetDebugLogHeaderName (default "x-debug-log-level")
//     — the header value is parsed as a log level, allowing any valid level (debug, info, warn, error)
//
// Combined with ColdBrew's trace ID propagation, this allows enabling debug
// logging for a single request and following it across services via trace ID.
func DebugLogInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		// Check proto field first
		if req != nil {
			if r, ok := req.(interface{ GetDebug() bool }); ok && r.GetDebug() {
				ctx = log.OverrideLogLevel(ctx, loggers.DebugLevel)
				return handler(ctx, req)
			}
			if r, ok := req.(interface{ GetEnableDebug() bool }); ok && r.GetEnableDebug() {
				ctx = log.OverrideLogLevel(ctx, loggers.DebugLevel)
				return handler(ctx, req)
			}
		}
		// Check gRPC metadata header
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if vals := md.Get(debugLogHeaderName); len(vals) > 0 {
				if level, err := loggers.ParseLevel(vals[0]); err == nil {
					ctx = log.OverrideLogLevel(ctx, level)
				}
			}
		}
		return handler(ctx, req)
	}
}

// SetDisableRateLimit disables the rate limiting interceptor in the default
// interceptor chain. Must be called during initialization.
func SetDisableRateLimit(disable bool) {
	disableRateLimit = disable
}

// SetRateLimiter sets a custom rate limiter implementation. This overrides the
// built-in token bucket limiter. Must be called during initialization.
func SetRateLimiter(limiter ratelimit_middleware.Limiter) {
	rateLimiter = limiter
}

// SetDefaultRateLimit configures the built-in token bucket rate limiter.
// rps is requests per second, burst is the maximum burst size.
// This is a per-pod in-memory limit — with N pods, the effective cluster-wide
// limit is N × rps. For distributed rate limiting, use SetRateLimiter() with
// a custom implementation (e.g., Redis-backed).
// Must be called during initialization.
func SetDefaultRateLimit(rps float64, burst int) {
	defaultRateLimit = rate.Limit(rps)
	if burst < 1 {
		burst = 1
	}
	defaultRateBurst = burst
}

// tokenBucketLimiter wraps golang.org/x/time/rate.Limiter to implement
// the ratelimit.Limiter interface.
type tokenBucketLimiter struct {
	limiter *rate.Limiter
}

func (l *tokenBucketLimiter) Limit(_ context.Context) error {
	if !l.limiter.Allow() {
		return fmt.Errorf("rate limit exceeded")
	}
	return nil
}

func getRateLimiter() ratelimit_middleware.Limiter {
	rateLimiterOnce.Do(func() {
		if rateLimiter != nil {
			rateLimiterVal = rateLimiter
			return
		}
		if defaultRateLimit == rate.Inf {
			rateLimiterVal = nil
			return
		}
		rateLimiterVal = &tokenBucketLimiter{
			limiter: rate.NewLimiter(defaultRateLimit, defaultRateBurst),
		}
	})
	return rateLimiterVal
}
