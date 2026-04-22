package interceptors

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-coldbrew/errors/notifier"
	"github.com/go-coldbrew/log"
	"github.com/go-coldbrew/log/loggers"
	nrutil "github.com/go-coldbrew/tracing/newrelic"
	ratelimit_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/ratelimit"
	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	newrelic "github.com/newrelic/go-agent/v3/newrelic"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcmd "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// mockStream implements grpc.ServerTransportStream for testing.
type mockStream struct{ method string }

func (s *mockStream) Method() string             { return s.method }
func (s *mockStream) SetHeader(grpcmd.MD) error  { return nil }
func (s *mockStream) SendHeader(grpcmd.MD) error { return nil }
func (s *mockStream) SetTrailer(grpcmd.MD) error { return nil }

// grpcContext returns a context that grpc.Method() recognizes as a gRPC server context.
func grpcContext() context.Context {
	return grpc.NewContextWithServerTransportStream(
		context.Background(), &mockStream{method: "/test.Service/Method"})
}

// resetGlobals restores package-level state so tests don't interfere with each other.
func resetGlobals() {
	defaultConfig = newDefaultConfig()
	FilterMethods = []string{"healthcheck", "readycheck", "serverreflectioninfo"}
	currentFilter.Store(buildFilterState())
	httpToGRPCOnce = sync.Once{}
	httpToGRPCInterceptor = nil
	rateLimiterOnce = sync.Once{}
	rateLimiterVal = nil
	srvMetricsOnce = sync.Once{}
	srvMetrics = nil
	cltMetricsOnce = sync.Once{}
	cltMetrics = nil
	protoValidatorOnce = sync.Once{}
	protoValidatorVal = nil
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

	if defaultConfig.filterFunc(ctx, "allow") != true {
		t.Error("custom filter should return true for 'allow'")
	}
	if defaultConfig.filterFunc(ctx, "deny") != false {
		t.Error("custom filter should return false for 'deny'")
	}

	// Setting nil should not change the filter.
	prev := defaultConfig.filterFunc
	SetFilterFunc(ctx, nil)
	// We can't compare funcs directly, so just verify behaviour is unchanged.
	if defaultConfig.filterFunc(ctx, "allow") != prev(ctx, "allow") {
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

// TestFilterMethods_DirectMutationDetected guards issue #41: the cache must
// invalidate when the deprecated FilterMethods var is mutated in place,
// including at indices beyond 0 (which the old length/first-element probe
// missed).
func TestFilterMethods_DirectMutationDetected(t *testing.T) {
	defer resetGlobals()
	ctx := grpcContext()

	// Prime the cache.
	if !FilterMethodsFunc(ctx, "/svc.A/Foo") {
		t.Fatal("expected /svc.A/Foo to pass default filter")
	}
	if FilterMethodsFunc(ctx, "/svc.A/healthcheck") {
		t.Fatal("expected /svc.A/healthcheck to be filtered by default")
	}

	// Mutate an element other than index 0 — the old detection would miss this.
	// Default is {"healthcheck", "readycheck", "serverreflectioninfo"}; replace
	// "readycheck" with "foo" so /svc.A/Foo should now be filtered and
	// /svc.A/readycheck should pass.
	FilterMethods[1] = "foo"

	if FilterMethodsFunc(ctx, "/svc.A/Foo") {
		t.Error("/svc.A/Foo should be filtered after index-1 mutation")
	}
	if !FilterMethodsFunc(ctx, "/svc.A/readycheck") {
		t.Error("/svc.A/readycheck should pass after 'readycheck' was replaced at index 1")
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
	f.cache.Range(func(_, _ any) bool {
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
	interceptor := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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
	handler := func(ctx context.Context, req any) (any, error) {
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
	AddUnaryServerInterceptor(ctx, func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		userCalled = true
		return handler(ctx, req)
	})
	ints = DefaultInterceptors()
	if len(ints) != 1 {
		t.Fatalf("expected 1 interceptor, got %d", len(ints))
	}

	info := &grpc.UnaryServerInfo{FullMethod: "/test/Method"}
	handler := func(ctx context.Context, req any) (any, error) {
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

	panicHandler := func(ctx context.Context, req any) (any, error) {
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
	panicHandler2 := func(ctx context.Context, req any) (any, error) {
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
	handler := func(ctx context.Context, req any) (any, error) {
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
	handler := func(ctx context.Context, req any) (any, error) {
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
	handler := func(ctx context.Context, req any) (any, error) {
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
	AddUnaryServerInterceptor(ctx, func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
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
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
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

func TestExecutorClientInterceptor(t *testing.T) {
	ctx := context.Background()

	t.Run("no executor passthrough", func(t *testing.T) {
		resetGlobals()
		interceptor := ExecutorClientInterceptor()
		invokerCalled := false
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
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

	t.Run("global executor", func(t *testing.T) {
		resetGlobals()
		executorCalled := false
		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			executorCalled = true
			return fn(ctx)
		})
		defer resetGlobals()

		interceptor := ExecutorClientInterceptor()
		invokerCalled := false
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			invokerCalled = true
			return nil
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !executorCalled {
			t.Error("executor was not called")
		}
		if !invokerCalled {
			t.Error("invoker was not called")
		}
	})

	t.Run("per-call executor overrides global", func(t *testing.T) {
		resetGlobals()
		globalCalled := false
		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			globalCalled = true
			return fn(ctx)
		})
		defer resetGlobals()

		perCallCalled := false
		perCallExec := func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			perCallCalled = true
			return fn(ctx)
		}

		interceptor := ExecutorClientInterceptor()
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return nil
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker, WithExecutor(perCallExec))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if globalCalled {
			t.Error("global executor should not be called when per-call is set")
		}
		if !perCallCalled {
			t.Error("per-call executor was not called")
		}
	})

	t.Run("WithoutExecutor disables global", func(t *testing.T) {
		resetGlobals()
		executorCalled := false
		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			executorCalled = true
			return fn(ctx)
		})
		defer resetGlobals()

		interceptor := ExecutorClientInterceptor()
		invokerCalled := false
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			invokerCalled = true
			return nil
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker, WithoutExecutor())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if executorCalled {
			t.Error("executor should not be called when WithoutExecutor is used")
		}
		if !invokerCalled {
			t.Error("invoker was not called")
		}
	})

	t.Run("excluded errors", func(t *testing.T) {
		resetGlobals()
		errExpected := errors.New("expected error")
		var executorSawErr error

		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			executorSawErr = fn(ctx)
			return executorSawErr
		})
		defer resetGlobals()

		interceptor := ExecutorClientInterceptor()
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return errExpected
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker, WithExcludedErrors(errExpected))
		if err == nil {
			t.Fatal("expected error to be returned to caller")
		}
		if !errors.Is(err, errExpected) {
			t.Fatalf("expected %v, got %v", errExpected, err)
		}
		if executorSawErr != nil {
			t.Error("executor should have seen nil error for excluded error")
		}
	})

	t.Run("excluded codes", func(t *testing.T) {
		resetGlobals()
		var executorSawErr error

		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			executorSawErr = fn(ctx)
			return executorSawErr
		})
		defer resetGlobals()

		interceptor := ExecutorClientInterceptor()
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return status.Error(codes.NotFound, "not found")
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker, WithExcludedCodes(codes.NotFound))
		if err == nil {
			t.Fatal("expected error to be returned to caller")
		}
		if s, ok := status.FromError(err); !ok || s.Code() != codes.NotFound {
			t.Fatalf("expected NotFound, got %v", err)
		}
		if executorSawErr != nil {
			t.Error("executor should have seen nil error for excluded code")
		}
	})

	t.Run("panic recovery", func(t *testing.T) {
		resetGlobals()
		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			return fn(ctx)
		})
		defer resetGlobals()

		interceptor := ExecutorClientInterceptor()
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			panic("test panic")
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker)
		if err == nil {
			t.Fatal("expected error from panic recovery")
		}
		if !strings.Contains(err.Error(), "panic in executor method") {
			t.Fatalf("expected wrapped panic error containing 'panic in executor method', got: %v", err)
		}
	})

	t.Run("executor error returned", func(t *testing.T) {
		resetGlobals()
		errCircuitOpen := errors.New("circuit open")
		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			_ = fn(ctx)
			return errCircuitOpen
		})
		defer resetGlobals()

		interceptor := ExecutorClientInterceptor()
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return nil
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker)
		if !errors.Is(err, errCircuitOpen) {
			t.Fatalf("expected circuit open error, got %v", err)
		}
	})

	t.Run("invoker error takes priority over executor error", func(t *testing.T) {
		resetGlobals()
		errInvoker := errors.New("invoker error")
		errExecutor := errors.New("executor error")
		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			fn(ctx) //nolint:errcheck
			return errExecutor
		})
		defer resetGlobals()

		interceptor := ExecutorClientInterceptor()
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return errInvoker
		}

		err := interceptor(ctx, "/test/Method", nil, nil, nil, invoker)
		if !errors.Is(err, errInvoker) {
			t.Fatalf("expected invoker error, got %v", err)
		}
	})

	t.Run("executor receives method name", func(t *testing.T) {
		resetGlobals()
		var receivedMethod string
		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			receivedMethod = method
			return fn(ctx)
		})
		defer resetGlobals()

		interceptor := ExecutorClientInterceptor()
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return nil
		}

		err := interceptor(ctx, "/my.package/MyMethod", nil, nil, nil, invoker)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if receivedMethod != "/my.package/MyMethod" {
			t.Fatalf("expected method /my.package/MyMethod, got %s", receivedMethod)
		}
	})
}

