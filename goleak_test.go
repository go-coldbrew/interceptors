package interceptors

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// rollbar-go creates a global async client at package init time
		// (rollbar.go:39: std = NewAsync(...)), starting a background goroutine
		// unconditionally when the package is imported. Cannot be avoided.
		goleak.IgnoreTopFunction("github.com/rollbar/rollbar-go.NewAsyncTransport.func1"),
		// hystrix-go starts metric exchange and pool metric goroutines when a
		// circuit breaker is first used in tests. These are global singletons
		// with no cleanup API.
		goleak.IgnoreTopFunction("github.com/afex/hystrix-go/hystrix.(*metricExchange).Monitor"),
		goleak.IgnoreTopFunction("github.com/afex/hystrix-go/hystrix.(*poolMetrics).Monitor"),
	)
}
