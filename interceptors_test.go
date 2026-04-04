package interceptors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-coldbrew/log/loggers"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	grpcmd "google.golang.org/grpc/metadata"
)

// mockStream implements grpc.ServerTransportStream for testing.
type mockStream struct{ method string }

func (s *mockStream) Method() string                    { return s.method }
func (s *mockStream) SetHeader(grpcmd.MD) error         { return nil }
func (s *mockStream) SendHeader(grpcmd.MD) error        { return nil }
func (s *mockStream) SetTrailer(grpcmd.MD) error        { return nil }

// grpcContext returns a context that grpc.Method() recognizes as a gRPC server context.
func grpcContext() context.Context {
	return grpc.NewContextWithServerTransportStream(
		context.Background(), &mockStream{method: "/test.Service/Method"})
}

// resetGlobals restores package-level state so tests don't interfere with each other.
func resetGlobals() {
	FilterMethods = []string{"healthcheck", "readycheck", "serverreflectioninfo"}
	currentFilter.Store(buildFilterState())
	defaultFilterFunc = FilterMethodsFunc
	unaryServerInterceptors = []grpc.UnaryServerInterceptor{}
	streamServerInterceptors = []grpc.StreamServerInterceptor{}
	useCBServerInterceptors = true
	unaryClientInterceptors = []grpc.UnaryClientInterceptor{}
	streamClientInterceptors = []grpc.StreamClientInterceptor{}
	useCBClientInterceptors = true
	responseTimeLogErrorOnly = false
	responseTimeLogLevel = loggers.InfoLevel
	httpToGRPCOnce = sync.Once{}
	httpToGRPCInterceptor = nil
}

func TestFilterMethodsFunc(t *testing.T) {
	ctx := context.Background()

	filtered := []string{
		"/grpc.health.v1.Health/healthcheck",
		"/some.Service/ReadyCheck",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	}
	for _, method := range filtered {
		if FilterMethodsFunc(ctx, method) {
			t.Errorf("expected %q to be filtered (return false), got true", method)
		}
	}

	passing := []string{
		"/mypackage.MyService/DoWork",
		"/another.Service/GetUser",
	}
	for _, method := range passing {
		if !FilterMethodsFunc(ctx, method) {
			t.Errorf("expected %q to pass (return true), got false", method)
		}
	}
}

func TestSetFilterFunc(t *testing.T) {
	defer resetGlobals()

	ctx := context.Background()
	custom := func(_ context.Context, method string) bool {
		return method == "allow"
	}
	SetFilterFunc(ctx, custom)

	if defaultFilterFunc(ctx, "allow") != true {
		t.Error("custom filter should return true for 'allow'")
	}
	if defaultFilterFunc(ctx, "deny") != false {
		t.Error("custom filter should return false for 'deny'")
	}

	// Setting nil should not change the filter.
	prev := defaultFilterFunc
	SetFilterFunc(ctx, nil)
	// We can't compare funcs directly, so just verify behaviour is unchanged.
	if defaultFilterFunc(ctx, "allow") != prev(ctx, "allow") {
		t.Error("SetFilterFunc(nil) should not change the filter")
	}
}

func TestSetFilterMethods(t *testing.T) {
	defer resetGlobals()
	// Use gRPC server context so caching is exercised.
	ctx := grpcContext()

	// "/mypackage.MyService/DoWork" passes with default filters.
	if !FilterMethodsFunc(ctx, "/mypackage.MyService/DoWork") {
		t.Fatal("DoWork should pass default filter")
	}

	// Cache is now warm for DoWork. Change filters to block it.
	SetFilterMethods(ctx, []string{"dowork"})

	// Cached decision must be invalidated — DoWork should now be filtered.
	if FilterMethodsFunc(ctx, "/mypackage.MyService/DoWork") {
		t.Error("DoWork should be filtered after SetFilterMethods")
	}

	// healthcheck should now pass since it's no longer in the filter list.
	if !FilterMethodsFunc(ctx, "/grpc.health.v1.Health/healthcheck") {
		t.Error("healthcheck should pass after SetFilterMethods removed it")
	}
}

