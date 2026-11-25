package auth_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

func actorFor(t *testing.T, ctx context.Context, q *sqlc.Queries, id uuid.UUID) *auth.Actor {
	t.Helper()
	u, err := q.GetUserByID(ctx, id)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	return &auth.Actor{User: &u}
}

// Wrong approval PINs share the PIN-login failure counter and lock the approver.
func TestManagerApprovalLockout(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	owner := seedOwnerActor(t, ctx, svc, q)
	mgr, err := svc.SaveStaff(ctx, owner, auth.StaffInput{Name: "Sita", Role: auth.RoleManager, Active: true, Username: "sita", Password: "managerpass", Pin: "2222"})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	approve := func(pin string) error {
		_, err := svc.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: mgr.ID.String(), Pin: pin})
		return err
	}

	for i := 0; i < 4; i++ {
		wantReason(t, approve("0000"), restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID)
	}
	if u, _ := q.GetUserByID(ctx, mgr.ID); u.FailedPinAttempts != 4 {
		t.Fatalf("failed attempts = %d, want 4", u.FailedPinAttempts)
	}
	// 5th wrong → locked; the right PIN no longer approves.
	wantReason(t, approve("0000"), restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED)
	wantReason(t, approve("2222"), restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED)

	// The lock is shared with PIN login.
	device, _, err := svc.EnrollDevice(ctx, owner, "Counter PC")
	if err != nil {
		t.Fatalf("enroll device: %v", err)
	}
	_, _, err = svc.PinLogin(ctx, mgr.ID, "2222", &device)
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED)

	// Owner reset unlocks; a successful approval resets the counter.
	if err := svc.ResetPin(ctx, owner, mgr.ID, "2323"); err != nil {
		t.Fatalf("reset pin: %v", err)
	}
	for i := 0; i < 3; i++ {
		wantReason(t, approve("0000"), restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID)
	}
	if err := approve("2323"); err != nil {
		t.Fatalf("approval after reset: %v", err)
	}
	if u, _ := q.GetUserByID(ctx, mgr.ID); u.FailedPinAttempts != 0 {
		t.Fatalf("failed attempts after success = %d, want 0", u.FailedPinAttempts)
	}
}

// Failed PIN logins also count against approvals (same counter, both ways).
func TestPinLoginFailuresLockApproval(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	owner := seedOwnerActor(t, ctx, svc, q)
	device, _, err := svc.EnrollDevice(ctx, owner, "Counter PC")
	if err != nil {
		t.Fatalf("enroll device: %v", err)
	}
	for i := 0; i < 5; i++ {
		_, _, _ = svc.PinLogin(ctx, owner.User.ID, "0000", &device)
	}
	_, err = svc.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: owner.User.ID.String(), Pin: "1111"})
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED)
}

func TestResetPinRules(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	owner := seedOwnerActor(t, ctx, svc, q)

	mk := func(in auth.StaffInput) uuid.UUID {
		t.Helper()
		u, err := svc.SaveStaff(ctx, owner, in)
		if err != nil {
			t.Fatalf("create %s: %v", in.Name, err)
		}
		return u.ID
	}
	mgrID := mk(auth.StaffInput{Name: "Sita", Role: auth.RoleManager, Active: true, Username: "sita", Password: "managerpass", Pin: "2222"})
	mgr2ID := mk(auth.StaffInput{Name: "Mina", Role: auth.RoleManager, Active: true, Username: "mina", Password: "managerpass", Pin: "2323"})
	owner2ID := mk(auth.StaffInput{Name: "Hari", Role: auth.RoleOwner, Active: true, Username: "hari", Password: "ownerpass1", Pin: "1212"})
	cashierID := mk(auth.StaffInput{Name: "Kiran", Role: auth.RoleCashier, Active: true, Pin: "3333"})
	waiterID := mk(auth.StaffInput{Name: "Gita", Role: auth.RoleWaiter, Active: true, Pin: "4444"})
	kitchenID := mk(auth.StaffInput{Name: "Bikash", Role: auth.RoleKitchen, Active: true, Pin: "5555"})
	mgr := actorFor(t, ctx, q, mgrID)

	// Manager may reset front-of-house and kitchen staff.
	for _, id := range []uuid.UUID{cashierID, waiterID, kitchenID} {
		if err := svc.ResetPin(ctx, mgr, id, "9090"); err != nil {
			t.Fatalf("manager reset %s: %v", id, err)
		}
	}
	// ...but not an owner, another manager, or themselves.
	for _, id := range []uuid.UUID{owner.User.ID, owner2ID, mgr2ID, mgrID} {
		wantReason(t, svc.ResetPin(ctx, mgr, id, "9090"), restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)
	}
	// Owner may reset anyone, including managers, other owners and self.
	for _, id := range []uuid.UUID{mgrID, owner2ID, cashierID, owner.User.ID} {
		if err := svc.ResetPin(ctx, owner, id, "8080"); err != nil {
			t.Fatalf("owner reset %s: %v", id, err)
		}
	}
	// Unknown user → NOT_FOUND (owner and manager alike).
	wantReason(t, svc.ResetPin(ctx, owner, uuid.New(), "8080"), restaurantv1.ErrorReason_ERROR_REASON_NOT_FOUND)
	wantReason(t, svc.ResetPin(ctx, mgr, uuid.New(), "8080"), restaurantv1.ErrorReason_ERROR_REASON_NOT_FOUND)

	// Non-privileged actors still cannot reset.
	cashier := actorFor(t, ctx, q, cashierID)
	wantReason(t, svc.ResetPin(ctx, cashier, waiterID, "8080"), restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)

	// Every successful reset is audited (3 by manager + 4 by owner).
	rows, err := q.QueryAuditLog(ctx, sqlc.QueryAuditLogParams{
		FromTs: time.Now().Add(-time.Hour), ToTs: time.Now().Add(time.Hour), Action: "staff.reset_pin", RowLimit: 100,
	})
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	if len(rows) != 7 {
		t.Fatalf("reset_pin audit rows = %d, want 7", len(rows))
	}
}

// Parallel wrong guesses must not get more than pinLockAfter (5) PIN checks.
func TestManagerApprovalLockoutConcurrent(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	owner := seedOwnerActor(t, ctx, svc, q)

	const n = 20
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.VerifyManagerApproval(ctx, auth.ManagerApproval{ManagerUserID: owner.User.ID.String(), Pin: "0000"})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	invalid := 0
	for err := range errs {
		var ae *connecterr.Error
		if !errors.As(err, &ae) {
			t.Fatalf("want *connecterr.Error, got %v", err)
		}
		if ae.Reason == restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID {
			invalid++
		}
	}
	if invalid > 4 {
		t.Fatalf("%d guesses answered INVALID; at most 4 allowed before lock", invalid)
	}
	if u, _ := q.GetUserByID(ctx, owner.User.ID); u.FailedPinAttempts > 5 {
		t.Fatalf("failed attempts = %d, want <= 5", u.FailedPinAttempts)
	}
}
