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
- [func AddStreamClientInterceptor(ctx context.Context, i ...grpc.StreamClientInterceptor)](<#func-addstreamclientinterceptor>)
- [func AddStreamServerInterceptor(ctx context.Context, i ...grpc.StreamServerInterceptor)](<#func-addstreamserverinterceptor>)
- [func AddUnaryClientInterceptor(ctx context.Context, i ...grpc.UnaryClientInterceptor)](<#func-addunaryclientinterceptor>)
- [func AddUnaryServerInterceptor(ctx context.Context, i ...grpc.UnaryServerInterceptor)](<#func-addunaryserverinterceptor>)
- [func DebugLoggingInterceptor() grpc.UnaryServerInterceptor](<#func-debuglogginginterceptor>)
- [func DefaultClientInterceptor(defaultOpts ...interface{}) grpc.UnaryClientInterceptor](<#func-defaultclientinterceptor>)
- [func DefaultClientInterceptors(defaultOpts ...interface{}) []grpc.UnaryClientInterceptor](<#func-defaultclientinterceptors>)
- [func DefaultClientStreamInterceptor(defaultOpts ...interface{}) grpc.StreamClientInterceptor](<#func-defaultclientstreaminterceptor>)
- [func DefaultClientStreamInterceptors(defaultOpts ...interface{}) []grpc.StreamClientInterceptor](<#func-defaultclientstreaminterceptors>)
- [func DefaultInterceptors() []grpc.UnaryServerInterceptor](<#func-defaultinterceptors>)
- [func DefaultStreamInterceptors() []grpc.StreamServerInterceptor](<#func-defaultstreaminterceptors>)
- [func DoHTTPtoGRPC(ctx context.Context, svr interface{}, handler func(ctx context.Context, req interface{}) (interface{}, error), in interface{}) (interface{}, error)](<#func-dohttptogrpc>)
- [func FilterMethodsFunc(ctx context.Context, fullMethodName string) bool](<#func-filtermethodsfunc>)
- [func GRPCClientInterceptor(options ...grpc_opentracing.Option) grpc.UnaryClientInterceptor](<#func-grpcclientinterceptor>)
- [func HystrixClientInterceptor(defaultOpts ...grpc.CallOption) grpc.UnaryClientInterceptor](<#func-hystrixclientinterceptor>)
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
- [func UseColdBrewClientInterceptors(ctx context.Context, flag bool)](<#func-usecoldbrewclientinterceptors>)
- [func UseColdBrewServerInterceptors(ctx context.Context, flag bool)](<#func-usecoldbrewserverinterceptors>)
- [type FilterFunc](<#type-filterfunc>)


## Variables

```go
var (
    //FilterMethods is the list of methods that are filtered by default
    FilterMethods = []string{"healthcheck", "readycheck", "serverreflectioninfo"}
)
```

## func [AddStreamClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L83>)

```go
func AddStreamClientInterceptor(ctx context.Context, i ...grpc.StreamClientInterceptor)
```

AddStreamClientInterceptor adds a server interceptor to default server interceptors

## func [AddStreamServerInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L66>)

```go
func AddStreamServerInterceptor(ctx context.Context, i ...grpc.StreamServerInterceptor)
```

AddStreamServerInterceptor adds a server interceptor to default server interceptors

## func [AddUnaryClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L78>)

```go
func AddUnaryClientInterceptor(ctx context.Context, i ...grpc.UnaryClientInterceptor)
```

AddUnaryClientInterceptor adds a server interceptor to default server interceptors

## func [AddUnaryServerInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L61>)

```go
func AddUnaryServerInterceptor(ctx context.Context, i ...grpc.UnaryServerInterceptor)
```

AddUnaryServerInterceptor adds a server interceptor to default server interceptors

## func [DebugLoggingInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L229>)

```go
func DebugLoggingInterceptor() grpc.UnaryServerInterceptor
```

DebugLoggingInterceptor is the interceptor that logs all request/response from a handler

## func [DefaultClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L219>)

```go
func DefaultClientInterceptor(defaultOpts ...interface{}) grpc.UnaryClientInterceptor
```

DefaultClientInterceptor are the set of default interceptors that should be applied to all client calls

## func [DefaultClientInterceptors](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L146>)

```go
func DefaultClientInterceptors(defaultOpts ...interface{}) []grpc.UnaryClientInterceptor
```

DefaultClientInterceptors are the set of default interceptors that should be applied to all client calls

## func [DefaultClientStreamInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L224>)

```go
func DefaultClientStreamInterceptor(defaultOpts ...interface{}) grpc.StreamClientInterceptor
```

DefaultClientStreamInterceptor are the set of default interceptors that should be applied to all stream client calls

## func [DefaultClientStreamInterceptors](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L176>)

```go
func DefaultClientStreamInterceptors(defaultOpts ...interface{}) []grpc.StreamClientInterceptor
```

DefaultClientStreamInterceptors are the set of default interceptors that should be applied to all stream client calls

## func [DefaultInterceptors](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L125>)

```go
func DefaultInterceptors() []grpc.UnaryServerInterceptor
```

DefaultInterceptors are the set of default interceptors that are applied to all coldbrew methods

## func [DefaultStreamInterceptors](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L201>)

```go
func DefaultStreamInterceptors() []grpc.StreamServerInterceptor
```

DefaultStreamInterceptors are the set of default interceptors that should be applied to all coldbrew streams

## func [DoHTTPtoGRPC](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L111>)

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

## func [FilterMethodsFunc](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L44>)

```go
func FilterMethodsFunc(ctx context.Context, fullMethodName string) bool
```

FilterMethodsFunc is the default implementation of Filter function

## func [GRPCClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L328>)

```go
func GRPCClientInterceptor(options ...grpc_opentracing.Option) grpc.UnaryClientInterceptor
```

GRPCClientInterceptor is the interceptor that intercepts all cleint requests and adds tracing info to them

## func [HystrixClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L333>)

```go
func HystrixClientInterceptor(defaultOpts ...grpc.CallOption) grpc.UnaryClientInterceptor
```

HystrixClientInterceptor is the interceptor that intercepts all client requests and adds hystrix info to them

## func [NRHttpTracer](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L404>)

```go
func NRHttpTracer(pattern string, h http.HandlerFunc) (string, http.HandlerFunc)
```

NRHttpTracer adds newrelic tracing to this http function

## func [NewRelicClientInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L317>)

```go
func NewRelicClientInterceptor() grpc.UnaryClientInterceptor
```

NewRelicClientInterceptor intercepts all client actions and reports them to newrelic

## func [NewRelicInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L264>)

```go
func NewRelicInterceptor() grpc.UnaryServerInterceptor
```

NewRelicInterceptor intercepts all server actions and reports them to newrelic

## func [OptionsInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L255>)

```go
func OptionsInterceptor() grpc.UnaryServerInterceptor
```

## func [PanicRecoveryInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L295>)

```go
func PanicRecoveryInterceptor() grpc.UnaryServerInterceptor
```

## func [ResponseTimeLoggingInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L239>)

```go
func ResponseTimeLoggingInterceptor(ff FilterFunc) grpc.UnaryServerInterceptor
```

ResponseTimeLoggingInterceptor logs response time for each request on server

## func [ResponseTimeLoggingStreamInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L372>)

```go
func ResponseTimeLoggingStreamInterceptor() grpc.StreamServerInterceptor
```

ResponseTimeLoggingStreamInterceptor logs response time for stream RPCs\.

## func [ServerErrorInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L276>)

```go
func ServerErrorInterceptor() grpc.UnaryServerInterceptor
```

ServerErrorInterceptor intercepts all server actions and reports them to error notifier

## func [ServerErrorStreamInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L384>)

```go
func ServerErrorStreamInterceptor() grpc.StreamServerInterceptor
```

ServerErrorStreamInterceptor intercepts server errors for stream RPCs and reports them to the error notifier\.

## func [SetFilterFunc](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L54>)

```go
func SetFilterFunc(ctx context.Context, ff FilterFunc)
```

SetFilterFunc sets the default filter function to be used by interceptors

## func [TraceIdInterceptor](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L426>)

```go
func TraceIdInterceptor() grpc.UnaryServerInterceptor
```

TraceIdInterceptor allows injecting trace id from request objects

## func [UseColdBrewClientInterceptors](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L90>)

```go
func UseColdBrewClientInterceptors(ctx context.Context, flag bool)
```

### UseColdBrewClientInterceptors allows enabling/disabling coldbrew client interceptors

when set to false\, the coldbrew client interceptors will not be used

## func [UseColdBrewServerInterceptors](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L73>)

```go
func UseColdBrewServerInterceptors(ctx context.Context, flag bool)
```

### UseColdBrewServerInterceptors allows enabling/disabling coldbrew server interceptors

when set to false\, the coldbrew server interceptors will not be used

## type [FilterFunc](<https://github.com/go-coldbrew/interceptors/blob/main/interceptors.go#L41>)

If it returns false\, the given request will not be traced\.

```go
type FilterFunc func(ctx context.Context, fullMethodName string) bool
```



Generated by [gomarkdoc](<https://github.com/princjef/gomarkdoc>)