func TestFilterMethodsFunc_HTTPPathNotCached(t *testing.T) {
	defer resetGlobals()
	// HTTP context (no gRPC metadata) — results should not be cached.
	httpCtx := context.Background()

	// Call twice with same path.
	FilterMethodsFunc(httpCtx, "/users/123")
	FilterMethodsFunc(httpCtx, "/users/456")

	// Verify nothing was cached.
	f := currentFilter.Load()
	cached := 0
	f.cache.Range(func(_, _ interface{}) bool {
		cached++
		return true
	})
	if cached != 0 {
		t.Errorf("expected 0 cached entries for HTTP paths, got %d", cached)
	}
}

func TestAddUnaryServerInterceptor(t *testing.T) {
	defer resetGlobals()

	ctx := context.Background()
	called := false
	interceptor := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		called = true
		return handler(ctx, req)
	}

	AddUnaryServerInterceptor(ctx, interceptor)

	ints := DefaultInterceptors()
	if len(ints) == 0 {
		t.Fatal("expected at least one interceptor")
	}

	// The user interceptor should be the first one.
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Method"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	}
	_, _ = ints[0](ctx, nil, info, handler)
	if !called {
		t.Error("user interceptor was not called")
	}
}

func TestDefaultInterceptors_Disabled(t *testing.T) {
	defer resetGlobals()

	ctx := context.Background()
	UseColdBrewServerInterceptors(ctx, false)

	ints := DefaultInterceptors()
	if len(ints) != 0 {
		t.Errorf("expected 0 interceptors when CB interceptors disabled, got %d", len(ints))
	}

	// Add a user interceptor and verify it still shows up.
	userCalled := false
	AddUnaryServerInterceptor(ctx, func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		userCalled = true
		return handler(ctx, req)
	})
	ints = DefaultInterceptors()
	if len(ints) != 1 {
		t.Fatalf("expected 1 interceptor, got %d", len(ints))
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/test/Method"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	}
	_, _ = ints[0](ctx, nil, info, handler)
	if !userCalled {
		t.Error("user interceptor was not called")
	}
}

func TestPanicRecoveryInterceptor(t *testing.T) {
	ctx := context.Background()
	interceptor := PanicRecoveryInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Panic"}

	panicHandler := func(ctx context.Context, req interface{}) (interface{}, error) {
		panic("test panic")
	}

	resp, err := interceptor(ctx, nil, info, panicHandler)
	if err == nil {
		t.Fatal("expected error from panic recovery, got nil")
	}
	if resp != nil {
		t.Errorf("expected nil response, got %v", resp)
	}
	expected := "panic: test panic"
	if err.Error() != expected {
		t.Errorf("expected error %q, got %q", expected, err.Error())
	}

	// Panic with an error value should return that error.
	errPanic := fmt.Errorf("error panic")
	panicHandler2 := func(ctx context.Context, req interface{}) (interface{}, error) {
		panic(errPanic)
	}
	_, err = interceptor(ctx, nil, info, panicHandler2)
	if err != errPanic {
		t.Errorf("expected original error, got %v", err)
	}
}

func TestResponseTimeLoggingInterceptor(t *testing.T) {
	ctx := context.Background()
	interceptor := ResponseTimeLoggingInterceptor(nil)
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Method"}

	handlerCalled := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		handlerCalled = true
		return "response", nil
	}

	resp, err := interceptor(ctx, "request", info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handlerCalled {
		t.Error("handler was not called")
	}
	if resp != "response" {
		t.Errorf("expected 'response', got %v", resp)
	}
}

func TestDebugLoggingInterceptor(t *testing.T) {
	ctx := context.Background()
	interceptor := DebugLoggingInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Debug"}

	handlerCalled := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		handlerCalled = true
		return "debug_resp", nil
	}

	resp, err := interceptor(ctx, "request", info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handlerCalled {
		t.Error("handler was not called")
	}
	if resp != "debug_resp" {
		t.Errorf("expected 'debug_resp', got %v", resp)
	}
}

