package interceptors

import (
	"context"

	"github.com/go-coldbrew/options"
	"google.golang.org/grpc"
)

type clientOption interface {
	grpc.CallOption
	process(*clientOptions)
}

type clientOptions struct {
	hystrixName    string
	disableHystrix bool
}

type optionCarrier struct {
	grpc.EmptyCallOption
	processor func(*clientOptions)
}

func (h *optionCarrier) process(co *clientOptions) {
	h.processor(co)
}

//WithHystrixName changes the hystrix name to be used in the client interceptors
func WithHystrixName(name string) clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			if name != "" {
				co.hystrixName = name
			}
		},
	}
}

// WithoutHystrix disables hystrix
func WithoutHystrix() clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.disableHystrix = true
		},
	}
}

// WithHystrix enables hystrix
func WithHystrix() clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.disableHystrix = false
		},
	}
}

const (
	doNotNotifyKey = "interceptor-do-not-notify"
)

// IgnoreErrorNotification defines if Coldbrew should send this error to notifier or not
// this should be called from within the service code
func IgnoreErrorNotification(ctx context.Context, ignore bool) context.Context {
	return options.AddToOptions(ctx, doNotNotifyKey, ignore)
}

func shouldNotify(ctx context.Context) bool {
	opt := options.FromContext(ctx)
	if opt != nil {
		if val, ok := opt.Get(doNotNotifyKey); ok && val != nil {
			if ignore, ok := val.(bool); ok {
				return ignore
			}
		}
	}
	return true
}
