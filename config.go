package interceptors

import (
	"context"
	"strings"
	"time"

	"buf.build/go/protovalidate"
	"github.com/go-coldbrew/log/loggers"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	ratelimit_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/ratelimit"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
)

// interceptorConfig consolidates all package-level configuration.
// All Set* functions modify fields on defaultConfig.
type interceptorConfig struct {
	// Interceptor slices
	unaryServerInterceptors  []grpc.UnaryServerInterceptor
	streamServerInterceptors []grpc.StreamServerInterceptor
	unaryClientInterceptors  []grpc.UnaryClientInterceptor
	streamClientInterceptors []grpc.StreamClientInterceptor

	// Feature flags
	useCBServerInterceptors    bool
	useCBClientInterceptors    bool
	disableProtoValidate       bool
	disableDebugLogInterceptor bool
	disableRateLimit           bool
	responseTimeLogErrorOnly   bool

	// Configuration values
	responseTimeLogLevel loggers.Level
	defaultTimeout       time.Duration
	debugLogHeaderName   string
	filterFunc           FilterFunc

	// Metrics options
	srvMetricsOpts []grpcprom.ServerMetricsOption
	cltMetricsOpts []grpcprom.ClientMetricsOption

	// ProtoValidate options
	protoValidateOpts []protovalidate.ValidatorOption

	// Rate limiting
	rateLimiter      ratelimit_middleware.Limiter
	defaultRateLimit rate.Limit
	defaultRateBurst int
}

var defaultConfig = newDefaultConfig()

func newDefaultConfig() interceptorConfig {
	return interceptorConfig{
		useCBServerInterceptors: true,
		useCBClientInterceptors: true,
		responseTimeLogLevel:    loggers.InfoLevel,
		defaultTimeout:          60 * time.Second,
		debugLogHeaderName:      "x-debug-log-level",
		filterFunc:              FilterMethodsFunc,
		defaultRateLimit:        rate.Inf,
	}
}

// SetResponseTimeLogLevel sets the log level for response time logging.
// Default is InfoLevel. Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetResponseTimeLogLevel(ctx context.Context, level loggers.Level) {
	defaultConfig.responseTimeLogLevel = level
}

// SetResponseTimeLogErrorOnly when set to true, only logs response time when
// the request returns an error. Successful requests are not logged.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetResponseTimeLogErrorOnly(errorOnly bool) {
	defaultConfig.responseTimeLogErrorOnly = errorOnly
}

// SetDefaultTimeout sets the default timeout applied to incoming unary RPCs
// that arrive without a deadline. When set to <= 0, the timeout interceptor is
// disabled (pass-through). Default is 60s.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetDefaultTimeout(d time.Duration) {
	defaultConfig.defaultTimeout = d
}

// SetFilterFunc sets the default filter function to be used by interceptors.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetFilterFunc(ctx context.Context, ff FilterFunc) {
	if ff != nil {
		defaultConfig.filterFunc = ff
	}
}

// AddUnaryServerInterceptor adds a server interceptor to default server interceptors.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func AddUnaryServerInterceptor(ctx context.Context, i ...grpc.UnaryServerInterceptor) {
	defaultConfig.unaryServerInterceptors = append(defaultConfig.unaryServerInterceptors, i...)
}

// AddStreamServerInterceptor adds a server interceptor to default server interceptors.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func AddStreamServerInterceptor(ctx context.Context, i ...grpc.StreamServerInterceptor) {
	defaultConfig.streamServerInterceptors = append(defaultConfig.streamServerInterceptors, i...)
}

// UseColdBrewServerInterceptors allows enabling/disabling coldbrew server interceptors.
// When set to false, the coldbrew server interceptors will not be used.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func UseColdBrewServerInterceptors(ctx context.Context, flag bool) {
	defaultConfig.useCBServerInterceptors = flag
}