func TestDefaultClientInterceptors_ExecutorBranching(t *testing.T) {
	t.Run("uses hystrix when no executor", func(t *testing.T) {
		resetGlobals()
		defer resetGlobals()

		ints := DefaultClientInterceptors()
		// Should have interceptors (hystrix path)
		if len(ints) == 0 {
			t.Fatal("expected interceptors in default chain")
		}
	})

	t.Run("uses executor when set", func(t *testing.T) {
		resetGlobals()
		executorCalled := false
		SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
			executorCalled = true
			return fn(ctx)
		})
		defer resetGlobals()

		ints := DefaultClientInterceptors()
		if len(ints) == 0 {
			t.Fatal("expected interceptors in default chain")
		}
		// Call the first interceptor (should be executor, not hystrix)
		invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return nil
		}
		err := ints[0](context.Background(), "/test/Method", nil, nil, nil, invoker)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !executorCalled {
			t.Error("executor interceptor should have been used, not hystrix")
		}
	})
}

func TestChainUnaryServer(t *testing.T) {
	var order []int
	makeInterceptor := func(id int) grpc.UnaryServerInterceptor {
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
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
	handler := func(ctx context.Context, req any) (any, error) {
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
		return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			order = append(order, id)
			return invoker(ctx, method, req, reply, cc, opts...)
		}
	}

	chain := chainUnaryClient([]grpc.UnaryClientInterceptor{
		makeInterceptor(1),
		makeInterceptor(2),
		makeInterceptor(3),
	})

	invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
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
		func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(ctx, req.(string)+"-A")
		},
		func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(ctx, req.(string)+"-B")
		},
	})

	info := &grpc.UnaryServerInfo{FullMethod: "/test/Concurrent"}
	handler := func(ctx context.Context, req any) (any, error) {
		return req.(string) + "-handler", nil
	}

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			resp, err := chain(context.Background(), "start", info, handler)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if resp != "start-A-B-handler" {
				t.Errorf("expected 'start-A-B-handler', got %v", resp)
			}
		})
	}
	wg.Wait()
}

// TestChainUnaryClientConcurrent verifies that the unary client chain is safe
// for concurrent use and produces the correct output from each goroutine.
func TestChainUnaryClientConcurrent(t *testing.T) {
	chain := chainUnaryClient([]grpc.UnaryClientInterceptor{
		func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method+"-A", req, reply, cc, opts...)
		},
		func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			return invoker(ctx, method+"-B", req, reply, cc, opts...)
		},
	})

	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			var got string
			invoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
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
		})
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
	for range 100 {
		wg.Go(func() {
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
		})
	}
	wg.Wait()
}

