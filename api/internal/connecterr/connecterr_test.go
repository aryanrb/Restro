package connecterr_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/connecterr"
)

func TestInterceptorMapsErrorToDetail(t *testing.T) {
	ic := connecterr.Interceptor()
	handler := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, connecterr.Duplicate("username", "already taken")
	}
	wrapped := ic.WrapUnary(handler)

	_, err := wrapped(context.Background(), connect.NewRequest(&restaurantv1.GetSessionRequest{}))

	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("want *connect.Error, got %T (%v)", err, err)
	}
	if cerr.Code() != connect.CodeAlreadyExists {
		t.Fatalf("code = %v, want already_exists", cerr.Code())
	}

	info := errorInfo(t, cerr)
	if info.GetReason() != restaurantv1.ErrorReason_ERROR_REASON_DUPLICATE {
		t.Fatalf("reason = %v, want DUPLICATE", info.GetReason())
	}
	if info.GetField() != "username" {
		t.Fatalf("field = %q, want username", info.GetField())
	}
}

func TestInterceptorPassesThroughNonAppError(t *testing.T) {
	ic := connecterr.Interceptor()
	sentinel := errors.New("boom")
	handler := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, sentinel
	}
	wrapped := ic.WrapUnary(handler)

	_, err := wrapped(context.Background(), connect.NewRequest(&restaurantv1.GetSessionRequest{}))
	if !errors.Is(err, sentinel) {
		t.Fatalf("want the original error, got %v", err)
	}
}

func errorInfo(t *testing.T, cerr *connect.Error) *restaurantv1.ErrorInfo {
	t.Helper()
	for _, d := range cerr.Details() {
		msg, err := d.Value()
		if err != nil {
			continue
		}
		if info, ok := msg.(*restaurantv1.ErrorInfo); ok {
			return info
		}
	}
	t.Fatalf("no ErrorInfo detail on the connect error")
	return nil
}