// AddUnaryClientInterceptor adds a client interceptor to default client interceptors.
// Must be called during initialization, before any RPCs are made. Not safe for concurrent use.
func AddUnaryClientInterceptor(ctx context.Context, i ...grpc.UnaryClientInterceptor) {
	defaultConfig.unaryClientInterceptors = append(defaultConfig.unaryClientInterceptors, i...)
}

// AddStreamClientInterceptor adds a client stream interceptor to default client stream interceptors.
// Must be called during initialization, before any RPCs are made. Not safe for concurrent use.
func AddStreamClientInterceptor(ctx context.Context, i ...grpc.StreamClientInterceptor) {
	defaultConfig.streamClientInterceptors = append(defaultConfig.streamClientInterceptors, i...)
}

// UseColdBrewClientInterceptors allows enabling/disabling coldbrew client interceptors.
// When set to false, the coldbrew client interceptors will not be used.
// Must be called during initialization, before any RPCs are made. Not safe for concurrent use.
func UseColdBrewClientInterceptors(ctx context.Context, flag bool) {
	defaultConfig.useCBClientInterceptors = flag
}

// SetServerMetricsOptions appends gRPC server metrics options (histogram, labels, namespace, etc.).
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetServerMetricsOptions(opts ...grpcprom.ServerMetricsOption) {
	defaultConfig.srvMetricsOpts = append(defaultConfig.srvMetricsOpts, opts...)
}

// SetClientMetricsOptions appends gRPC client metrics options.
// Must be called during initialization, before the server starts. Not safe for concurrent use.
func SetClientMetricsOptions(opts ...grpcprom.ClientMetricsOption) {
	defaultConfig.cltMetricsOpts = append(defaultConfig.cltMetricsOpts, opts...)
}

// SetProtoValidateOptions configures custom protovalidate options (e.g.,
// custom constraints). Must be called during init() — not safe for
// concurrent use. Follows ColdBrew's init-only config pattern.
func SetProtoValidateOptions(opts ...protovalidate.ValidatorOption) {
	defaultConfig.protoValidateOpts = append(defaultConfig.protoValidateOpts, opts...)
}

// SetDisableProtoValidate disables the protovalidate interceptor in the
// default chain. Must be called during init() — not safe for concurrent use.
func SetDisableProtoValidate(disable bool) {
	defaultConfig.disableProtoValidate = disable
}

// SetDisableDebugLogInterceptor disables the DebugLogInterceptor in the default
// interceptor chain. Must be called during initialization, before the server starts.
func SetDisableDebugLogInterceptor(disable bool) {
	defaultConfig.disableDebugLogInterceptor = disable
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
	defaultConfig.debugLogHeaderName = name
}

// GetDebugLogHeaderName returns the current debug log header name.
func GetDebugLogHeaderName() string {
	return defaultConfig.debugLogHeaderName
}

// SetDisableRateLimit disables the rate limiting interceptor in the default
// interceptor chain. Must be called during initialization.
func SetDisableRateLimit(disable bool) {
	defaultConfig.disableRateLimit = disable
}

// SetRateLimiter sets a custom rate limiter implementation. This overrides the
// built-in token bucket limiter. Must be called during initialization.
func SetRateLimiter(limiter ratelimit_middleware.Limiter) {
	defaultConfig.rateLimiter = limiter
}

// SetDefaultRateLimit configures the built-in token bucket rate limiter.
// rps is requests per second, burst is the maximum burst size.
// This is a per-pod in-memory limit — with N pods, the effective cluster-wide
// limit is N × rps. For distributed rate limiting, use SetRateLimiter() with
// a custom implementation (e.g., Redis-backed).
// Must be called during initialization.
func SetDefaultRateLimit(rps float64, burst int) {
	defaultConfig.defaultRateLimit = rate.Limit(rps)
	if burst < 1 {
		burst = 1
	}
	defaultConfig.defaultRateBurst = burst
}
