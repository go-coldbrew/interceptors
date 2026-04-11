// Package interceptors provides gRPC server and client interceptors for the ColdBrew framework.
//
// Interceptor configuration functions (AddUnaryServerInterceptor, SetFilterFunc, etc.)
// must be called during program initialization, before the gRPC server starts.
// They are not safe for concurrent use.
package interceptors

import (
	"github.com/go-coldbrew/errors"
)

// SupportPackageIsVersion1 is a compile-time assertion constant.
// Downstream packages (e.g. core) reference this constant to enforce
// version compatibility. When interceptors makes a breaking change,
// export a new constant and remove this one to force coordinated updates.
const SupportPackageIsVersion1 = true

// Compile-time version compatibility check.
var _ = errors.SupportPackageIsVersion1