func TestDoHTTPtoGRPC(t *testing.T) {
	defer resetGlobals()

	// Disable CB interceptors to simplify the chain.
	ctx := context.Background()
	UseColdBrewServerInterceptors(ctx, false)

	handlerCalled := false
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		handlerCalled = true
		return "result", nil
	}

	// Without RPCMethod in context, handler should be called directly.
	resp, err := DoHTTPtoGRPC(ctx, nil, handler, "input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handlerCalled {
		t.Error("handler should be called directly when no RPCMethod in context")
	}
	if resp != "result" {
		t.Errorf("expected 'result', got %v", resp)
	}

	// With a user interceptor but still no RPCMethod in context,
	// the interceptor chain should NOT be invoked.
	handlerCalled = false
	interceptorCalled := false
	AddUnaryServerInterceptor(ctx, func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		interceptorCalled = true
		return handler(ctx, req)
	})

	handlerCalled = false
	interceptorCalled = false
	resp, err = DoHTTPtoGRPC(ctx, nil, handler, "input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handlerCalled {
		t.Error("handler should be called")
	}
	if interceptorCalled {
		t.Error("interceptor should not be called without RPCMethod context")
	}

	// With RPCMethod set in context, the interceptor chain should be invoked.
	handlerCalled = false
	interceptorCalled = false
	req, _ := http.NewRequest("GET", "http://localhost/test", nil)
	mux := runtime.NewServeMux()
	ctxWithRPC, err := runtime.AnnotateIncomingContext(ctx, mux, req, "/test.Service/Method")
	if err != nil {
		t.Fatalf("AnnotateIncomingContext: %v", err)
	}
	resp, err = DoHTTPtoGRPC(ctxWithRPC, nil, handler, "input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handlerCalled {
		t.Error("handler should be called when RPCMethod is present")
	}
	if !interceptorCalled {
		t.Error("interceptor should be called when RPCMethod is present")
	}
	if resp != "result" {
		t.Errorf("expected 'result', got %v", resp)
	}
}

func TestHystrixClientInterceptor(t *testing.T) {
	ctx := context.Background()

	t.Run("basic invocation", func(t *testing.T) {
		interceptor := HystrixClientInterceptor()
		invokerCalled := false
		invoker := func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			invokerCalled = true
			return nil
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !invokerCalled {
			t.Error("invoker was not called")
		}
	})

	t.Run("WithoutHystrix short-circuits", func(t *testing.T) {
		interceptor := HystrixClientInterceptor()
		invokerCalled := false
		invoker := func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			invokerCalled = true
			return nil
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker, WithoutHystrix())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !invokerCalled {
			t.Error("invoker was not called when hystrix is disabled")
		}
	})
}

func TestChainUnaryServer(t *testing.T) {
	var order []int
	makeInterceptor := func(id int) grpc.UnaryServerInterceptor {
		return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			order = append(order, id)
			return handler(ctx, req)
		}
	}

	chain := chainUnaryServer([]grpc.UnaryServerInterceptor{
		makeInterceptor(1),
		makeInterceptor(2),
		makeInterceptor(3),
	})

	info := &grpc.UnaryServerInfo{FullMethod: "/test/Chain"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		order = append(order, 0) // handler marker
		return "ok", nil
	}

	resp, err := chain(context.Background(), nil, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %v", resp)
	}
	if len(order) != 4 || order[0] != 1 || order[1] != 2 || order[2] != 3 || order[3] != 0 {
		t.Errorf("expected execution order [1 2 3 0], got %v", order)
	}
}

func TestChainUnaryClient(t *testing.T) {
	var order []int
	makeInterceptor := func(id int) grpc.UnaryClientInterceptor {
		return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			order = append(order, id)
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}

	chain := chainUnaryClient([]grpc.UnaryClientInterceptor{
		makeInterceptor(1),
		makeInterceptor(2),
		makeInterceptor(3),
	})

	invoker := func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		order = append(order, 0)
		return nil
	}

	err := chain(context.Background(), "/test/Chain", nil, nil, nil, invoker)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 4 || order[0] != 1 || order[1] != 2 || order[2] != 3 || order[3] != 0 {
		t.Errorf("expected execution order [1 2 3 0], got %v", order)
	}
}