func TestGRPCClientInterceptorNoOp(t *testing.T) {
	interceptor := GRPCClientInterceptor()

	invoked := false
	mockInvoker := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
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
var noopHandler grpc.UnaryHandler = func(ctx context.Context, req any) (any, error) {
	return "ok", nil
}

// errHandler is a handler that returns an error.
var errHandler grpc.UnaryHandler = func(ctx context.Context, req any) (any, error) {
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
	defaultConfig.responseTimeLogLevel = loggers.DebugLevel
	ctx := grpcContext()
	ff := FilterMethodsFunc

	b.Run("default/success", func(b *testing.B) {
		defaultConfig.responseTimeLogErrorOnly = false
		interceptor := ResponseTimeLoggingInterceptor(ff)
		b.ResetTimer()
		b.ReportAllocs()
		for b.Loop() {
			interceptor(ctx, nil, benchInfo, noopHandler)
		}
	})

	b.Run("default/error", func(b *testing.B) {
		defaultConfig.responseTimeLogErrorOnly = false
		interceptor := ResponseTimeLoggingInterceptor(ff)
		b.ResetTimer()
		b.ReportAllocs()
		for b.Loop() {
			interceptor(ctx, nil, benchInfo, errHandler)
		}
	})

	b.Run("error_only/success", func(b *testing.B) {
		defaultConfig.responseTimeLogErrorOnly = true
		interceptor := ResponseTimeLoggingInterceptor(ff)
		b.ResetTimer()
		b.ReportAllocs()
		for b.Loop() {
			interceptor(ctx, nil, benchInfo, noopHandler)
		}
	})

	b.Run("error_only/error", func(b *testing.B) {
		defaultConfig.responseTimeLogErrorOnly = true
		interceptor := ResponseTimeLoggingInterceptor(ff)
		b.ResetTimer()
		b.ReportAllocs()
		for b.Loop() {
			interceptor(ctx, nil, benchInfo, errHandler)
		}
	})

	// Restore default.
	defaultConfig.responseTimeLogErrorOnly = false
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
		chainedHandler = func(ctx context.Context, req any) (any, error) {
			return interceptor(ctx, req, benchInfo, func(ctx context.Context, req any) (any, error) {
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
	resp, err := interceptor(ctx, "request", info, func(ctx context.Context, req any) (any, error) {
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
		func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
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

	resp, err := interceptor(ctx, nil, info, func(ctx context.Context, req any) (any, error) {
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
	resp, err := interceptor(ctx, nil, info, func(ctx context.Context, req any) (any, error) {
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
	handler := func(ctx context.Context, req any) (any, error) {
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
	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		capturedMethod = info.FullMethod
		return handler(ctx, req)
	})

	handler := func(ctx context.Context, req any) (any, error) {
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

	var capturedReq any
	handler := func(ctx context.Context, req any) (any, error) {
		capturedReq = req
		return "ok", nil
	}

	// Without RPCMethod — input goes directly to handler.
	if _, err := DoHTTPtoGRPC(context.Background(), nil, handler, "direct-input"); err != nil {
		t.Fatalf("DoHTTPtoGRPC without RPCMethod: %v", err)
	}
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

	var capturedServer any
	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		capturedServer = info.Server
		return handler(ctx, req)
	})

	handler := func(ctx context.Context, req any) (any, error) {
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
	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		atomic.AddInt64(&callCount, 1)
		return handler(ctx, req)
	})

	handler := func(ctx context.Context, req any) (any, error) {
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
	for i := range goroutines {
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

	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(ctx, req)
	})

	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}

	req, _ := http.NewRequest("GET", "http://localhost/test", nil)
	mux := runtime.NewServeMux()
	ctxWithRPC, err := runtime.AnnotateIncomingContext(context.Background(), mux, req, "/test.Service/Echo")
	if err != nil {
		t.Fatalf("AnnotateIncomingContext: %v", err)
	}

	// Call twice — interceptor should be built only once.
	if _, err := DoHTTPtoGRPC(ctxWithRPC, nil, handler, "first"); err != nil {
		t.Fatalf("first DoHTTPtoGRPC: %v", err)
	}
	if _, err := DoHTTPtoGRPC(ctxWithRPC, nil, handler, "second"); err != nil {
		t.Fatalf("second DoHTTPtoGRPC: %v", err)
	}

	// Adding a new interceptor after first call should NOT affect the cached chain.
	interceptor2Called := false
	AddUnaryServerInterceptor(context.Background(), func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		interceptor2Called = true
		return handler(ctx, req)
	})

	if _, err := DoHTTPtoGRPC(ctxWithRPC, nil, handler, "third"); err != nil {
		t.Fatalf("third DoHTTPtoGRPC: %v", err)
	}
	if interceptor2Called {
		t.Error("interceptor added after first DoHTTPtoGRPC call should not be in the cached chain")
	}
}

func TestDefaultTimeoutInterceptor_AppliesTimeout(t *testing.T) {
	defer resetGlobals()

	interceptor := DefaultTimeoutInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	ctx := context.Background()

	handler := func(ctx context.Context, req any) (any, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected deadline to be set")
		}
		remaining := time.Until(deadline)
		if remaining < 59*time.Second || remaining > 61*time.Second {
			t.Fatalf("expected ~60s deadline, got %v", remaining)
		}
		return "ok", nil
	}

	resp, err := interceptor(ctx, nil, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected 'ok', got %v", resp)
	}
}

func TestDefaultTimeoutInterceptor_ExistingDeadline(t *testing.T) {
	defer resetGlobals()

	interceptor := DefaultTimeoutInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	handler := func(ctx context.Context, req any) (any, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("expected deadline to be set")
		}
		remaining := time.Until(deadline)
		if remaining > 6*time.Second {
			t.Fatalf("deadline should be ~5s from caller, got %v", remaining)
		}
		return "ok", nil
	}

	_, err := interceptor(ctx, nil, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultTimeoutInterceptor_Disabled(t *testing.T) {
	defer resetGlobals()

	SetDefaultTimeout(0)
	interceptor := DefaultTimeoutInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	ctx := context.Background()

	handler := func(ctx context.Context, req any) (any, error) {
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("expected no deadline when timeout is disabled")
		}
		return "ok", nil
	}

	_, err := interceptor(ctx, nil, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDefaultInterceptors_IncludesTimeout(t *testing.T) {
	defer resetGlobals()

	// Call the DefaultTimeoutInterceptor directly from the chain to verify it's wired in.
	// The first CB interceptor (index 0 when no user interceptors) should be the timeout.
	ints := DefaultInterceptors()
	if len(ints) == 0 {
		t.Fatal("expected at least one interceptor")
	}

	// The first interceptor should set a deadline on a bare context.
	ctx := context.Background()
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Method"}
	deadlineSet := false

	handler := func(ctx context.Context, req any) (any, error) {
		if _, ok := ctx.Deadline(); ok {
			deadlineSet = true
		}
		return "ok", nil
	}

	_, err := ints[0](ctx, nil, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !deadlineSet {
		t.Error("expected first CB interceptor (DefaultTimeoutInterceptor) to set a deadline")
	}
}

// --- DebugLogInterceptor tests ---

type debugRequest struct{ debug bool }

func (r *debugRequest) GetDebug() bool { return r.debug }

type enableDebugRequest struct{ enable bool }

func (r *enableDebugRequest) GetEnableDebug() bool { return r.enable }

type plainRequest struct{}

func TestDebugLogInterceptor_ProtoFieldDebug(t *testing.T) {
	resetGlobals()
	interceptor := DebugLogInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Debug"}

	var capturedCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) {
		capturedCtx = ctx
		return "ok", nil
	}

	_, err := interceptor(context.Background(), &debugRequest{debug: true}, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	level, found := log.GetOverridenLogLevel(capturedCtx)
	if !found || level != loggers.DebugLevel {
		t.Errorf("expected debug level override, found=%v level=%v", found, level)
	}
}

func TestDebugLogInterceptor_ProtoFieldEnableDebug(t *testing.T) {
	resetGlobals()
	interceptor := DebugLogInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Debug"}

	var capturedCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) {
		capturedCtx = ctx
		return "ok", nil
	}

	_, err := interceptor(context.Background(), &enableDebugRequest{enable: true}, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	level, found := log.GetOverridenLogLevel(capturedCtx)
	if !found || level != loggers.DebugLevel {
		t.Errorf("expected debug level override, found=%v level=%v", found, level)
	}
}

func TestDebugLogInterceptor_NoField(t *testing.T) {
	resetGlobals()
	interceptor := DebugLogInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test/NoDebug"}

	var capturedCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) {
		capturedCtx = ctx
		return "ok", nil
	}

	_, err := interceptor(context.Background(), &plainRequest{}, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, found := log.GetOverridenLogLevel(capturedCtx)
	if found {
		t.Error("expected no log level override for request without debug field")
	}
}

func TestDebugLogInterceptor_DebugFalse(t *testing.T) {
	resetGlobals()
	interceptor := DebugLogInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Debug"}

	var capturedCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) {
		capturedCtx = ctx
		return "ok", nil
	}

	_, err := interceptor(context.Background(), &debugRequest{debug: false}, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	_, found := log.GetOverridenLogLevel(capturedCtx)
	if found {
		t.Error("expected no log level override when debug=false")
	}
}

func TestDebugLogInterceptor_Metadata(t *testing.T) {
	resetGlobals()
	interceptor := DebugLogInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test/MetadataDebug"}

	md := grpcmd.New(map[string]string{"x-debug-log-level": "debug"})
	ctx := grpcmd.NewIncomingContext(context.Background(), md)

	var capturedCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) {
		capturedCtx = ctx
		return "ok", nil
	}

	_, err := interceptor(ctx, &plainRequest{}, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	level, found := log.GetOverridenLogLevel(capturedCtx)
	if !found || level != loggers.DebugLevel {
		t.Errorf("expected debug level from metadata, found=%v level=%v", found, level)
	}
}

func TestDebugLogInterceptor_CustomHeaderName(t *testing.T) {
	resetGlobals()
	SetDebugLogHeaderName("X-My-Debug")
	interceptor := DebugLogInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: "/test/CustomHeader"}

	md := grpcmd.New(map[string]string{"x-my-debug": "debug"})
	ctx := grpcmd.NewIncomingContext(context.Background(), md)

	var capturedCtx context.Context
	handler := func(ctx context.Context, req any) (any, error) {
		capturedCtx = ctx
		return "ok", nil
	}

	_, err := interceptor(ctx, &plainRequest{}, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	level, found := log.GetOverridenLogLevel(capturedCtx)
	if !found || level != loggers.DebugLevel {
		t.Errorf("expected debug level from custom header, found=%v level=%v", found, level)
	}
}

func TestDebugLogInterceptor_Disabled(t *testing.T) {
	resetGlobals()
	SetDisableDebugLogInterceptor(true)

	ints := DefaultInterceptors()
	for _, interceptor := range ints {
		info := &grpc.UnaryServerInfo{FullMethod: "/test/Disabled"}
		var capturedCtx context.Context
		handler := func(ctx context.Context, req any) (any, error) {
			capturedCtx = ctx
			return "ok", nil
		}
		_, _ = interceptor(context.Background(), &debugRequest{debug: true}, info, handler)
		if capturedCtx != nil {
			if _, found := log.GetOverridenLogLevel(capturedCtx); found {
				t.Error("expected no debug override when interceptor is disabled")
			}
		}
	}
}

// --- RateLimit interceptor tests ---

type alwaysRejectLimiter struct{}

func (l *alwaysRejectLimiter) Limit(_ context.Context) error {
	return fmt.Errorf("always rejected")
}

func TestRateLimitInterceptor_DefaultInf(t *testing.T) {
	resetGlobals()
	// Default is rate.Inf — no rate limiting, getRateLimiter returns nil
	limiter := getRateLimiter()
	if limiter != nil {
		t.Error("expected nil limiter with default rate.Inf")
	}
}

func TestRateLimitInterceptor_Allowed(t *testing.T) {
	resetGlobals()
	SetDefaultRateLimit(1000, 100)
	info := &grpc.UnaryServerInfo{FullMethod: "/test/RateLimit"}

	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}

	limiter := getRateLimiter()
	if limiter == nil {
		t.Fatal("expected non-nil limiter after SetDefaultRateLimit")
	}

	interceptor := ratelimit_middleware.UnaryServerInterceptor(limiter)
	resp, err := interceptor(context.Background(), nil, info, handler)
	if err != nil {
		t.Fatalf("expected request to pass, got: %v", err)
	}
	if resp != "ok" {
		t.Errorf("expected 'ok', got %v", resp)
	}
}

func TestRateLimitInterceptor_Exceeded(t *testing.T) {
	resetGlobals()
	SetDefaultRateLimit(1, 1) // 1 rps, burst 1
	info := &grpc.UnaryServerInfo{FullMethod: "/test/RateLimit"}

	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}

	limiter := getRateLimiter()
	interceptor := ratelimit_middleware.UnaryServerInterceptor(limiter)

	// First request should pass
	_, err := interceptor(context.Background(), nil, info, handler)
	if err != nil {
		t.Fatalf("first request should pass, got: %v", err)
	}

	// Second request should be rate limited
	_, err = interceptor(context.Background(), nil, info, handler)
	if err == nil {
		t.Fatal("second request should be rate limited")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted, got: %v", err)
	}
}

func TestRateLimitInterceptor_CustomLimiter(t *testing.T) {
	resetGlobals()
	SetRateLimiter(&alwaysRejectLimiter{})
	info := &grpc.UnaryServerInfo{FullMethod: "/test/CustomLimit"}

	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}

	limiter := getRateLimiter()
	interceptor := ratelimit_middleware.UnaryServerInterceptor(limiter)

	_, err := interceptor(context.Background(), nil, info, handler)
	if err == nil {
		t.Fatal("expected rejection from custom limiter")
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.ResourceExhausted {
		t.Errorf("expected ResourceExhausted, got: %v", err)
	}
}

func TestRateLimitInterceptor_Disabled(t *testing.T) {
	resetGlobals()
	SetDefaultRateLimit(1, 1)
	SetDisableRateLimit(true)

	ints := DefaultInterceptors()
	// Verify no ratelimit interceptor in chain by running all interceptors
	// with a handler that should always succeed
	info := &grpc.UnaryServerInfo{FullMethod: "/test/Disabled"}
	handler := func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	}

	// Fire multiple requests through the chain — none should be rate limited
	for i := 0; i < 5; i++ {
		for _, interceptor := range ints {
			_, err := interceptor(context.Background(), nil, info, handler)
			if err != nil {
				st, ok := status.FromError(err)
				if ok && st.Code() == codes.ResourceExhausted {
					t.Fatal("rate limiting should be disabled")
				}
			}
		}
	}
}

// mockServerStream implements grpc.ServerStream for testing stream interceptors.
type mockServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *mockServerStream) Context() context.Context { return s.ctx }

func TestPanicRecoveryStreamInterceptor_NoPanic(t *testing.T) {
	interceptor := PanicRecoveryStreamInterceptor()
	stream := &mockServerStream{ctx: context.Background()}
	info := &grpc.StreamServerInfo{FullMethod: "/test.Svc/Stream"}

	called := false
	err := interceptor(nil, stream, info, func(_ any, _ grpc.ServerStream) error {
		called = true
		return nil
	})
	if !called {
		t.Fatal("handler should have been called")
	}
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestPanicRecoveryStreamInterceptor_Panic(t *testing.T) {
	interceptor := PanicRecoveryStreamInterceptor()
	stream := &mockServerStream{ctx: context.Background()}
	info := &grpc.StreamServerInfo{FullMethod: "/test.Svc/Stream"}

	err := interceptor(nil, stream, info, func(_ any, _ grpc.ServerStream) error {
		panic("test panic")
	})
	if err == nil {
		t.Fatal("expected error from panic recovery")
	}
	if err.Error() != "panic: test panic" {
		t.Fatalf("expected 'panic: test panic', got %q", err.Error())
	}
}

func TestPanicRecoveryStreamInterceptor_PanicWithError(t *testing.T) {
	interceptor := PanicRecoveryStreamInterceptor()
	stream := &mockServerStream{ctx: context.Background()}
	info := &grpc.StreamServerInfo{FullMethod: "/test.Svc/Stream"}
	origErr := errors.New("original error")

	err := interceptor(nil, stream, info, func(_ any, _ grpc.ServerStream) error {
		panic(origErr)
	})
	if err != origErr {
		t.Fatalf("expected original error, got %v", err)
	}
}

func TestServerErrorStreamInterceptor_ContextWrapped(t *testing.T) {
	resetGlobals()
	interceptor := ServerErrorStreamInterceptor()
	stream := &mockServerStream{ctx: context.Background()}
	info := &grpc.StreamServerInfo{FullMethod: "/test.Svc/Stream"}

	var handlerCtx context.Context
	_ = interceptor(nil, stream, info, func(_ any, s grpc.ServerStream) error {
		handlerCtx = s.Context()
		return nil
	})
	// The handler should receive a wrapped stream with trace ID set
	if handlerCtx == nil {
		t.Fatal("handler context should not be nil")
	}
	// Context should differ from original (trace ID was added)
	if handlerCtx == context.Background() {
		t.Fatal("handler should receive a wrapped context, not the original")
	}
}

func TestServerErrorStreamInterceptor_Error(t *testing.T) {
	resetGlobals()
	interceptor := ServerErrorStreamInterceptor()
	stream := &mockServerStream{ctx: context.Background()}
	info := &grpc.StreamServerInfo{FullMethod: "/test.Svc/Stream"}
	testErr := errors.New("stream error")

	err := interceptor(nil, stream, info, func(_ any, _ grpc.ServerStream) error {
		return testErr
	})
	if err != testErr {
		t.Fatalf("expected test error, got %v", err)
	}
}

func TestDefaultStreamInterceptors_IncludesPanicRecovery(t *testing.T) {
	resetGlobals()
	ints := DefaultStreamInterceptors()
	// Verify the chain handles panics by running a panicking handler
	chain := ints[len(ints)-1] // PanicRecoveryStreamInterceptor is last
	stream := &mockServerStream{ctx: context.Background()}
	info := &grpc.StreamServerInfo{FullMethod: "/test.Svc/Stream"}

	err := chain(nil, stream, info, func(_ any, _ grpc.ServerStream) error {
		panic("chain panic test")
	})
	if err == nil {
		t.Fatal("expected error from panic recovery in default stream chain")
	}
}

// --- Interceptor ordering contract tests ---
//
// These tests guard the layering described on DefaultInterceptors /
// DefaultStreamInterceptors in server.go. They fail if a position constant is
// reordered or a slot is wired to the wrong interceptor — any such change
// silently alters observable server semantics (panic recovery coverage,
// validation-error reporting, deadline application).

type traceIDRequest struct{ id string }

func (r *traceIDRequest) GetTraceId() string { return r.id }

func TestInterceptorPositionConstants(t *testing.T) {
	// Unary: required relative ordering encoding the contract.
	// See ordering contract godoc in server.go.
	if unaryPosTimeout != 0 {
		t.Errorf("unaryPosTimeout must be outermost (0); got %d", unaryPosTimeout)
	}
	if unaryPosPanicRecovery != unaryPosCount-1 {
		t.Errorf("unaryPosPanicRecovery must be innermost (unaryPosCount-1=%d); got %d",
			unaryPosCount-1, unaryPosPanicRecovery)
	}
	unaryOrder := []struct {
		name string
		pos  int
	}{
		{"Timeout", unaryPosTimeout},
		{"RateLimit", unaryPosRateLimit},
		{"ResponseTimeLog", unaryPosResponseTimeLog},
		{"TraceID", unaryPosTraceID},
		{"DebugLog", unaryPosDebugLog},
		{"ProtoValidate", unaryPosProtoValidate},
		{"Metrics", unaryPosMetrics},
		{"ServerError", unaryPosServerError},
		{"NewRelic", unaryPosNewRelic},
		{"PanicRecovery", unaryPosPanicRecovery},
	}
	for i := 1; i < len(unaryOrder); i++ {
		if unaryOrder[i-1].pos >= unaryOrder[i].pos {
			t.Errorf("unary position order violation: %s (%d) must precede %s (%d)",
				unaryOrder[i-1].name, unaryOrder[i-1].pos,
				unaryOrder[i].name, unaryOrder[i].pos)
		}
	}

	// Critical semantic invariants: panic recovery must be INNER to
	// metrics, error-reporting, and tracing so those layers observe the
	// synthesized error from a recovered handler panic.
	if unaryPosPanicRecovery <= unaryPosServerError {
		t.Error("panic recovery must be INNER to ServerErrorInterceptor")
	}
	if unaryPosPanicRecovery <= unaryPosMetrics {
		t.Error("panic recovery must be INNER to metrics")
	}
	if unaryPosPanicRecovery <= unaryPosNewRelic {
		t.Error("panic recovery must be INNER to NewRelic")
	}
	// Protovalidate sits OUTER to metrics / error-reporting so that
	// obviously-invalid requests short-circuit with InvalidArgument before
	// any metrics or error-reporting work runs.
	if unaryPosProtoValidate >= unaryPosMetrics {
		t.Error("protovalidate must be OUTER to metrics (short-circuits before metrics work runs)")
	}
	if unaryPosProtoValidate >= unaryPosServerError {
		t.Error("protovalidate must be OUTER to ServerErrorInterceptor (short-circuits before error-reporting runs)")
	}

	// Stream variants.
	if streamPosRateLimit != 0 {
		t.Errorf("streamPosRateLimit must be outermost (0); got %d", streamPosRateLimit)
	}
	if streamPosPanicRecovery != streamPosCount-1 {
		t.Errorf("streamPosPanicRecovery must be innermost (streamPosCount-1=%d); got %d",
			streamPosCount-1, streamPosPanicRecovery)
	}
	streamOrder := []struct {
		name string
		pos  int
	}{
		{"RateLimit", streamPosRateLimit},
		{"ResponseTimeLog", streamPosResponseTimeLog},
		{"ProtoValidate", streamPosProtoValidate},
		{"Metrics", streamPosMetrics},
		{"ServerError", streamPosServerError},
		{"PanicRecovery", streamPosPanicRecovery},
	}
	for i := 1; i < len(streamOrder); i++ {
		if streamOrder[i-1].pos >= streamOrder[i].pos {
			t.Errorf("stream position order violation: %s (%d) must precede %s (%d)",
				streamOrder[i-1].name, streamOrder[i-1].pos,
				streamOrder[i].name, streamOrder[i].pos)
		}
	}
	if streamPosPanicRecovery <= streamPosServerError {
		t.Error("stream panic recovery must be INNER to ServerErrorStreamInterceptor")
	}
	if streamPosProtoValidate >= streamPosMetrics {
		t.Error("stream protovalidate must be OUTER to metrics (short-circuits before metrics work runs)")
	}
}

func TestDefaultInterceptors_AllSlotsPopulated(t *testing.T) {
	resetGlobals()
	defer resetGlobals()

	// Enable the rate limit slot (default config leaves the limiter nil).
	SetDefaultRateLimit(1000, 1000)

	ints := DefaultInterceptors()
	if len(ints) != unaryPosCount {
		t.Fatalf("expected %d interceptors, got %d", unaryPosCount, len(ints))
	}
}

func TestDefaultStreamInterceptors_AllSlotsPopulated(t *testing.T) {
	resetGlobals()
	defer resetGlobals()
	SetDefaultRateLimit(1000, 1000)

	ints := DefaultStreamInterceptors()
	if len(ints) != streamPosCount {
		t.Fatalf("expected %d stream interceptors, got %d", streamPosCount, len(ints))
	}
}

// TestDefaultInterceptors_SlotWiring verifies that each named position holds
// the expected interceptor by probing its characteristic side effect. Combined
// with TestInterceptorPositionConstants, this enforces both the positional
// layering and the slot-to-interceptor mapping.
func TestDefaultInterceptors_SlotWiring(t *testing.T) {
	resetGlobals()
	defer resetGlobals()
	SetDefaultRateLimit(1000, 1000)

	ints := DefaultInterceptors()
	if len(ints) != unaryPosCount {
		t.Fatalf("expected %d interceptors, got %d", unaryPosCount, len(ints))
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Svc/Method"}

	t.Run("Timeout", func(t *testing.T) {
		gotDeadline := false
		_, err := ints[unaryPosTimeout](context.Background(), nil, info,
			func(ctx context.Context, _ any) (any, error) {
				_, gotDeadline = ctx.Deadline()
				return "ok", nil
			})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !gotDeadline {
			t.Errorf("slot unaryPosTimeout (%d) should set a deadline", unaryPosTimeout)
		}
	})

	t.Run("ResponseTimeLog", func(t *testing.T) {
		var grpcMethod any
		var found bool
		_, err := ints[unaryPosResponseTimeLog](context.Background(), nil, info,
			func(ctx context.Context, _ any) (any, error) {
				grpcMethod, found = loggers.FromContext(ctx).Load("grpcMethod")
				return "ok", nil
			})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found || grpcMethod != "/test.Svc/Method" {
			t.Errorf("slot unaryPosResponseTimeLog (%d) should add grpcMethod to log context; found=%v val=%v",
				unaryPosResponseTimeLog, found, grpcMethod)
		}
	})

	t.Run("TraceID", func(t *testing.T) {
		var observed string
		_, err := ints[unaryPosTraceID](context.Background(), &traceIDRequest{id: "trace-xyz"}, info,
			func(ctx context.Context, _ any) (any, error) {
				observed = notifier.GetTraceId(ctx)
				return "ok", nil
			})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if observed != "trace-xyz" {
			t.Errorf("slot unaryPosTraceID (%d) should extract trace id from request; got %q",
				unaryPosTraceID, observed)
		}
	})

	t.Run("DebugLog", func(t *testing.T) {
		var level loggers.Level
		var found bool
		_, err := ints[unaryPosDebugLog](context.Background(), &debugRequest{debug: true}, info,
			func(ctx context.Context, _ any) (any, error) {
				level, found = log.GetOverridenLogLevel(ctx)
				return "ok", nil
			})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !found || level != loggers.DebugLevel {
			t.Errorf("slot unaryPosDebugLog (%d) should override log level on GetDebug()=true; found=%v level=%v",
				unaryPosDebugLog, found, level)
		}
	})

	t.Run("PanicRecovery", func(t *testing.T) {
		_, err := ints[unaryPosPanicRecovery](context.Background(), nil, info,
			func(_ context.Context, _ any) (any, error) {
				panic("slot panic")
			})
		if err == nil {
			t.Errorf("slot unaryPosPanicRecovery (%d) should convert handler panic to error",
				unaryPosPanicRecovery)
		}
	})

	t.Run("ServerError_PanicPropagates", func(t *testing.T) {
		// ServerErrorInterceptor must NOT recover panics — if it did, a
		// misplaced recovery would silently swallow errors and break the
		// contract that only unaryPosPanicRecovery recovers.
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("slot unaryPosServerError (%d) must NOT recover panics", unaryPosServerError)
			}
		}()
		_, _ = ints[unaryPosServerError](context.Background(), nil, info,
			func(_ context.Context, _ any) (any, error) {
				panic("should propagate past ServerErrorInterceptor")
			})
	})
}

// TestDefaultInterceptors_PanicThroughFullChain runs the full default unary
// chain with a panicking handler and asserts (a) the chain recovers and
// returns an error, and (b) outer layers (user interceptor registered via
// AddUnaryServerInterceptor) observe that error. This validates end-to-end
// that panic-recovery is wired into the chain and is innermost relative to
// user interceptors.
func TestDefaultInterceptors_PanicThroughFullChain(t *testing.T) {
	resetGlobals()
	defer resetGlobals()

	// protovalidate sits outer to panic recovery and rejects nil requests
	// with InvalidArgument; without disabling it, the handler never runs
	// and the test would pass on the validation error instead of the
	// recovered panic.
	SetDisableProtoValidate(true)

	var userSawErr error
	AddUnaryServerInterceptor(context.Background(),
		func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			resp, err := handler(ctx, req)
			userSawErr = err
			return resp, err
		})

	chain := chainUnaryServer(DefaultInterceptors())
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Svc/Panic"}

	resp, err := chain(context.Background(), nil, info,
		func(_ context.Context, _ any) (any, error) {
			panic("boom")
		})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("chain should surface the recovered panic (want error containing 'boom'); got %v", err)
	}
	if resp != nil {
		t.Errorf("chain should return nil resp on panic, got %v", resp)
	}
	if userSawErr == nil || !strings.Contains(userSawErr.Error(), "boom") {
		t.Errorf("user interceptor (outermost) should observe the recovered panic error; got %v", userSawErr)
	}
}

// TestDefaultInterceptors_UserInterceptorsOutermost verifies that interceptors
// registered via AddUnaryServerInterceptor run BEFORE the CB set — i.e., they
// see a context without the default timeout deadline applied.
func TestDefaultInterceptors_UserInterceptorsOutermost(t *testing.T) {
	resetGlobals()
	defer resetGlobals()

	// Disable protovalidate; it rejects nil requests which would short-circuit
	// the chain before it reaches the handler. We only care about ordering here.
	SetDisableProtoValidate(true)

	var userSawDeadline bool
	AddUnaryServerInterceptor(context.Background(),
		func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			_, userSawDeadline = ctx.Deadline()
			return handler(ctx, req)
		})

	chain := chainUnaryServer(DefaultInterceptors())
	info := &grpc.UnaryServerInfo{FullMethod: "/test.Svc/Method"}

	_, err := chain(context.Background(), nil, info,
		func(_ context.Context, _ any) (any, error) { return "ok", nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if userSawDeadline {
		t.Error("user interceptor should run before DefaultTimeoutInterceptor and see no deadline")
	}
}

func TestDefaultStreamInterceptors_SlotWiring(t *testing.T) {
	resetGlobals()
	defer resetGlobals()
	SetDefaultRateLimit(1000, 1000)

	ints := DefaultStreamInterceptors()
	if len(ints) != streamPosCount {
		t.Fatalf("expected %d stream interceptors, got %d", streamPosCount, len(ints))
	}
	info := &grpc.StreamServerInfo{FullMethod: "/test.Svc/Stream"}
	stream := &mockServerStream{ctx: context.Background()}

	t.Run("PanicRecovery", func(t *testing.T) {
		err := ints[streamPosPanicRecovery](nil, stream, info,
			func(_ any, _ grpc.ServerStream) error {
				panic("stream slot panic")
			})
		if err == nil {
			t.Errorf("slot streamPosPanicRecovery (%d) should convert handler panic to error",
				streamPosPanicRecovery)
		}
	})

	t.Run("ServerError_PanicPropagates", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("slot streamPosServerError (%d) must NOT recover panics", streamPosServerError)
			}
		}()
		_ = ints[streamPosServerError](nil, stream, info,
			func(_ any, _ grpc.ServerStream) error {
				panic("should propagate past ServerErrorStreamInterceptor")
			})
	})
}

