package interceptors

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/afex/hystrix-go/hystrix"
	"github.com/go-coldbrew/errors"
	"github.com/go-coldbrew/errors/notifier"
	"github.com/go-coldbrew/log"
	"github.com/go-coldbrew/log/loggers"
	nrutil "github.com/go-coldbrew/tracing/newrelic"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_retry "github.com/grpc-ecosystem/go-grpc-middleware/retry"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	grpc_opentracing "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	newrelic "github.com/newrelic/go-agent/v3/newrelic"
	"google.golang.org/grpc"
)

var (
	//FilterMethods is the list of methods that are filtered by default
	FilterMethods = []string{"Healthcheck", "HealthCheck"}
)

func filterFromZipkin(ctx context.Context, fullMethodName string) bool {
	for _, name := range FilterMethods {
		if strings.Contains(fullMethodName, name) {
			return false
		}
	}
	return true
}

//DefaultInterceptors are the set of default interceptors that are applied to all coldbrew methods
func DefaultInterceptors() []grpc.UnaryServerInterceptor {
	return []grpc.UnaryServerInterceptor{
		ResponseTimeLoggingInterceptor(),
		grpc_ctxtags.UnaryServerInterceptor(),
		grpc_opentracing.UnaryServerInterceptor(grpc_opentracing.WithFilterFunc(filterFromZipkin)),
		grpc_prometheus.UnaryServerInterceptor,
		ServerErrorInterceptor(),
		NewRelicInterceptor(),
		PanicRecoveryInterceptor(),
	}
}

//DefaultClientInterceptors are the set of default interceptors that should be applied to all client calls
func DefaultClientInterceptors(address string) []grpc.UnaryClientInterceptor {
	return []grpc.UnaryClientInterceptor{
		grpc_retry.UnaryClientInterceptor(),
		GRPCClientInterceptor(),
		NewRelicClientInterceptor(address),
		HystrixClientInterceptor(),
	}
}

//DefaultStreamInterceptors are the set of default interceptors that should be applied to all coldbrew streams
func DefaultStreamInterceptors() []grpc.StreamServerInterceptor {
	return []grpc.StreamServerInterceptor{
		ResponseTimeLoggingStreamInterceptor(),
		grpc_ctxtags.StreamServerInterceptor(),
		grpc_opentracing.StreamServerInterceptor(),
		grpc_prometheus.StreamServerInterceptor,
		ServerErrorStreamInterceptor(),
	}
}

//DefaultClientInterceptor are the set of default interceptors that should be applied to all client calls
func DefaultClientInterceptor(address string) grpc.UnaryClientInterceptor {
	return grpc_middleware.ChainUnaryClient(DefaultClientInterceptors(address)...)
}

//DebugLoggingInterceptor is the interceptor that logs all request/response from a handler
func DebugLoggingInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		fmt.Println(info, "requst", req)
		resp, err := handler(ctx, req)
		fmt.Println(info, "response", resp, "err", err)
		return resp, err
	}
}

//ResponseTimeLoggingInterceptor logs response time for each request on server
func ResponseTimeLoggingInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func(begin time.Time) {
			log.Info(ctx, "method", info.FullMethod, "error", err, "took", time.Since(begin))
		}(time.Now())
		resp, err = handler(ctx, req)
		return resp, err
	}
}

//NewRelicInterceptor intercepts all server actions and reports them to newrelic
func NewRelicInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		ctx = nrutil.StartNRTransaction(info.FullMethod, ctx, nil, nil)
		resp, err = handler(ctx, req)
		nrutil.FinishNRTransaction(ctx, err)
		return resp, err
	}
}

//ServerErrorInterceptor intercepts all server actions and reports them to error notifier
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
		go notifier.Notify(err, ctx)
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

//NewRelicClientInterceptor intercepts all client actions and reports them to newrelic
func NewRelicClientInterceptor(address string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		txn := nrutil.GetNewRelicTransactionFromContext(ctx)
		seg := newrelic.ExternalSegment{
			StartTime: newrelic.StartSegmentNow(txn),
			URL:       "http://" + address + "/" + method,
		}
		defer seg.End()
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

//GRPCClientInterceptor is the interceptor that intercepts all cleint requests and adds tracing info to them
func GRPCClientInterceptor() grpc.UnaryClientInterceptor {
	return grpc_opentracing.UnaryClientInterceptor()
}

//HystrixClientInterceptor is the interceptor that intercepts all cleint requests and adds hystrix info to them
func HystrixClientInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		options := clientOptions{
			hystrixName: method,
		}
		for _, opt := range opts {
			if opt != nil {
				if o, ok := opt.(clientOption); ok {
					o.process(&options)
				}
			}
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
		go notifier.Notify(err, ctx)
		return err

	}
}