func TestChainStreamClient(t *testing.T) {
	var order []int
	makeInterceptor := func(id int) grpc.StreamClientInterceptor {
		return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			order = append(order, id)
			return streamer(ctx, desc, cc, method, opts...)
		}
	}

	chain := chainStreamClient([]grpc.StreamClientInterceptor{
		makeInterceptor(1),
		makeInterceptor(2),
		makeInterceptor(3),
	})

	streamer := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		order = append(order, 0)
		return nil, nil
	}

	_, err := chain(context.Background(), nil, nil, "/test/Chain", streamer)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(order) != 4 || order[0] != 1 || order[1] != 2 || order[2] != 3 || order[3] != 0 {
		t.Errorf("expected execution order [1 2 3 0], got %v", order)
	}
}

// TestChainUnaryServerConcurrent verifies that a single chained interceptor
// can be invoked concurrently from multiple goroutines without data races
// and that each goroutine sees the correct execution order and output.
// Run with -race to detect violations.
func TestChainUnaryServerConcurrent(t *testing.T) {
	chain := chainUnaryServer([]grpc.UnaryServerInterceptor{
		func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req.(string)+"-A")
		},
		func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
			return handler(ctx, req.(string)+"-B")
		},
	})

	info := &grpc.UnaryServerInfo{FullMethod: "/test/Concurrent"}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return req.(string) + "-handler", nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := chain(context.Background(), "start", info, handler)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if resp != "start-A-B-handler" {
				t.Errorf("expected 'start-A-B-handler', got %v", resp)
			}
		}()
	}
	wg.Wait()
}

// TestChainUnaryClientConcurrent verifies that the unary client chain is safe
// for concurrent use and produces the correct output from each goroutine.
func TestChainUnaryClientConcurrent(t *testing.T) {
	chain := chainUnaryClient([]grpc.UnaryClientInterceptor{
		func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method+"-A", req, reply, cc, opts...)
		},
		func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method+"-B", req, reply, cc, opts...)
		},
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got string
			invoker := func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
				got = method
				return nil
			}
			err := chain(context.Background(), "/svc/Call", nil, nil, nil, invoker)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != "/svc/Call-A-B" {
				t.Errorf("expected '/svc/Call-A-B', got %v", got)
			}
		}()
	}
	wg.Wait()
}

// TestChainStreamClientConcurrent verifies that the stream client chain is safe
// for concurrent use and produces the correct output from each goroutine.
func TestChainStreamClientConcurrent(t *testing.T) {
	chain := chainStreamClient([]grpc.StreamClientInterceptor{
		func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(ctx, desc, cc, method+"-A", opts...)
		},
		func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			return streamer(ctx, desc, cc, method+"-B", opts...)
		},
	})

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got string
			streamer := func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
				got = method
				return nil, nil
			}
			_, err := chain(context.Background(), nil, nil, "/svc/Stream", streamer)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != "/svc/Stream-A-B" {
				t.Errorf("expected '/svc/Stream-A-B', got %v", got)
			}
		}()
	}
	wg.Wait()
}

func TestGRPCClientInterceptorNoOp(t *testing.T) {
	interceptor := GRPCClientInterceptor()

	invoked := false
	mockInvoker := func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		invoked = true
		return nil
	}

	err := interceptor(context.Background(), "/test.Service/Method", nil, nil, nil, mockInvoker)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !invoked {
		t.Fatal("expected invoker to be called")
	}
}

func BenchmarkFilterMethodsFunc(b *testing.B) {
	// Reset to known state to avoid cross-test contamination.
	resetGlobals()
	// Use a gRPC server context so caching is enabled.
	grpcCtx := grpcContext()
	methods := []string{
		"/mypackage.MyService/DoWork",
		"/grpc.health.v1.Health/healthcheck",
		"/another.Service/GetUser",
	}
	b.Run("cached", func(b *testing.B) {
		// Warm the cache
		for _, m := range methods {
			FilterMethodsFunc(grpcCtx, m)
		}
		b.ResetTimer()
		b.ReportAllocs()
		for b.Loop() {
			for _, m := range methods {
				FilterMethodsFunc(grpcCtx, m)
			}
		}
	})
	b.Run("cold", func(b *testing.B) {
		// Measures full cold-path cost: cache miss + ToLower + contains scan + store.
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			currentFilter.Store(buildFilterState())
			b.StartTimer()
			for _, m := range methods {
				FilterMethodsFunc(grpcCtx, m)
			}
		}
	})
}

