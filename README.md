<!-- Code generated by gomarkdoc. DO NOT EDIT -->

[![GoDoc](https://img.shields.io/badge/pkg.go.dev-doc-blue)](http://pkg.go.dev/github.com/go-coldbrew/interceptors)
[![Go Report Card](https://goreportcard.com/badge/github.com/go-coldbrew/interceptors)](https://goreportcard.com/report/github.com/go-coldbrew/interceptors)

# interceptors

```go
import "github.com/go-coldbrew/interceptors"
```

### Package interceptors provides a common set of interceptors which are used in Coldbrew

Almost all of these interceptors are reusable and can be used in any other project using grpc\.

## Index

- [Variables](<#variables>)
- [func DebugLoggingInterceptor() grpc.UnaryServerInterceptor](<#func-debuglogginginterceptor>)
- [func DefaultClientInterceptor(defaultOpts ...interface{}) grpc.UnaryClientInterceptor](<#func-defaultclientinterceptor>)
- [func DefaultClientInterceptors(defaultOpts ...interface{}) []grpc.UnaryClientInterceptor](<#func-defaultclientinterceptors>)
- [func DefaultInterceptors() []grpc.UnaryServerInterceptor](<#func-defaultinterceptors>)
- [func DefaultStreamInterceptors() []grpc.StreamServerInterceptor](<#func-defaultstreaminterceptors>)
- [func DoHTTPtoGRPC(ctx context.Context, svr interface{}, handler func(ctx context.Context, req interface{}) (interface{}, error), in interface{}) (interface{}, error)](<#func-dohttptogrpc>)
- [func FilterMethodsFunc(ctx context.Context, fullMethodName string) bool](<#func-filtermethodsfunc>)
- [func GRPCClientInterceptor(options ...grpc_opentracing.Option) grpc.UnaryClientInterceptor](<#func-grpcclientinterceptor>)
- [func HystrixClientInterceptor(defaultOpts ...grpc.CallOption) grpc.UnaryClientInterceptor](<#func-hystrixclientinterceptor>)
- [func IgnoreErrorNotification(ctx context.Context, ignore bool) context.Context](<#func-ignoreerrornotification>)
- [func NRHttpTracer(pattern string, h http.HandlerFunc) (string, http.HandlerFunc)](<#func-nrhttptracer>)
- [func NewRelicClientInterceptor() grpc.UnaryClientInterceptor](<#func-newrelicclientinterceptor>)
- [func NewRelicInterceptor() grpc.UnaryServerInterceptor](<#func-newrelicinterceptor>)
- [func OptionsInterceptor() grpc.UnaryServerInterceptor](<#func-optionsinterceptor>)
- [func PanicRecoveryInterceptor() grpc.UnaryServerInterceptor](<#func-panicrecoveryinterceptor>)
- [func ResponseTimeLoggingInterceptor(ff FilterFunc) grpc.UnaryServerInterceptor](<#func-responsetimelogginginterceptor>)
- [func ResponseTimeLoggingStreamInterceptor() grpc.StreamServerInterceptor](<#func-responsetimeloggingstreaminterceptor>)
- [func ServerErrorInterceptor() grpc.UnaryServerInterceptor](<#func-servererrorinterceptor>)
- [func ServerErrorStreamInterceptor() grpc.StreamServerInterceptor](<#func-servererrorstreaminterceptor>)
- [func SetFilterFunc(ctx context.Context, ff FilterFunc)](<#func-setfilterfunc>)
- [func TraceIdInterceptor() grpc.UnaryServerInterceptor](<#func-traceidinterceptor>)
- [type FilterFunc](<#type-filterfunc>)


## Variables

```go
var (
    //FilterMethods is the list of methods that are filtered by default
    FilterMethods = []string{"Healthcheck", "HealthCheck", "healthcheck"}
)
```

## func [DebugLoggingInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L139>)

```go
func DebugLoggingInterceptor() grpc.UnaryServerInterceptor
```

DebugLoggingInterceptor is the interceptor that logs all request/response from a handler

## func [DefaultClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L134>)

```go
func DefaultClientInterceptor(defaultOpts ...interface{}) grpc.UnaryClientInterceptor
```

DefaultClientInterceptor are the set of default interceptors that should be applied to all client calls

## func [DefaultClientInterceptors](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L99>)

```go
func DefaultClientInterceptors(defaultOpts ...interface{}) []grpc.UnaryClientInterceptor
```

DefaultClientInterceptors are the set of default interceptors that should be applied to all client calls

## func [DefaultInterceptors](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L85>)

```go
func DefaultInterceptors() []grpc.UnaryServerInterceptor
```

DefaultInterceptors are the set of default interceptors that are applied to all coldbrew methods

## func [DefaultStreamInterceptors](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L123>)

```go
func DefaultStreamInterceptors() []grpc.StreamServerInterceptor
```

DefaultStreamInterceptors are the set of default interceptors that should be applied to all coldbrew streams

## func [DoHTTPtoGRPC](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L71>)

```go
func DoHTTPtoGRPC(ctx context.Context, svr interface{}, handler func(ctx context.Context, req interface{}) (interface{}, error), in interface{}) (interface{}, error)
```

DoHTTPtoGRPC allows calling the interceptors when you use the Register\<svc\-name\>HandlerServer in grpc\-gateway\, see example below for reference

```
func (s *svc) Echo(ctx context.Context, req *proto.EchoRequest) (*proto.EchoResponse, error) {
    handler := func(ctx context.Context, req interface{}) (interface{}, error) {
        return s.echo(ctx, req.(*proto.EchoRequest))
    }
    r, e := doHTTPtoGRPC(ctx, s, handler, req)
    if e != nil {
        return nil, e.(error)
    }
    return r.(*proto.EchoResponse), nil
}

func (s *svc) echo(ctx context.Context, req *proto.EchoRequest) (*proto.EchoResponse, error) {
       .... implementation ....
}
```

## func [FilterMethodsFunc](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L38>)

```go
func FilterMethodsFunc(ctx context.Context, fullMethodName string) bool
```

FilterMethodsFunc is the default implementation of Filter function

## func [GRPCClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L238>)

```go
func GRPCClientInterceptor(options ...grpc_opentracing.Option) grpc.UnaryClientInterceptor
```

GRPCClientInterceptor is the interceptor that intercepts all cleint requests and adds tracing info to them

## func [HystrixClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L243>)

```go
func HystrixClientInterceptor(defaultOpts ...grpc.CallOption) grpc.UnaryClientInterceptor
```

HystrixClientInterceptor is the interceptor that intercepts all client requests and adds hystrix info to them

## func [IgnoreErrorNotification](<https://github.com/go-coldbrew/interceptors/blob/main/options.go#L64>)

```go
func IgnoreErrorNotification(ctx context.Context, ignore bool) context.Context
```

IgnoreErrorNotification defines if Coldbrew should send this error to notifier or not this should be called from within the service code

## func [NRHttpTracer](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L314>)

```go
func NRHttpTracer(pattern string, h http.HandlerFunc) (string, http.HandlerFunc)
```

NRHttpTracer adds newrelic tracing to this http function

## func [NewRelicClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L227>)

```go
func NewRelicClientInterceptor() grpc.UnaryClientInterceptor
```

NewRelicClientInterceptor intercepts all client actions and reports them to newrelic

## func [NewRelicInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L174>)

```go
func NewRelicInterceptor() grpc.UnaryServerInterceptor
```

NewRelicInterceptor intercepts all server actions and reports them to newrelic

## func [OptionsInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L165>)

```go
func OptionsInterceptor() grpc.UnaryServerInterceptor
```

## func [PanicRecoveryInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L205>)

```go
func PanicRecoveryInterceptor() grpc.UnaryServerInterceptor
```

## func [ResponseTimeLoggingInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L149>)

```go
func ResponseTimeLoggingInterceptor(ff FilterFunc) grpc.UnaryServerInterceptor
```

ResponseTimeLoggingInterceptor logs response time for each request on server

## func [ResponseTimeLoggingStreamInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L282>)

```go
func ResponseTimeLoggingStreamInterceptor() grpc.StreamServerInterceptor
```

ResponseTimeLoggingStreamInterceptor logs response time for stream RPCs\.

## func [ServerErrorInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L186>)

```go
func ServerErrorInterceptor() grpc.UnaryServerInterceptor
```

ServerErrorInterceptor intercepts all server actions and reports them to error notifier

## func [ServerErrorStreamInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L294>)

```go
func ServerErrorStreamInterceptor() grpc.StreamServerInterceptor
```

ServerErrorStreamInterceptor intercepts server errors for stream RPCs and reports them to the error notifier\.

## func [SetFilterFunc](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L48>)

```go
func SetFilterFunc(ctx context.Context, ff FilterFunc)
```

SetFilterFunc sets the default filter function to be used by interceptors

## func [TraceIdInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L336>)

```go
func TraceIdInterceptor() grpc.UnaryServerInterceptor
```

TraceIdInterceptor allows injecting trace id from request objects

## type [FilterFunc](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L35>)

If it returns false\, the given request will not be traced\.

```go
type FilterFunc func(ctx context.Context, fullMethodName string) bool
```



Generated by [gomarkdoc](<https://github.com/princjef/gomarkdoc>)