// TestDefaultStreamInterceptors_UserInterceptorsOutermost — user stream
// interceptors registered via AddStreamServerInterceptor run before the CB set.
func TestDefaultStreamInterceptors_UserInterceptorsOutermost(t *testing.T) {
	resetGlobals()
	defer resetGlobals()

	var order []string
	AddStreamServerInterceptor(context.Background(),
		func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			order = append(order, "user")
			return handler(srv, ss)
		})

	ints := DefaultStreamInterceptors()
	// The user interceptor must be the first element of the slice.
	if len(ints) == 0 {
		t.Fatal("no interceptors returned")
	}
	info := &grpc.StreamServerInfo{FullMethod: "/test.Svc/Stream"}
	stream := &mockServerStream{ctx: context.Background()}

	// Run the outermost interceptor directly and verify the user interceptor
	// is hit first.
	_ = ints[0](nil, stream, info, func(_ any, _ grpc.ServerStream) error {
		order = append(order, "handler")
		return nil
	})
	if len(order) == 0 || order[0] != "user" {
		t.Errorf("user stream interceptor should be outermost (first); order=%v", order)
	}
}

// --- NRHttpTracer tests ---

// newTestNRApp builds a disabled New Relic application suitable for tests —
// no network calls, but Application / Transaction objects behave normally
// including request-context propagation.
func newTestNRApp(t *testing.T) *newrelic.Application {
	t.Helper()
	app, err := newrelic.NewApplication(
		newrelic.ConfigAppName("test"),
		newrelic.ConfigLicense(strings.Repeat("x", 40)),
		newrelic.ConfigEnabled(false),
	)
	if err != nil {
		t.Fatalf("newrelic.NewApplication: %v", err)
	}
	return app
}