// noopHandler is a handler that returns immediately with no error.
var noopHandler grpc.UnaryHandler = func(ctx context.Context, req interface{}) (interface{}, error) {
	return "ok", nil
}

// errHandler is a handler that returns an error.
var errHandler grpc.UnaryHandler = func(ctx context.Context, req interface{}) (interface{}, error) {
	return nil, errors.New("test error")
}

var benchInfo = &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

func BenchmarkNewRelicInterceptor_NilApp(b *testing.B) {
	// NR app is nil by default (no license key). The interceptor should
	// be a direct pass-through with zero overhead.
	resetGlobals()
	interceptor := NewRelicInterceptor()
	ctx := grpcContext()

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		interceptor(ctx, nil, benchInfo, noopHandler)
	}
}

func BenchmarkResponseTimeLogging(b *testing.B) {
	resetGlobals()
	// Use debug level — the slog default logger discards debug, so we
	// measure interceptor + log-args-building overhead without I/O noise.
	responseTimeLogLevel = loggers.DebugLevel
	ctx := grpcContext()
	ff := FilterMethodsFunc

	b.Run("default/success", func(b *testing.B) {
		responseTimeLogErrorOnly = false
		interceptor := ResponseTimeLoggingInterceptor(ff)
		b.ResetTimer()
		b.ReportAllocs()
		for b.Loop() {
			interceptor(ctx, nil, benchInfo, noopHandler)
		}
	})

	b.Run("default/error", func(b *testing.B) {
		responseTimeLogErrorOnly = false
		interceptor := ResponseTimeLoggingInterceptor(ff)
		b.ResetTimer()
		b.ReportAllocs()
		for b.Loop() {
			interceptor(ctx, nil, benchInfo, errHandler)
		}
	})

	b.Run("error_only/success", func(b *testing.B) {
		responseTimeLogErrorOnly = true
		interceptor := ResponseTimeLoggingInterceptor(ff)
		b.ResetTimer()
		b.ReportAllocs()
		for b.Loop() {
			interceptor(ctx, nil, benchInfo, noopHandler)
		}
	})

	b.Run("error_only/error", func(b *testing.B) {
		responseTimeLogErrorOnly = true
		interceptor := ResponseTimeLoggingInterceptor(ff)
		b.ResetTimer()
		b.ReportAllocs()
		for b.Loop() {
			interceptor(ctx, nil, benchInfo, errHandler)
		}
	})

	// Restore default.
	responseTimeLogErrorOnly = false
}

func BenchmarkDefaultInterceptors(b *testing.B) {
	resetGlobals()
	ctx := grpcContext()
	chain := DefaultInterceptors()

	// Build a chained handler that applies all interceptors.
	var chainedHandler grpc.UnaryHandler = noopHandler
	for i := len(chain) - 1; i >= 0; i-- {
		next := chainedHandler
		interceptor := chain[i]
		chainedHandler = func(ctx context.Context, req interface{}) (interface{}, error) {
			return interceptor(ctx, req, benchInfo, func(ctx context.Context, req interface{}) (interface{}, error) {
				return next(ctx, req)
			})
		}
	}

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		chainedHandler(ctx, nil)
	}
}

func TestNewRelicInterceptor_NilApp(t *testing.T) {
	resetGlobals()
	interceptor := NewRelicInterceptor()
	ctx := grpcContext()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	handlerCalled := false
	resp, err := interceptor(ctx, "request", info, func(ctx context.Context, req interface{}) (interface{}, error) {
		handlerCalled = true
		return "response", nil
	})
	if !handlerCalled {
		t.Fatal("handler should be called with nil NR app")
	}
	if resp != "response" || err != nil {
		t.Fatalf("expected (response, nil), got (%v, %v)", resp, err)
	}
}

