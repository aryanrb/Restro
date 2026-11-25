package auth_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
)

// ownerCtx logs the seeded owner in and resolves the session through the auth
// interceptor, exactly as a request would be.
func ownerCtx(t *testing.T, ctx context.Context, svc *auth.Service) context.Context {
	t.Helper()
	_, token, err := svc.PasswordLogin(ctx, "owner", "owner123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	req := connect.NewRequest(&restaurantv1.ListStaffRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var out context.Context
	_, _ = svc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out = c
		return nil, nil
	})(ctx, req)
	if !auth.ActorFrom(out).LoggedIn() {
		t.Fatalf("actor not resolved")
	}
	return out
}

func strp(s string) *string { return &s }

func TestStaffMemberHasPasswordAndPhone(t *testing.T) {
	ctx := context.Background()
	svc, q := newTestService(t)
	seedOwnerActor(t, ctx, svc, q)
	octx := ownerCtx(t, ctx, svc)
	h := auth.NewHandler(svc, false)

	save := func(in *restaurantv1.StaffInput) (*restaurantv1.StaffMember, error) {
		res, err := h.SaveStaff(octx, connect.NewRequest(&restaurantv1.SaveStaffRequest{Input: in}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}

	cashier, err := save(&restaurantv1.StaffInput{
		Name: "Sita", Role: restaurantv1.Role_ROLE_CASHIER, Active: true, Pin: strp("2222"), Phone: strp("+977 980-1234567"),
	})
	if err != nil {
		t.Fatalf("create cashier: %v", err)
	}
	if cashier.HasPassword {
		t.Fatalf("cashier should have no password")
	}
	if cashier.GetPhone() != "9801234567" || cashier.Phone == nil {
		t.Fatalf("phone = %v, want normalised 9801234567", cashier.Phone)
	}

	// Unset phone on edit leaves it unchanged.
	edited, err := save(&restaurantv1.StaffInput{Id: strp(cashier.Id), Name: "Sita K", Role: restaurantv1.Role_ROLE_CASHIER, Active: true})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.GetPhone() != "9801234567" {
		t.Fatalf("phone lost on edit: %v", edited.Phone)
	}
	// Landline written internationally keeps its trunk 0.
	edited, err = save(&restaurantv1.StaffInput{Id: strp(cashier.Id), Name: "Sita K", Role: restaurantv1.Role_ROLE_CASHIER, Active: true, Phone: strp("+977 1 4412345")})
	if err != nil {
		t.Fatalf("edit landline: %v", err)
	}
	if edited.GetPhone() != "014412345" {
		t.Fatalf("landline = %q, want 014412345", edited.GetPhone())
	}
	// "" clears it.
	edited, err = save(&restaurantv1.StaffInput{Id: strp(cashier.Id), Name: "Sita K", Role: restaurantv1.Role_ROLE_CASHIER, Active: true, Phone: strp("")})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if edited.Phone != nil {
		t.Fatalf("phone should be cleared, got %q", edited.GetPhone())
	}

	for _, bad := range []string{"abc", "12345", "98012345678"} {
		_, err = save(&restaurantv1.StaffInput{Name: "X", Role: restaurantv1.Role_ROLE_WAITER, Active: true, Pin: strp("3333"), Phone: strp(bad)})
		wantReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID)
	}

	res, err := h.ListStaff(octx, connect.NewRequest(&restaurantv1.ListStaffRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, m := range res.Msg.Staff {
		switch m.Role {
		case restaurantv1.Role_ROLE_OWNER:
			if !m.HasPassword {
				t.Fatalf("owner should have a password")
			}
		case restaurantv1.Role_ROLE_CASHIER:
			if m.HasPassword {
				t.Fatalf("cashier should not have a password")
			}
		}
	}
}

func TestNormalisePhone(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"  ":               "",
		"9801234567":       "9801234567",
		"980-123-4567":     "9801234567",
		"+9779801234567":   "9801234567",
		"00977 9801234567": "9801234567",
		"01-4412345":       "014412345",
		"+977 1 4412345":   "014412345",
	}
	for in, want := range cases {
		got, ok := auth.NormalisePhone(in)
		if !ok || got != want {
			t.Errorf("NormalisePhone(%q) = %q,%v want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"abc", "12345", "98012345678", "9801x34567"} {
		if _, ok := auth.NormalisePhone(bad); ok {
			t.Errorf("NormalisePhone(%q) should fail", bad)
		}
	}
}
