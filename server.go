package interceptors

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"buf.build/go/protovalidate"
	"github.com/go-coldbrew/errors"
	"github.com/go-coldbrew/errors/notifier"
	"github.com/go-coldbrew/log"
	"github.com/go-coldbrew/log/loggers"
	"github.com/go-coldbrew/options"
	nrutil "github.com/go-coldbrew/tracing/newrelic"
	protovalidate_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/protovalidate"
	ratelimit_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/ratelimit"
	"github.com/newrelic/go-agent/v3/integrations/nrgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var (
	protoValidatorOnce sync.Once
	protoValidatorVal  protovalidate.Validator
)

// getProtoValidator returns a cached protovalidate.Validator configured with
// custom options if set, falling back to GlobalValidator.
func getProtoValidator() protovalidate.Validator {
	protoValidatorOnce.Do(func() {
		if len(defaultConfig.protoValidateOpts) > 0 {
			v, err := protovalidate.New(defaultConfig.protoValidateOpts...)
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

// Interceptor ordering contract (read before reordering).
//
// The gRPC server chain helper (chainUnaryServer in chain.go) wraps
// interceptors LAST-FIRST, so the LAST element of the slice returned by
// DefaultInterceptors / DefaultStreamInterceptors is the INNERMOST (runs
// closest to the handler) and the FIRST element is the OUTERMOST (runs
// first on an incoming request).
//
// The unaryPos* / streamPos* constants below encode the required layering;
// changing a position changes observable server semantics:
//
//   - Timeout / rate-limit are OUTERMOST. They short-circuit or cap work
//     before any other interceptor runs.
//   - Response-time logging, trace-id propagation, and the debug-log override
//     run next. They set up context fields that downstream interceptors and
//     the handler rely on.
//   - Protovalidate runs BEFORE metrics / error reporting / tracing so that
//     validation failures are visible to them as InvalidArgument errors.
//   - Metrics, ServerErrorInterceptor, and New Relic wrap the handler from
//     the OUTSIDE. They observe the final error/response, including errors
//     synthesized by the panic-recovery layer.
//   - Panic recovery is INNERMOST. Handler panics are recovered and converted
//     to errors, which then propagate outward through error reporting,
//     metrics, and tracing so those layers record the call as a failure
//     rather than a success.
//
// User-supplied interceptors registered via AddUnaryServerInterceptor /
// AddStreamServerInterceptor are prepended OUTERMOST, before the ColdBrew
// (CB) set.
//
// Tests in interceptors_test.go (TestInterceptorPositionConstants,
// TestDefaultInterceptors_SlotWiring, TestDefaultInterceptors_PanicThroughFullChain,
// TestDefaultInterceptors_UserInterceptorsOutermost, and their stream
// variants) guard this contract.
const (
	unaryPosTimeout = iota // outermost
	unaryPosRateLimit
	unaryPosResponseTimeLog
	unaryPosTraceID
	unaryPosDebugLog
	unaryPosProtoValidate
	unaryPosMetrics
	unaryPosServerError
	unaryPosNewRelic
	unaryPosPanicRecovery // innermost
	unaryPosCount
)

const (
	streamPosRateLimit = iota // outermost
	streamPosResponseTimeLog
	streamPosProtoValidate
	streamPosMetrics
	streamPosServerError
	streamPosPanicRecovery // innermost
	streamPosCount
)

// DefaultInterceptors returns the default unary server interceptor chain.
// The ordering is defined by the unaryPos* constants above; this function
// assigns each interceptor to its named slot and drops any slot that is
// disabled via configuration. See the ordering contract above for semantics.
func DefaultInterceptors() []grpc.UnaryServerInterceptor {
	ints := make([]grpc.UnaryServerInterceptor, 0, len(defaultConfig.unaryServerInterceptors)+unaryPosCount)
	ints = append(ints, defaultConfig.unaryServerInterceptors...)
	if !defaultConfig.useCBServerInterceptors {
		return ints
	}

	cb := make([]grpc.UnaryServerInterceptor, unaryPosCount)
	cb[unaryPosTimeout] = DefaultTimeoutInterceptor()
	if !defaultConfig.disableRateLimit {
		if limiter := getRateLimiter(); limiter != nil {
			cb[unaryPosRateLimit] = ratelimit_middleware.UnaryServerInterceptor(limiter)
		}
	}
	cb[unaryPosResponseTimeLog] = ResponseTimeLoggingInterceptor(defaultConfig.filterFunc)
	cb[unaryPosTraceID] = TraceIdInterceptor()
	if !defaultConfig.disableDebugLogInterceptor {
		cb[unaryPosDebugLog] = DebugLogInterceptor()
	}
	if !defaultConfig.disableProtoValidate {
		cb[unaryPosProtoValidate] = ProtoValidateInterceptor()
	}
	cb[unaryPosMetrics] = getServerMetrics().UnaryServerInterceptor()
	cb[unaryPosServerError] = ServerErrorInterceptor()
	cb[unaryPosNewRelic] = NewRelicInterceptor()
	cb[unaryPosPanicRecovery] = PanicRecoveryInterceptor()

	for _, i := range cb {
		if i != nil {
			ints = append(ints, i)
		}
	}
	return ints
}

// DefaultStreamInterceptors returns the default stream server interceptor
// chain. The ordering is defined by the streamPos* constants above; this
// function assigns each interceptor to its named slot and drops any slot
// that is disabled via configuration. See the ordering contract above for
// semantics.
func DefaultStreamInterceptors() []grpc.StreamServerInterceptor {
	ints := make([]grpc.StreamServerInterceptor, 0, len(defaultConfig.streamServerInterceptors)+streamPosCount)
	ints = append(ints, defaultConfig.streamServerInterceptors...)
	if !defaultConfig.useCBServerInterceptors {
		return ints
	}

	cb := make([]grpc.StreamServerInterceptor, streamPosCount)
	if !defaultConfig.disableRateLimit {
		if limiter := getRateLimiter(); limiter != nil {
			cb[streamPosRateLimit] = ratelimit_middleware.StreamServerInterceptor(limiter)
		}
	}
	cb[streamPosResponseTimeLog] = ResponseTimeLoggingStreamInterceptor()
	if !defaultConfig.disableProtoValidate {
		cb[streamPosProtoValidate] = ProtoValidateStreamInterceptor()
	}
	cb[streamPosMetrics] = getServerMetrics().StreamServerInterceptor()
	cb[streamPosServerError] = ServerErrorStreamInterceptor()
	cb[streamPosPanicRecovery] = PanicRecoveryStreamInterceptor()

	for _, i := range cb {
		if i != nil {
			ints = append(ints, i)
		}
	}
	return ints
}

// DefaultTimeoutInterceptor returns a unary server interceptor that applies a
// default deadline to incoming requests that have no deadline set. If the
// incoming context already has a deadline (regardless of duration), it is left
// unchanged. When defaultTimeout is <= 0, the interceptor is a no-op pass-through.
func DefaultTimeoutInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if defaultConfig.defaultTimeout <= 0 {
			return handler(ctx, req)
		}
		if _, ok := ctx.Deadline(); ok {
			return handler(ctx, req)
		}
		ctx, cancel := context.WithTimeout(ctx, defaultConfig.defaultTimeout)
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
			if defaultConfig.responseTimeLogErrorOnly && err == nil {
				return
			}
			logArgs := make([]any, 0, 6)
			logArgs = append(logArgs, "error", err, "took", time.Since(begin))
			if err != nil {
				logArgs = append(logArgs, "grpcCode", status.Code(err))
			}
			log.GetLogger().Log(ctx, defaultConfig.responseTimeLogLevel, 1, logArgs...)
		}(ctx, info.FullMethod, time.Now())
		resp, err = handler(ctx, req)
		return resp, err
	}
}

// ResponseTimeLoggingStreamInterceptor logs response time for stream RPCs.
func ResponseTimeLoggingStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func(begin time.Time) {
			if defaultConfig.responseTimeLogErrorOnly && err == nil {
				return
			}
			logArgs := make([]any, 0, 8)
			logArgs = append(logArgs, "method", info.FullMethod, "error", err, "took", time.Since(begin))
			if err != nil {
				logArgs = append(logArgs, "grpcCode", status.Code(err))
			}
			log.GetLogger().Log(stream.Context(), defaultConfig.responseTimeLogLevel, 1, logArgs...)
		}(time.Now())
		err = handler(srv, stream)
		return err
	}
}

func OptionsInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = options.AddToOptions(ctx, "", "")
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
		if defaultConfig.filterFunc(ctx, info.FullMethod) {
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
		if err != nil && defaultConfig.filterFunc(ctx, info.FullMethod) {
			_ = notifier.NotifyAsync(err, ctx, notifier.Tags{
				"grpcMethod": info.FullMethod,
				"duration":   time.Since(start).Truncate(time.Millisecond).String(),
			})
		}
		return resp, err
	}
}

// wrappedStream wraps a grpc.ServerStream to override its context.
type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *wrappedStream) Context() context.Context { return w.ctx }

// ServerErrorStreamInterceptor intercepts server errors for stream RPCs and
// reports them to the error notifier.
func ServerErrorStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		ctx := stream.Context()
		ctx, _ = notifier.SetTraceIdWithValue(ctx)
		start := time.Now()
		err = handler(srv, &wrappedStream{ServerStream: stream, ctx: ctx})
		if err != nil && defaultConfig.filterFunc(ctx, info.FullMethod) {
			_ = notifier.NotifyAsync(err, ctx, notifier.Tags{
				"grpcMethod": info.FullMethod,
				"duration":   time.Since(start).Truncate(time.Millisecond).String(),
			})
		}
		return err
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
					err = errors.New(fmt.Sprintf("panic: %v", r))
				}
				nrutil.FinishNRTransaction(ctx, err)
				_ = notifier.NotifyWithLevel(err, "critical", info.FullMethod, ctx, stack)
			}
		}(ctx)

		resp, err = handler(ctx, req)
		return resp, err
	}
}

// PanicRecoveryStreamInterceptor recovers from panics in stream handlers,
// logs the panic and stack trace, and reports it to the error notifier.
func PanicRecoveryStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				ctx := stream.Context()
				stack := string(debug.Stack())
				log.Error(ctx, "panic", r, "method", info.FullMethod, "stack", stack)
				if e, ok := r.(error); ok {
					err = e
				} else {
					err = errors.New(fmt.Sprintf("panic: %v", r))
				}
				nrutil.FinishNRTransaction(ctx, err)
				_ = notifier.NotifyWithLevel(err, "critical", info.FullMethod, ctx, stack)
			}
		}()
		return handler(srv, stream)
	}
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
			if vals := md.Get(defaultConfig.debugLogHeaderName); len(vals) > 0 {
				if level, err := loggers.ParseLevel(vals[0]); err == nil {
					ctx = log.OverrideLogLevel(ctx, level)
				}
			}
		}
		return handler(ctx, req)
	}
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