func TestNewRelicClientInterceptor_NilApp(t *testing.T) {
	resetGlobals()
	interceptor := NewRelicClientInterceptor()

	invokerCalled := false
	err := interceptor(context.Background(), "/test.Service/Method", nil, nil, nil,
		func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			invokerCalled = true
			return nil
		})
	if !invokerCalled {
		t.Fatal("invoker should be called with nil NR app")
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestResponseTimeLogErrorOnly_SkipsSuccess(t *testing.T) {
	resetGlobals()
	SetResponseTimeLogErrorOnly(true)
	defer resetGlobals()

	interceptor := ResponseTimeLoggingInterceptor(FilterMethodsFunc)
	ctx := grpcContext()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	resp, err := interceptor(ctx, nil, info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	})
	if resp != "ok" || err != nil {
		t.Fatalf("expected (ok, nil), got (%v, %v)", resp, err)
	}
	// Success path with error-only mode: interceptor should complete without logging.
	// No assertion on log output since we don't capture logs, but verifying
	// no panic and correct return values confirms the code path works.
}

func TestResponseTimeLogErrorOnly_LogsErrors(t *testing.T) {
	resetGlobals()
	SetResponseTimeLogErrorOnly(true)
	defer resetGlobals()

	interceptor := ResponseTimeLoggingInterceptor(FilterMethodsFunc)
	ctx := grpcContext()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}

	testErr := errors.New("handler failed")
	resp, err := interceptor(ctx, nil, info, func(ctx context.Context, req interface{}) (interface{}, error) {
		return nil, testErr
	})
	if resp != nil {
		t.Fatalf("expected nil resp, got %v", resp)
	}
	if err != testErr {
		t.Fatalf("expected handler error, got %v", err)
	}
}

func TestDoHTTPtoGRPC_HandlerError(t *testing.T) {
	defer resetGlobals()
	UseColdBrewServerInterceptors(context.Background(), false)

	testErr := errors.New("handler failed")
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return nil, testErr
	}

	// Without RPCMethod — error should propagate directly.
	resp, err := DoHTTPtoGRPC(context.Background(), nil, handler, "input")
	if err != testErr {
		t.Fatalf("expected testErr, got %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil resp, got %v", resp)
	}

	// With RPCMethod — error should propagate through interceptor chain.
	req, _ := http.NewRequest("GET", "http://localhost/test", nil)
	mux := runtime.NewServeMux()
	ctxWithRPC, err := runtime.AnnotateIncomingContext(context.Background(), mux, req, "/test.Service/Echo")
	if err != nil {
		t.Fatalf("AnnotateIncomingContext: %v", err)
	}
	resp, err = DoHTTPtoGRPC(ctxWithRPC, nil, handler, "input")
	if err != testErr {
		t.Fatalf("expected testErr through chain, got %v", err)
	}
	if resp != nil {
		t.Fatalf("expected nil resp through chain, got %v", resp)
	}
}

func TestDoHTTPtoGRPC_MethodPassedToInfo(t *testing.T) {
	defer resetGlobals()
	UseColdBrewServerInterceptors(context.Background(), false)

	var capturedMethod string
	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		capturedMethod = info.FullMethod
		return handler(ctx, req)
	})

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	}

	req, _ := http.NewRequest("GET", "http://localhost/test", nil)
	mux := runtime.NewServeMux()
	ctxWithRPC, err := runtime.AnnotateIncomingContext(context.Background(), mux, req, "/test.Service/Echo")
	if err != nil {
		t.Fatalf("AnnotateIncomingContext: %v", err)
	}

	_, err = DoHTTPtoGRPC(ctxWithRPC, nil, handler, "input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedMethod != "/test.Service/Echo" {
		t.Errorf("expected FullMethod '/test.Service/Echo', got %q", capturedMethod)
	}
}

