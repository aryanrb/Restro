package auth_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// newTestService returns a clean auth.Service against TEST_DATABASE_URL, plus the
// sqlc Queries for test fixtures. Skips when no DB is configured.
func newTestService(t *testing.T) (*auth.Service, *sqlc.Queries) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run auth integration tests")
	}
	ctx := context.Background()
	pool, err := db.New(ctx, url)
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	if err := db.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE sessions, devices, audit_log, processed_commands, users RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return auth.NewService(pool, zap.NewNop()), sqlc.New(pool)
}

func wantReason(t *testing.T, err error, reason restaurantv1.ErrorReason) {
	t.Helper()
	var ae *connecterr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *connecterr.Error, got %v", err)
	}
	if ae.Reason != reason {
		t.Fatalf("reason = %v, want %v", ae.Reason, reason)
	}
}

func seedOwnerActor(t *testing.T, ctx context.Context, svc *auth.Service, q *sqlc.Queries) *auth.Actor {
	t.Helper()
	if _, err := svc.SeedOwner(ctx, "Ram", "owner", "owner123", "1111"); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	owner, err := q.GetUserByUsername(ctx, pgtype.Text{String: "owner", Valid: true})
	if err != nil {
		t.Fatalf("get owner: %v", err)
	}
	return &auth.Actor{User: &owner}
}

func TestPasswordLogin(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	seedOwnerActor(t, ctx, svc, q)

	u, token, err := svc.PasswordLogin(ctx, "owner", "owner123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if u.Role != auth.RoleOwner || token == "" {
		t.Fatalf("bad login result: role=%s token=%q", u.Role, token)
	}

	_, _, err = svc.PasswordLogin(ctx, "owner", "wrong")
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS)
}

func TestPinLockoutAndReset(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	owner := seedOwnerActor(t, ctx, svc, q)

	// Create a waiter and enrol a shared device.
	waiter, err := svc.SaveStaff(ctx, owner, auth.StaffInput{Name: "Gita", Role: auth.RoleWaiter, Active: true, Pin: "4444"})
	if err != nil {
		t.Fatalf("create waiter: %v", err)
	}
	device, _, err := svc.EnrollDevice(ctx, owner, "Counter PC")
	if err != nil {
		t.Fatalf("enroll device: %v", err)
	}

	// 4 wrong PINs → INVALID_CREDENTIALS each.
	for i := 0; i < 4; i++ {
		_, _, err := svc.PinLogin(ctx, waiter.ID, "0000", &device)
		wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID_CREDENTIALS)
	}
	// 5th wrong → PIN_LOCKED; and the correct PIN stays locked.
	_, _, err = svc.PinLogin(ctx, waiter.ID, "0000", &device)
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED)
	_, _, err = svc.PinLogin(ctx, waiter.ID, "4444", &device)
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED)

	// Reset clears the lock.
	if err := svc.ResetPin(ctx, owner, waiter.ID, "7777"); err != nil {
		t.Fatalf("reset pin: %v", err)
	}
	if _, _, err := svc.PinLogin(ctx, waiter.ID, "7777", &device); err != nil {
		t.Fatalf("login after reset: %v", err)
	}
}

func TestPinLoginNeedsSharedDevice(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	owner := seedOwnerActor(t, ctx, svc, q)
	waiter, _ := svc.SaveStaff(ctx, owner, auth.StaffInput{Name: "Gita", Role: auth.RoleWaiter, Active: true, Pin: "4444"})

	_, _, err := svc.PinLogin(ctx, waiter.ID, "4444", nil) // no device
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_DEVICE_NOT_SHARED)

	_, err = svc.ListPinUsers(ctx, nil)
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_DEVICE_NOT_SHARED)
}

func TestSaveStaffRules(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	owner := seedOwnerActor(t, ctx, svc, q)

	// Create a manager; then act as that manager.
	mgr, err := svc.SaveStaff(ctx, owner, auth.StaffInput{Name: "Sita", Role: auth.RoleManager, Active: true, Username: "sita", Password: "managerpass", Pin: "2222"})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	mgrRow, _ := q.GetUserByID(ctx, mgr.ID)
	mgrActor := &auth.Actor{User: &mgrRow}

	// Manager may not edit the owner.
	_, err = svc.SaveStaff(ctx, mgrActor, auth.StaffInput{ID: owner.User.ID.String(), Name: "Ram", Role: auth.RoleOwner, Active: true, Username: "owner"})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)

	// Owner may not deactivate self when last active owner.
	_, err = svc.SaveStaff(ctx, owner, auth.StaffInput{ID: owner.User.ID.String(), Name: "Ram", Role: auth.RoleOwner, Active: false, Username: "owner"})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)

	// Duplicate username.
	_, err = svc.SaveStaff(ctx, owner, auth.StaffInput{Name: "Imposter", Role: auth.RoleManager, Active: true, Username: "sita", Password: "another8x", Pin: "3333"})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_DUPLICATE)

	// Missing PIN on create → INVALID (field pin).
	_, err = svc.SaveStaff(ctx, owner, auth.StaffInput{Name: "Hari", Role: auth.RoleCashier, Active: true})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID)
}

func TestManagerApproval(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	owner := seedOwnerActor(t, ctx, svc, q)

	// Good owner PIN approves.
	if _, err := svc.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: owner.User.ID.String(), Pin: "1111"}); err != nil {
		t.Fatalf("approval should pass: %v", err)
	}
	// Wrong PIN → invalid.
	_, err := svc.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: owner.User.ID.String(), Pin: "9999"})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID)

	// A waiter is not a valid approver.
	waiter, _ := svc.SaveStaff(ctx, owner, auth.StaffInput{Name: "Gita", Role: auth.RoleWaiter, Active: true, Pin: "4444"})
	_, err = svc.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: waiter.ID.String(), Pin: "4444"})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID)
}
