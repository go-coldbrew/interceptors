package interceptors

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/afex/hystrix-go/hystrix"
	"github.com/go-coldbrew/errors"
	"github.com/go-coldbrew/errors/notifier"
	"github.com/go-coldbrew/log"
	"github.com/go-coldbrew/log/loggers"
	"github.com/go-coldbrew/options"
	nrutil "github.com/go-coldbrew/tracing/newrelic"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	grpc_opentracing "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/newrelic/go-agent/v3/integrations/nrgrpc"
	newrelic "github.com/newrelic/go-agent/v3/newrelic"
	"google.golang.org/grpc"
)

var (
	//FilterMethods is the list of methods that are filtered by default
	FilterMethods             = []string{"healthcheck", "readycheck", "serverreflectioninfo"}
	defaultFilterFunc         = FilterMethodsFunc
	unaryServerInterceptors   = []grpc.UnaryServerInterceptor{}
	streamServerInterceptors  = []grpc.StreamServerInterceptor{}
	replaceServerInterceptors = false
	unaryClientInterceptors   = []grpc.UnaryClientInterceptor{}
	streamClientInterceptors  = []grpc.StreamClientInterceptor{}
	replaceClientInterceptors = false
)

// If it returns false, the given request will not be traced.
type FilterFunc func(ctx context.Context, fullMethodName string) bool

// FilterMethodsFunc is the default implementation of Filter function
func FilterMethodsFunc(ctx context.Context, fullMethodName string) bool {
	for _, name := range FilterMethods {
		if strings.Contains(strings.ToLower(fullMethodName), name) {
			return false
		}
	}
	return true
}

// SetFilterFunc sets the default filter function to be used by interceptors
func SetFilterFunc(ctx context.Context, ff FilterFunc) {
	if ff != nil {
		defaultFilterFunc = ff
	}
}

// AddUnaryServerInterceptor adds a server interceptor to default server interceptors
func AddUnaryServerInterceptor(ctx context.Context, i grpc.UnaryServerInterceptor) {
	unaryServerInterceptors = append(unaryServerInterceptors, i)
}

// AddStreamServerInterceptor adds a server interceptor to default server interceptors
func AddStreamServerInterceptor(ctx context.Context, i grpc.StreamServerInterceptor) {
	streamServerInterceptors = append(streamServerInterceptors, i)
}

// ReplaceColdBrewServerInterceptors allows enabling/disabling coldbrew server interceptors
//
// when set to true, the coldbrew server interceptors will not used
func ReplaceColdBrewServerInterceptors(ctx context.Context, flag bool) {
	replaceServerInterceptors = flag
}

// AddUnaryClientInterceptor adds a server interceptor to default server interceptors
func AddUnaryClientInterceptor(ctx context.Context, i grpc.UnaryClientInterceptor) {
	unaryClientInterceptors = append(unaryClientInterceptors, i)
}

// AddStreamClientInterceptor adds a server interceptor to default server interceptors
func AddStreamClientInterceptor(ctx context.Context, i grpc.StreamClientInterceptor) {
	streamClientInterceptors = append(streamClientInterceptors, i)
}

// ReplaceColdBrewClientInterceptors allows enabling/disabling coldbrew client interceptors
//
// when set to true, the coldbrew client interceptors will not used
func ReplaceColdBrewClientInterceptors(ctx context.Context, flag bool) {
	replaceClientInterceptors = flag
}

