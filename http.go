package interceptors

import (
	"context"
	"net/http"
	"sync"

	nrutil "github.com/go-coldbrew/tracing/newrelic"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	newrelic "github.com/newrelic/go-agent/v3/newrelic"
	"google.golang.org/grpc"
)

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

// NRHttpTracer wraps an HTTP handler with New Relic tracing. The configured
// filterFunc (see SetFilterFunc) is consulted on every request: paths it
// rejects run the underlying handler without starting a New Relic
// transaction. When pattern is non-empty, newrelic.WrapHandleFunc is used so
// its route-level instrumentation stays intact for non-filtered paths.
func NRHttpTracer(pattern string, h http.HandlerFunc) (string, http.HandlerFunc) {
	app := nrutil.GetNewRelicApp()
	if app == nil {
		return pattern, h
	}
	if pattern != "" {
		wrappedPattern, wrappedHandler := newrelic.WrapHandleFunc(app, pattern, h)
		return wrappedPattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if defaultConfig.filterFunc(r.Context(), r.URL.Path) {
				wrappedHandler(w, r)
				return
			}
			h(w, r)
		})
	}
	return pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if defaultConfig.filterFunc(r.Context(), r.URL.Path) {
			txn := app.StartTransaction(r.Method + " " + r.URL.Path)
			defer txn.End()
			w = txn.SetWebResponse(w)
			txn.SetWebRequestHTTP(r)
			r = newrelic.RequestWithTransactionContext(r, txn)
		}
		h.ServeHTTP(w, r)
	})
}
