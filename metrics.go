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

func registerCollector(c prometheus.Collector) {
	if err := prometheus.Register(c); err != nil {
		var are prometheus.AlreadyRegisteredError
		if stdError.As(err, &are) {
			prometheus.Unregister(are.ExistingCollector)
			if err := prometheus.Register(c); err != nil {
				log.Warn(context.Background(), "msg", "failed to re-register gRPC metrics with Prometheus", "err", err)
			}
			return
		}
		log.Error(context.Background(), "msg", "gRPC Prometheus metrics registration failed. If you are using github.com/go-coldbrew/core, it may need to be updated to the latest version.", "err", err)
	}
}

func getServerMetrics() *grpcprom.ServerMetrics {
	srvMetricsOnce.Do(func() {
		srvMetrics = grpcprom.NewServerMetrics(defaultConfig.srvMetricsOpts...)
		registerCollector(srvMetrics)
	})
	return srvMetrics
}

func getClientMetrics() *grpcprom.ClientMetrics {
	cltMetricsOnce.Do(func() {
		cltMetrics = grpcprom.NewClientMetrics(defaultConfig.cltMetricsOpts...)
		registerCollector(cltMetrics)
	})
	return cltMetrics
}