func TestDoHTTPtoGRPC_InputPassedThrough(t *testing.T) {
	defer resetGlobals()
	UseColdBrewServerInterceptors(context.Background(), false)

	var capturedReq interface{}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		capturedReq = req
		return "ok", nil
	}

	// Without RPCMethod — input goes directly to handler.
	_, _ = DoHTTPtoGRPC(context.Background(), nil, handler, "direct-input")
	if capturedReq != "direct-input" {
		t.Errorf("expected 'direct-input', got %v", capturedReq)
	}

	// With RPCMethod — input goes through interceptor chain.
	capturedReq = nil
	req, _ := http.NewRequest("GET", "http://localhost/test", nil)
	mux := runtime.NewServeMux()
	ctxWithRPC, err := runtime.AnnotateIncomingContext(context.Background(), mux, req, "/test.Service/Echo")
	if err != nil {
		t.Fatalf("AnnotateIncomingContext: %v", err)
	}
	_, err = DoHTTPtoGRPC(ctxWithRPC, nil, handler, "chain-input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedReq != "chain-input" {
		t.Errorf("expected 'chain-input', got %v", capturedReq)
	}
}

func TestDoHTTPtoGRPC_ServerPassedToInfo(t *testing.T) {
	defer resetGlobals()
	UseColdBrewServerInterceptors(context.Background(), false)

	type fakeServer struct{ Name string }
	svr := &fakeServer{Name: "test-server"}

	var capturedServer interface{}
	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		capturedServer = info.Server
		return handler(ctx, req)
	})

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	}

	req, _ := http.NewRequest("GET", "http://localhost/test", nil)
	mux := runtime.NewServeMux()
	ctxWithRPC, err := runtime.AnnotateIncomingContext(context.Background(), mux, req, "/test.Service/Echo")
	if err != nil {
		t.Fatalf("AnnotateIncomingContext: %v", err)
	}

	_, err = DoHTTPtoGRPC(ctxWithRPC, svr, handler, "input")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedServer != svr {
		t.Errorf("expected server %v, got %v", svr, capturedServer)
	}
}

func TestDoHTTPtoGRPC_Concurrent(t *testing.T) {
	defer resetGlobals()
	UseColdBrewServerInterceptors(context.Background(), false)

	var callCount int64
	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		atomic.AddInt64(&callCount, 1)
		return handler(ctx, req)
	})

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return req, nil
	}

	req, _ := http.NewRequest("GET", "http://localhost/test", nil)
	mux := runtime.NewServeMux()
	ctxWithRPC, err := runtime.AnnotateIncomingContext(context.Background(), mux, req, "/test.Service/Echo")
	if err != nil {
		t.Fatalf("AnnotateIncomingContext: %v", err)
	}

	const goroutines = 50
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			resp, err := DoHTTPtoGRPC(ctxWithRPC, nil, handler, fmt.Sprintf("req-%d", n))
			if err != nil {
				t.Errorf("goroutine %d: unexpected error: %v", n, err)
			}
			expected := fmt.Sprintf("req-%d", n)
			if resp != expected {
				t.Errorf("goroutine %d: expected %q, got %v", n, expected, resp)
			}
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt64(&callCount); got != goroutines {
		t.Errorf("expected %d interceptor calls, got %d", goroutines, got)
	}
}

func TestDoHTTPtoGRPC_InterceptorCaching(t *testing.T) {
	defer resetGlobals()
	UseColdBrewServerInterceptors(context.Background(), false)

	buildCount := 0
	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		return handler(ctx, req)
	})

	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	}

	req, _ := http.NewRequest("GET", "http://localhost/test", nil)
	mux := runtime.NewServeMux()
	ctxWithRPC, err := runtime.AnnotateIncomingContext(context.Background(), mux, req, "/test.Service/Echo")
	if err != nil {
		t.Fatalf("AnnotateIncomingContext: %v", err)
	}

	// Call twice — interceptor should be built only once.
	_, _ = DoHTTPtoGRPC(ctxWithRPC, nil, handler, "first")
	_, _ = DoHTTPtoGRPC(ctxWithRPC, nil, handler, "second")

	// Adding a new interceptor after first call should NOT affect the cached chain.
	interceptor2Called := false
	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		interceptor2Called = true
		return handler(ctx, req)
	})

	_, _ = DoHTTPtoGRPC(ctxWithRPC, nil, handler, "third")
	if interceptor2Called {
		t.Error("interceptor added after first DoHTTPtoGRPC call should not be in the cached chain")
	}
	_ = buildCount // used for documentation clarity
}
