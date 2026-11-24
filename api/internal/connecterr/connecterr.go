// Package connecterr maps service-layer errors to Connect errors that carry a
// machine-readable ErrorReason detail, so clients branch on the reason rather than
// on message text. Handlers return *connecterr.Error; the Interceptor renders it.
package connecterr

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
)

// Error is a service-layer error carrying a Connect code and a machine-readable
// reason. Field names the offending input for DUPLICATE/INVALID.
type Error struct {
	Code    connect.Code
	Reason  restaurantv1.ErrorReason
	Field   string
	Message string
	Err     error // underlying cause, never sent to the client
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Reason.String()
}

func (e *Error) Unwrap() error { return e.Err }

// New builds an Error. Prefer the named constructors below where one exists.
func New(code connect.Code, reason restaurantv1.ErrorReason, msg string) *Error {
	return &Error{Code: code, Reason: reason, Message: msg}
}

// Named constructors for the reasons shared across domains. Grow this list as
// each step introduces a new reason; keep the Connect-code mapping here, once.
func StaleVersion(msg string) *Error {
	return New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_STALE_VERSION, msg)
}

func NotAllowed(msg string) *Error {
	return New(connect.CodePermissionDenied, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED, msg)
}

func InUse(msg string) *Error {
	return New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_IN_USE, msg)
}

func NotFound(msg string) *Error {
	return New(connect.CodeNotFound, restaurantv1.ErrorReason_ERROR_REASON_NOT_FOUND, msg)
}

func Duplicate(field, msg string) *Error {
	e := New(connect.CodeAlreadyExists, restaurantv1.ErrorReason_ERROR_REASON_DUPLICATE, msg)
	e.Field = field
	return e
}

func Invalid(field, msg string) *Error {
	e := New(connect.CodeInvalidArgument, restaurantv1.ErrorReason_ERROR_REASON_INVALID, msg)
	e.Field = field
	return e
}

// Unauthenticated means "no valid session" (expired/absent). It carries NO reason
// detail, so the web treats it as session-expired and redirects to login — unlike
// InvalidCredentials below, which is a rejected login the user should see inline.
func Unauthenticated(msg string) *Error {
	return &Error{Code: connect.CodeUnauthenticated, Reason: restaurantv1.ErrorReason_ERROR_REASON_UNSPECIFIED, Message: msg}
}

func InvalidCredentials() *Error {
	return New(connect.CodeUnauthenticated, restaurantv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS, "invalid username, password or PIN")
}

func PinLocked() *Error {
	return New(connect.CodeUnauthenticated, restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED, "PIN locked — ask a manager to reset it")
}

func DeviceNotShared() *Error {
	return New(connect.CodePermissionDenied, restaurantv1.ErrorReason_ERROR_REASON_DEVICE_NOT_SHARED, "this device is not enrolled for staff PIN login")
}

func ManagerApprovalRequired(msg string) *Error {
	return New(connect.CodePermissionDenied, restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_REQUIRED, msg)
}

func ManagerApprovalInvalid() *Error {
	return New(connect.CodePermissionDenied, restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID, "manager PIN not recognised")
}

func OrderClosed(msg string) *Error {
	return New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_ORDER_CLOSED, msg)
}

func ShiftNotOpen(msg string) *Error {
	return New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_SHIFT_NOT_OPEN, msg)
}

// Precondition is a generic failed-precondition with no machine reason.
func Precondition(msg string) *Error {
	return &Error{Code: connect.CodeFailedPrecondition, Reason: restaurantv1.ErrorReason_ERROR_REASON_UNSPECIFIED, Message: msg}
}

// Internal wraps an unexpected error. The cause is logged (by the caller) and
// never returned to the client; no reason detail is attached.
func Internal(err error) *Error {
	return &Error{Code: connect.CodeInternal, Reason: restaurantv1.ErrorReason_ERROR_REASON_UNSPECIFIED, Message: "internal error", Err: err}
}

// toConnect converts an Error into a connect.Error. A machine-readable ErrorInfo
// detail is attached unless the reason is UNSPECIFIED (e.g. a bare Unauthenticated),
// so "no reason" stays genuinely reason-less on the wire.
func toConnect(e *Error) *connect.Error {
	cerr := connect.NewError(e.Code, errors.New(e.Error()))
	if e.Reason == restaurantv1.ErrorReason_ERROR_REASON_UNSPECIFIED {
		return cerr
	}
	if d, derr := connect.NewErrorDetail(&restaurantv1.ErrorInfo{
		Reason:  e.Reason,
		Field:   e.Field,
		Message: e.Message,
	}); derr == nil {
		cerr.AddDetail(d)
	}
	return cerr
}

// Interceptor renders any *Error returned by a handler as a connect.Error with a
// machine-readable ErrorInfo detail. Other errors pass through unchanged.
func Interceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			resp, err := next(ctx, req)
			var appErr *Error
			if errors.As(err, &appErr) {
				return resp, toConnect(appErr)
			}
			return resp, err
		}
	}
}
