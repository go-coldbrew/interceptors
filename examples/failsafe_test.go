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

// ExampleSetDefaultExecutor demonstrates setting up a circuit breaker for
// specific gRPC methods using failsafe-go. The executor receives the method
// name, so you can filter which methods get circuit breaking.
func ExampleSetDefaultExecutor() {
	cb := circuitbreaker.NewBuilder[any]().
		WithFailureThreshold(5).
		WithDelay(5 * time.Second).
		WithSuccessThreshold(2).
		Build()

	// Only apply circuit breaking to specific methods
	protected := map[string]bool{
		"/payment.Service/Charge": true,
		"/payment.Service/Refund": true,
	}

	interceptors.SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
		if !protected[method] {
			return fn(ctx) // passthrough for non-protected methods
		}
		return failsafe.With[any](cb).WithContext(ctx).Run(func() error {
			return fn(ctx)
		})
	})

	fmt.Println("method-filtered circuit breaker configured")
	// Output: method-filtered circuit breaker configured
}

// ExampleSetDefaultExecutor_perMethod demonstrates per-method circuit breakers
// with different limits. Each method gets its own circuit breaker with
// tuning appropriate for that method's characteristics.
func ExampleSetDefaultExecutor_perMethod() {
	type cbConfig struct {
		failureThreshold uint
		delay            time.Duration
	}

	// Different limits per method
	configs := map[string]cbConfig{
		"/payment.Service/Charge": {failureThreshold: 3, delay: 10 * time.Second}, // sensitive — trip fast, recover slow
		"/payment.Service/Refund": {failureThreshold: 3, delay: 10 * time.Second},
		"/user.Service/GetUser":   {failureThreshold: 10, delay: 5 * time.Second}, // tolerant — allow more failures
		"/feed.Service/GetFeed":   {failureThreshold: 10, delay: 5 * time.Second},
	}

	var (
		mu       sync.Mutex
		breakers = make(map[string]circuitbreaker.CircuitBreaker[any])
	)

	interceptors.SetDefaultExecutor(func(ctx context.Context, method string, fn func(ctx context.Context) error) error {
		cfg, ok := configs[method]
		if !ok {
			return fn(ctx) // no circuit breaker for unconfigured methods
		}

		mu.Lock()
		cb, exists := breakers[method]
		if !exists {
			cb = circuitbreaker.NewBuilder[any]().
				WithFailureThreshold(cfg.failureThreshold).
				WithDelay(cfg.delay).
				Build()
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