// DoHTTPtoGRPC allows calling the interceptors when you use the Register<svc-name>HandlerServer in grpc-gateway,
// see example below for reference
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
func DoHTTPtoGRPC(ctx context.Context, svr interface{}, handler func(ctx context.Context, req interface{}) (interface{}, error), in interface{}) (interface{}, error) {
	method, ok := runtime.RPCMethod(ctx)
	if ok {
		interceptor := grpc_middleware.ChainUnaryServer(DefaultInterceptors()...)
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
	if !replaceServerInterceptors {
		ints = append(ints,
			ResponseTimeLoggingInterceptor(defaultFilterFunc),
			TraceIdInterceptor(),
			grpc_ctxtags.UnaryServerInterceptor(),
			grpc_opentracing.UnaryServerInterceptor(grpc_opentracing.WithFilterFunc(defaultFilterFunc)),
			grpc_prometheus.UnaryServerInterceptor,
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
	if !replaceClientInterceptors {
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
		ints = append(ints, grpc_retry.UnaryClientInterceptor(),
			GRPCClientInterceptor(opentracingOpt...),
			NewRelicClientInterceptor(),
			HystrixClientInterceptor(hystrixOptions...),
			grpc_prometheus.UnaryClientInterceptor,
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
	if !replaceClientInterceptors {
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
			grpc_prometheus.StreamClientInterceptor,
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
	if !replaceServerInterceptors {
		ints = append(ints,
			ResponseTimeLoggingStreamInterceptor(),
			grpc_ctxtags.StreamServerInterceptor(),
			grpc_opentracing.StreamServerInterceptor(),
			grpc_prometheus.StreamServerInterceptor,
			ServerErrorStreamInterceptor(),
		)
	}
	return ints
}

// DefaultClientInterceptor are the set of default interceptors that should be applied to all client calls
func DefaultClientInterceptor(defaultOpts ...interface{}) grpc.UnaryClientInterceptor {
	return grpc_middleware.ChainUnaryClient(DefaultClientInterceptors(defaultOpts...)...)
}

// DefaultClientStreamInterceptor are the set of default interceptors that should be applied to all stream client calls
func DefaultClientStreamInterceptor(defaultOpts ...interface{}) grpc.StreamClientInterceptor {
	return grpc_middleware.ChainStreamClient(DefaultClientStreamInterceptors(defaultOpts...)...)
}

// DebugLoggingInterceptor is the interceptor that logs all request/response from a handler
func DebugLoggingInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		log.Debug(ctx, "method", info.FullMethod, "requst", req)
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
		defer func(ctx context.Context, method string, begin time.Time) {
			if ff != nil && !ff(ctx, method) {
				return
			}
			log.Info(ctx, "error", err, "took", time.Since(begin))
		}(ctx, info.FullMethod, time.Now())
		ctx = loggers.AddToLogContext(ctx, "grpcMethod", info.FullMethod)
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

		t := grpc_ctxtags.Extract(ctx)
		if t != nil {
			traceID := notifier.GetTraceId(ctx)
			t.Set("trace", traceID)
			ctx = loggers.AddToLogContext(ctx, "trace", traceID)
		}
		resp, err = handler(ctx, req)
		if defaultFilterFunc(ctx, info.FullMethod) {
			go notifier.Notify(err, ctx)
		}
		return resp, err
	}
}

func PanicRecoveryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func(ctx context.Context) {
			// panic handler
			if r := recover(); r != nil {
				log.Error(ctx, "panic", r, "method", info.FullMethod)
				if e, ok := r.(error); ok {
					err = e
				} else {
					err = errors.New(fmt.Sprintf("panic: %s", r))
				}
				nrutil.FinishNRTransaction(ctx, err)
				notifier.NotifyWithLevel(err, "critical", info.FullMethod, ctx)
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

// HystrixClientInterceptor is the interceptor that intercepts all client requests and adds hystrix info to them
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
		return hystrix.Do(options.hystrixName, func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = errors.Wrap(fmt.Errorf("Panic inside hystrix Method: %s, req: %v, reply: %v", method, req, reply), "Hystrix")
					log.Error(ctx, "panic", r, "method", method, "req", req, "reply", reply)
				}
			}()
			defer notifier.NotifyOnPanic(newCtx, method)
			return invoker(newCtx, method, req, reply, cc, opts...)
		}, nil)
	}
}

// ResponseTimeLoggingStreamInterceptor logs response time for stream RPCs.
func ResponseTimeLoggingStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func(begin time.Time) {
			log.Info(stream.Context(), "method", info.FullMethod, "error", err, "took", time.Since(begin))
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
		t := grpc_ctxtags.Extract(ctx)
		if t != nil {
			traceID := notifier.GetTraceId(ctx)
			t.Set("trace", traceID)
			ctx = loggers.AddToLogContext(ctx, "trace", traceID)
		}
		err = handler(srv, stream)
		if defaultFilterFunc(ctx, info.FullMethod) {
			go notifier.Notify(err, ctx)
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
				notifier.UpdateTraceId(ctx, r.GetTraceId())
			} else if r, ok := req.(interface{ GetTraceID() string }); ok {
				notifier.UpdateTraceId(ctx, r.GetTraceID())
			}
		}
		return handler(ctx, req)
	}
}
