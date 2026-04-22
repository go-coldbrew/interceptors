package interceptors

import (
	"context"
	stdError "errors"
	"sync"

	"github.com/go-coldbrew/log"
	grpcprom "github.com/grpc-ecosystem/go-grpc-middleware/providers/prometheus"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	srvMetricsOnce sync.Once
	srvMetrics     *grpcprom.ServerMetrics
	cltMetricsOnce sync.Once
	cltMetrics     *grpcprom.ClientMetrics
)

// registerOrReuse registers c with the default Prometheus registry and
// returns the collector that is actually registered. If a collector with
// the same fully-qualified name is already registered, returns that
// existing collector so its accumulated observations are preserved —
// callers should swap to the returned collector rather than use the
// argument, which would otherwise be dropped. On any other registration
// error, logs and returns c so callers still get a usable instance (its
// observations will not be exported).
func registerOrReuse(c prometheus.Collector) prometheus.Collector {
	err := prometheus.Register(c)
	if err == nil {
		return c
	}
	var are prometheus.AlreadyRegisteredError
	if stdError.As(err, &are) {
		return are.ExistingCollector
	}
	log.Error(context.Background(), "msg", "gRPC Prometheus metrics registration failed. If you are using github.com/go-coldbrew/core, it may need to be updated to the latest version.", "err", err)
	return c
}

func getServerMetrics() *grpcprom.ServerMetrics {
	srvMetricsOnce.Do(func() {
		m := grpcprom.NewServerMetrics(defaultConfig.srvMetricsOpts...)
		reused := registerOrReuse(m)
		if typed, ok := reused.(*grpcprom.ServerMetrics); ok {
			srvMetrics = typed
			return
		}
		log.Warn(context.Background(), "msg", "existing gRPC server metrics collector has an unexpected type; using the new (unregistered) instance", "existing", reused)
		srvMetrics = m
	})
	return srvMetrics
}

func getClientMetrics() *grpcprom.ClientMetrics {
	cltMetricsOnce.Do(func() {
		m := grpcprom.NewClientMetrics(defaultConfig.cltMetricsOpts...)
		reused := registerOrReuse(m)
		if typed, ok := reused.(*grpcprom.ClientMetrics); ok {
			cltMetrics = typed
			return
		}
		log.Warn(context.Background(), "msg", "existing gRPC client metrics collector has an unexpected type; using the new (unregistered) instance", "existing", reused)
		cltMetrics = m
	})
	return cltMetrics
}
