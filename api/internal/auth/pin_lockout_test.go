package auth_test

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
)

// fakeClock is a settable clock injected into the service.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

type lockoutFixture struct {
	svc   *auth.Service
	q     *sqlc.Queries
	clock *fakeClock
	owner *auth.Actor
	mgrID uuid.UUID
}

func newLockoutFixture(t *testing.T) *lockoutFixture {
	t.Helper()
	ctx := context.Background()
	svc, q := newTestService(t)
	clock := &fakeClock{t: time.Now().Truncate(time.Second)}
	auth.SetNow(svc, clock.now)
	owner := seedOwnerActor(t, ctx, svc, q)
	mgr, err := svc.SaveStaff(ctx, owner, auth.StaffInput{Name: "Sita", Role: auth.RoleManager, Active: true, Username: "sita", Password: "managerpass", Pin: "2222"})
	if err != nil {
		t.Fatalf("create manager: %v", err)
	}
	return &lockoutFixture{svc: svc, q: q, clock: clock, owner: owner, mgrID: mgr.ID}
}

func (f *lockoutFixture) approve(pin string) error {
	_, err := f.svc.VerifyManagerApproval(context.Background(), auth.ManagerApproval{ManagerUserID: f.mgrID.String(), Pin: pin})
	return err
}

// lockOut burns a fresh set of 5 attempts with wrong PINs; the 5th locks.
func (f *lockoutFixture) lockOut(t *testing.T) {
	t.Helper()
	for i := 0; i < 4; i++ {
		wantReason(t, f.approve("0000"), restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID)
	}
	wantReason(t, f.approve("0000"), restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED)
}

func (f *lockoutFixture) user(t *testing.T) sqlc.User {
	t.Helper()
	u, err := f.q.GetUserByID(context.Background(), f.mgrID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	return u
}

func wantLockedMsg(t *testing.T, err error, wantMins int) {
	t.Helper()
	wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED)
	var ae *connecterr.Error
	errors.As(err, &ae)
	want := "try again in " + strconv.Itoa(wantMins) + " min"
	if !strings.Contains(ae.Message, want) {
		t.Fatalf("message = %q, want it to contain %q", ae.Message, want)
	}
}

// assertLockFor checks the lock (just applied at the current clock) holds the
// right PIN out until just before d, then lifts with a fresh 5 attempts.
func (f *lockoutFixture) assertLockFor(t *testing.T, d time.Duration) {
	t.Helper()
	wantLockedMsg(t, f.approve("2222"), int(d/time.Minute))
	f.clock.advance(d - time.Second)
	wantLockedMsg(t, f.approve("2222"), 1)
	f.clock.advance(2 * time.Second)
	// Expired: a fresh 5 attempts (4 wrong answer INVALID, not LOCKED).
	for i := 0; i < 4; i++ {
		wantReason(t, f.approve("0000"), restaurantv1.ErrorReason_ERROR_REASON_MANAGER_APPROVAL_INVALID)
	}
	if u := f.user(t); u.FailedPinAttempts != 4 {
		t.Fatalf("failed attempts after expiry = %d, want 4", u.FailedPinAttempts)
	}
	wantReason(t, f.approve("0000"), restaurantv1.ErrorReason_ERROR_REASON_PIN_LOCKED)
}

func TestPinLockoutProgressive(t *testing.T) {
	f := newLockoutFixture(t)
	f.lockOut(t)
	if u := f.user(t); u.PinLockoutCount != 1 || !u.PinLockedUntil.Valid || !u.PinLockedUntil.Time.Equal(f.clock.t.Add(2*time.Minute)) {
		t.Fatalf("after 1st lock: count=%d until=%v, want 1 / now+2m", u.PinLockoutCount, u.PinLockedUntil)
	}
	f.assertLockFor(t, 2*time.Minute) // ends by triggering the 2nd lock
	if u := f.user(t); u.PinLockoutCount != 2 {
		t.Fatalf("lockout count = %d, want 2", u.PinLockoutCount)
	}
	f.assertLockFor(t, 5*time.Minute)  // → 3rd lock
	f.assertLockFor(t, 30*time.Minute) // → 4th lock, still 30
	wantLockedMsg(t, f.approve("2222"), 30)
	if u := f.user(t); u.PinLockoutCount != 4 {
		t.Fatalf("lockout count = %d, want 4", u.PinLockoutCount)
	}
}

// The lock is shared with PIN login, which reports the remaining wait too, and
// expiry lets the user log in with the right PIN.
func TestPinLockoutSharedWithLoginAndExpires(t *testing.T) {
	ctx := context.Background()
	f := newLockoutFixture(t)
	device, _, err := f.svc.EnrollDevice(ctx, f.owner, "Counter PC")
	if err != nil {
		t.Fatalf("enroll device: %v", err)
	}
	f.lockOut(t)
	f.clock.advance(61 * time.Second)
	_, _, err = f.svc.PinLogin(ctx, f.mgrID, "2222", &device)
	wantLockedMsg(t, err, 1)
	f.clock.advance(time.Minute)
	if _, _, err := f.svc.PinLogin(ctx, f.mgrID, "2222", &device); err != nil {
		t.Fatalf("pin login after expiry: %v", err)
	}
}

// A successful PIN clears the counter and the escalation.
func TestPinSuccessResetsEscalation(t *testing.T) {
	f := newLockoutFixture(t)
	f.lockOut(t)
	f.clock.advance(2*time.Minute + time.Second)
	f.lockOut(t) // 2nd lock: 5 min
	f.clock.advance(5*time.Minute + time.Second)
	if err := f.approve("2222"); err != nil {
		t.Fatalf("approval after expiry: %v", err)
	}
	if u := f.user(t); u.FailedPinAttempts != 0 || u.PinLockoutCount != 0 || u.PinLockedUntil.Valid {
		t.Fatalf("after success: fails=%d count=%d until=%v, want 0/0/null", u.FailedPinAttempts, u.PinLockoutCount, u.PinLockedUntil)
	}
	f.lockOut(t)
	f.assertLockFor(t, 2*time.Minute)
}

// An owner's reset unlocks immediately and clears the escalation.
func TestResetPinUnlocksAndResetsEscalation(t *testing.T) {
	ctx := context.Background()
	f := newLockoutFixture(t)
	f.lockOut(t)
	f.clock.advance(2*time.Minute + time.Second)
	f.lockOut(t) // 2nd lock
	if err := f.svc.ResetPin(ctx, f.owner, f.mgrID, "2323"); err != nil {
		t.Fatalf("reset pin: %v", err)
	}
	if u := f.user(t); u.FailedPinAttempts != 0 || u.PinLockoutCount != 0 || u.PinLockedUntil.Valid {
		t.Fatalf("after reset: fails=%d count=%d until=%v, want 0/0/null", u.FailedPinAttempts, u.PinLockoutCount, u.PinLockedUntil)
	}
	if err := f.approve("2323"); err != nil {
		t.Fatalf("approval right after reset: %v", err)
	}
	f.lockOut(t)
	f.assertLockFor(t, 2*time.Minute)
}
