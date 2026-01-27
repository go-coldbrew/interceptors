package interceptors

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

type clientOption interface {
	grpc.CallOption
	process(*clientOptions)
}

type clientOptions struct {
	hystrixName    string
	disableHystrix bool
	excludedErrors []error
	excludedCodes  []codes.Code
}

type optionCarrier struct {
	grpc.EmptyCallOption
	processor func(*clientOptions)
}

func (h *optionCarrier) process(co *clientOptions) {
	h.processor(co)
}

// WithHystrixName creates a clientOption that sets the Hystrix command name used by client interceptors.
// If name is empty, the existing Hystrix name is left unchanged.
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

// WithHystrixExcludedErrors returns a clientOption that adds the provided errors to the list of errors
// excluded from the Hystrix circuit breaker.
func WithHystrixExcludedErrors(errors ...error) clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.excludedErrors = append(co.excludedErrors, errors...)
		},
	}
}

// WithHystrixExcludedCodes returns a clientOption that appends the provided gRPC codes to the list of codes excluded from the Hystrix circuit breaker.
func WithHystrixExcludedCodes(codes ...codes.Code) clientOption {
	return &optionCarrier{
		processor: func(co *clientOptions) {
			co.excludedCodes = append(co.excludedCodes, codes...)
		},
	}
}
