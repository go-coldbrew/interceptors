package interceptors

import "google.golang.org/grpc"

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