func TestNRHttpTracer_NilApp(t *testing.T) {
	resetGlobals()
	defer resetGlobals()

	nrutil.SetNewRelicApp(nil)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })

	p, got := NRHttpTracer("/route", h)
	if p != "/route" {
		t.Errorf("pattern should be unchanged when NR app is nil; got %q", p)
	}
	// With no NR app, the function must return the original handler reference.
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(h).Pointer() {
		t.Error("expected original handler to be returned unchanged when NR app is nil")
	}
}

// TestNRHttpTracer_FilterFuncAppliedForNonEmptyPattern guards issue #42:
// when the pattern is non-empty, the configured filterFunc must still gate
// NR instrumentation so filtered routes run the original handler without
// starting a transaction.
func TestNRHttpTracer_FilterFuncAppliedForNonEmptyPattern(t *testing.T) {
	resetGlobals()
	defer resetGlobals()

	nrutil.SetNewRelicApp(newTestNRApp(t))
	defer nrutil.SetNewRelicApp(nil)

	SetFilterFunc(context.Background(), func(_ context.Context, path string) bool {
		return path != "/skip"
	})

	var sawTxn bool
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawTxn = newrelic.FromContext(r.Context()) != nil
	})

	_, wrapped := NRHttpTracer("/route", h)

	sawTxn = false
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/skip", nil))
	if sawTxn {
		t.Error("filtered path should run without a New Relic transaction in request context")
	}

	sawTxn = false
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/keep", nil))
	if !sawTxn {
		t.Error("non-filtered path should run with a New Relic transaction in request context")
	}
}

// TestNRHttpTracer_FilterFuncAppliedForEmptyPattern exercises the other
// branch so a future regression in either path is caught.
func TestNRHttpTracer_FilterFuncAppliedForEmptyPattern(t *testing.T) {
	resetGlobals()
	defer resetGlobals()

	nrutil.SetNewRelicApp(newTestNRApp(t))
	defer nrutil.SetNewRelicApp(nil)

	SetFilterFunc(context.Background(), func(_ context.Context, path string) bool {
		return path != "/skip"
	})

	var sawTxn bool
	h := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		sawTxn = newrelic.FromContext(r.Context()) != nil
	})

	_, wrapped := NRHttpTracer("", h)

	sawTxn = false
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/skip", nil))
	if sawTxn {
		t.Error("filtered path should run without a New Relic transaction in request context")
	}

	sawTxn = false
	wrapped.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/keep", nil))
	if !sawTxn {
		t.Error("non-filtered path should run with a New Relic transaction in request context")
	}
}
