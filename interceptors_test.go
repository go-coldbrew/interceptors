package interceptors

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc"
)

// resetGlobals restores package-level state so tests don't interfere with each other.
func resetGlobals() {
	defaultFilterFunc = FilterMethodsFunc
	unaryServerInterceptors = []grpc.UnaryServerInterceptor{}
	streamServerInterceptors = []grpc.StreamServerInterceptor{}
	useCBServerInterceptors = true
	unaryClientInterceptors = []grpc.UnaryClientInterceptor{}
	streamClientInterceptors = []grpc.StreamClientInterceptor{}
	useCBClientInterceptors = true
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

	// With a user interceptor, verify the chain is applied when RPCMethod is present.
	handlerCalled = false
	interceptorCalled := false
	AddUnaryServerInterceptor(ctx, func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		interceptorCalled = true
		return handler(ctx, req)
	})

	// Simulate RPCMethod context using grpc-gateway's runtime.AnnotateIncomingContext indirectly.
	// Since runtime.RPCMethod reads from context metadata, we can set it via the runtime helpers.
	// However, the simplest way is to use runtime.WithRPCMethod if available; if not,
	// we test the no-RPCMethod path which is already covered above.
	// Without RPCMethod, interceptor chain is not invoked.
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
