package examples_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/bulkhead"
	"github.com/failsafe-go/failsafe-go/circuitbreaker"
	"github.com/go-coldbrew/interceptors"
)

// ExampleSetDefaultExecutor demonstrates setting up a simple global circuit
// breaker using failsafe-go. This is the most common usage pattern — one
// circuit breaker shared across all gRPC methods.
func ExampleSetDefaultExecutor() {
	cb := circuitbreaker.NewBuilder[any]().
		WithFailureThreshold(5).
		WithDelay(5 * time.Second).
		WithSuccessThreshold(2).
		Build()

	interceptors.SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
		return failsafe.With[any](cb).WithContext(ctx).Run(func() error {
			return fn(ctx)
		})
	})

	fmt.Println("global circuit breaker configured")
	// Output: global circuit breaker configured
}

// ExampleSetDefaultExecutor_perMethod demonstrates per-method circuit breakers.
// Each gRPC method gets its own circuit breaker, so failures in one method
// don't trip the breaker for another.
func ExampleSetDefaultExecutor_perMethod() {
	var (
		mu       sync.Mutex
		breakers = make(map[string]circuitbreaker.CircuitBreaker[any])
	)

	newBreaker := func() circuitbreaker.CircuitBreaker[any] {
		return circuitbreaker.NewBuilder[any]().
			WithFailureThreshold(5).
			WithDelay(5 * time.Second).
			Build()
	}

	interceptors.SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
		mu.Lock()
		cb, ok := breakers[method]
		if !ok {
			cb = newBreaker()
			breakers[method] = cb
		}
		mu.Unlock()

		return failsafe.With[any](cb).WithContext(ctx).Run(func() error {
			return fn(ctx)
		})
	})

	fmt.Println("per-method circuit breakers configured")
	// Output: per-method circuit breakers configured
}

// ExampleSetDefaultExecutor_bulkhead demonstrates composing a circuit breaker
// with a bulkhead (concurrency limiter) using failsafe-go.
func ExampleSetDefaultExecutor_bulkhead() {
	cb := circuitbreaker.NewBuilder[any]().
		WithFailureThreshold(5).
		WithDelay(5 * time.Second).
		Build()

	bh := bulkhead.New[any](200)

	// Policies execute right-to-left: bulkhead limits concurrency,
	// circuit breaker wraps the result.
	interceptors.SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
		return failsafe.With[any](cb, bh).WithContext(ctx).Run(func() error {
			return fn(ctx)
		})
	})

	fmt.Println("circuit breaker + bulkhead configured")
	// Output: circuit breaker + bulkhead configured
}

// ExampleWithoutExecutor demonstrates disabling the executor for specific RPCs.
// This is useful for health checks or internal loopback connections that should
// not be circuit-broken.
func ExampleWithoutExecutor() {
	_ = interceptors.WithoutExecutor()
	fmt.Println("executor disabled for this call")
	// Output: executor disabled for this call
}

// ExampleWithExecutor demonstrates setting a custom per-service executor
// with different circuit breaker tuning.
func ExampleWithExecutor() {
	paymentCB := circuitbreaker.NewBuilder[any]().
		WithFailureThreshold(3).     // more sensitive
		WithDelay(10 * time.Second). // longer recovery
		Build()

	_ = interceptors.WithExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
		return failsafe.With[any](paymentCB).WithContext(ctx).Run(func() error {
			return fn(ctx)
		})
	})

	fmt.Println("per-service executor configured")
	// Output: per-service executor configured
}
