package kitchen

import (
	"context"
	"errors"
	"os"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
)

func TestKitchenRPCsRequireKitchenRole(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run kitchen integration tests")
	}
	ctx := context.Background()
	pool, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE users, sessions, devices, audit_log RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	q := sqlc.New(pool)
	authSvc := auth.NewService(pool, zap.NewNop())
	svc := NewService(pool, nil, zap.NewNop())
	pwHash, err := auth.HashSecret("secret123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	// as resolves a real session for a fresh user of the given role through the
	// auth interceptor, exactly as a request would be.
	as := func(role string) context.Context {
		username := "u-" + uuid.NewString()[:8]
		if _, err := q.CreateUser(ctx, sqlc.CreateUserParams{
			Name: role, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(pwHash), Language: "en", Active: true,
		}); err != nil {
			t.Fatalf("create user: %v", err)
		}
		_, token, err := authSvc.PasswordLogin(ctx, username, "secret123")
		if err != nil {
			t.Fatalf("login: %v", err)
		}
		req := connect.NewRequest(&restaurantv1.ListStationTicketsRequest{})
		req.Header().Set("Authorization", "Bearer "+token)
		var out context.Context
		_, _ = authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			out = c
			return nil, nil
		})(ctx, req)
		if !auth.ActorFrom(out).LoggedIn() {
			t.Fatalf("actor not resolved")
		}
		return out
	}

	someID := uuid.NewString()
	calls := map[string]func(context.Context) error{
		"ListStationTickets": func(c context.Context) error { _, err := svc.ListStationTickets(c, someID); return err },
		"StartItems":         func(c context.Context) error { _, err := svc.StartItems(c, []string{someID}); return err },
		"MarkItemsReady":     func(c context.Context) error { _, err := svc.MarkItemsReady(c, []string{someID}); return err },
		"BumpTicket":         func(c context.Context) error { _, err := svc.BumpTicket(c, "bad", "bad"); return err },
	}
	allowed := map[string]bool{auth.RoleOwner: true, auth.RoleManager: true, auth.RoleKitchen: true}
	for _, role := range []string{auth.RoleOwner, auth.RoleManager, auth.RoleKitchen, auth.RoleWaiter, auth.RoleCashier} {
		c := as(role)
		for name, call := range calls {
			err := call(c)
			var ae *connecterr.Error
			isNotAllowed := errors.As(err, &ae) && ae.Reason == restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED && ae.Code == connect.CodePermissionDenied
			if allowed[role] && isNotAllowed {
				t.Errorf("%s as %s: got NOT_ALLOWED, want allowed", name, role)
			}
			if !allowed[role] && !isNotAllowed {
				t.Errorf("%s as %s: got %v, want PermissionDenied/NOT_ALLOWED", name, role, err)
			}
		}
	}

	if _, err := svc.ListStationTickets(ctx, someID); err == nil {
		t.Fatal("logged out: want Unauthenticated, got nil")
	}
}
