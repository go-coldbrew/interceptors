package interceptors

import (
	"context"
	"fmt"
	"sync"

	ratelimit_middleware "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/ratelimit"
	"golang.org/x/time/rate"
)

var (
	rateLimiterOnce sync.Once
	rateLimiterVal  ratelimit_middleware.Limiter
)

// tokenBucketLimiter wraps golang.org/x/time/rate.Limiter to implement
// the ratelimit.Limiter interface.
type tokenBucketLimiter struct {
	limiter *rate.Limiter
}

func (l *tokenBucketLimiter) Limit(_ context.Context) error {
	if !l.limiter.Allow() {
		return fmt.Errorf("rate limit exceeded")
	}
	return nil
}

func getRateLimiter() ratelimit_middleware.Limiter {
	rateLimiterOnce.Do(func() {
		if defaultConfig.rateLimiter != nil {
			rateLimiterVal = defaultConfig.rateLimiter
			return
		}
		if defaultConfig.defaultRateLimit == rate.Inf {
			rateLimiterVal = nil
			return
		}
		rateLimiterVal = &tokenBucketLimiter{
			limiter: rate.NewLimiter(defaultConfig.defaultRateLimit, defaultConfig.defaultRateBurst),
		}
	})
	return rateLimiterVal
}
