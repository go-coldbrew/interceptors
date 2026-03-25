package interceptors

import (
	"context"
	"testing"

	"google.golang.org/grpc"
)

func TestChainUnaryServerEmpty(t *testing.T) {
	chain := chainUnaryServer(nil)
	resp, err := chain(context.Background(), "req", &grpc.UnaryServerInfo{}, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	})
	if err != nil || resp != "ok" {
		t.Errorf("expected (ok, nil), got (%v, %v)", resp, err)
	}
}

func TestChainUnaryServerSingle(t *testing.T) {
	called := false
	interceptor := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		called = true
		return handler(ctx, req)
	}
	chain := chainUnaryServer([]grpc.UnaryServerInterceptor{interceptor})
	_, _ = chain(context.Background(), "req", &grpc.UnaryServerInfo{}, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	})
	if !called {
		t.Error("expected interceptor to be called")
	}
}

func TestChainUnaryServerOrder(t *testing.T) {
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
	_, _ = chain(context.Background(), "req", &grpc.UnaryServerInfo{}, func(ctx context.Context, req interface{}) (interface{}, error) {
		return nil, nil
	})
	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Errorf("expected execution order [1 2 3], got %v", order)
	}
}

func TestChainUnaryServerNilFiltered(t *testing.T) {
	called := false
	interceptor := func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		called = true
		return handler(ctx, req)
	}
	chain := chainUnaryServer([]grpc.UnaryServerInterceptor{nil, interceptor, nil})
	_, _ = chain(context.Background(), "req", &grpc.UnaryServerInfo{}, func(ctx context.Context, req interface{}) (interface{}, error) {
		return "ok", nil
	})
	if !called {
		t.Error("expected non-nil interceptor to be called")
	}
}

func TestChainUnaryClientOrder(t *testing.T) {
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
	})
	_ = chain(context.Background(), "/test", nil, nil, nil, func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		return nil
	})
	if len(order) != 2 || order[0] != 1 || order[1] != 2 {
		t.Errorf("expected execution order [1 2], got %v", order)
	}
}

func TestChainUnaryClientNilFiltered(t *testing.T) {
	chain := chainUnaryClient([]grpc.UnaryClientInterceptor{nil, nil})
	invokerCalled := false
	err := chain(context.Background(), "/test", nil, nil, nil, func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
		invokerCalled = true
		return nil
	})
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if !invokerCalled {
		t.Error("expected invoker to be called when all interceptors are nil")
	}
}

func TestChainStreamClientNilFiltered(t *testing.T) {
	chain := chainStreamClient([]grpc.StreamClientInterceptor{nil})
	called := false
	_, _ = chain(context.Background(), nil, nil, "/test", func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		called = true
		return nil, nil
	})
	if !called {
		t.Error("expected streamer to be called directly when all interceptors are nil")
	}
}
