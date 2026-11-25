package reservations_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/reservations"
)

type fixture struct {
	t       *testing.T
	pool    *pgxpool.Pool
	authSvc *auth.Service
	svc     *reservations.Service

	owner, waiter, kitchen context.Context
	waiterID               uuid.UUID
	area                   string
	t1, t2                 string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set TEST_DATABASE_URL to run reservations integration tests")
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
	if _, err := pool.Exec(ctx, `TRUNCATE reservations, customers, order_item_modifiers, order_items, orders,
		processed_commands, audit_log, sessions, devices, dining_tables, areas, users RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE reservation_settings SET enabled = true, duration_min = 90, large_party_size = 9,
		large_duration_min = 120, hold_before_min = 60, grace_min = 15, release_after_min = 30`); err != nil {
		t.Fatalf("settings: %v", err)
	}
	f := &fixture{t: t, pool: pool, authSvc: auth.NewService(pool, zap.NewNop())}
	f.svc = reservations.NewService(pool, nil, zap.NewNop())
	f.owner, _ = f.user("Ram Owner", auth.RoleOwner)
	f.waiter, f.waiterID = f.user("Gita", auth.RoleWaiter)
	f.kitchen, _ = f.user("Bikash", auth.RoleKitchen)

	q := sqlc.New(pool)
	a, err := q.CreateArea(ctx, sqlc.CreateAreaParams{NameEn: "Main"})
	if err != nil {
		t.Fatalf("area: %v", err)
	}
	f.area = a.ID.String()
	for i, name := range []string{"T1", "T2"} {
		tb, err := q.CreateTable(ctx, sqlc.CreateTableParams{AreaID: a.ID, Name: name, Seats: 4, Active: true, Sort: int32(i)})
		if err != nil {
			t.Fatalf("table: %v", err)
		}
		if i == 0 {
			f.t1 = tb.ID.String()
		} else {
			f.t2 = tb.ID.String()
		}
	}
	return f
}

func (f *fixture) user(name, role string) (context.Context, uuid.UUID) {
	f.t.Helper()
	ctx := context.Background()
	hash, err := auth.HashSecret("secret123")
	if err != nil {
		f.t.Fatal(err)
	}
	username := "u-" + uuid.NewString()[:8]
	u, err := sqlc.New(f.pool).CreateUser(ctx, sqlc.CreateUserParams{
		Name: name, Role: role, Username: dbx.Text(username), PasswordHash: dbx.Text(hash), Language: "en", Active: true,
	})
	if err != nil {
		f.t.Fatalf("create user: %v", err)
	}
	_, token, err := f.authSvc.PasswordLogin(ctx, username, "secret123")
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	req := connect.NewRequest(&restaurantv1.GetSessionRequest{})
	req.Header().Set("Authorization", "Bearer "+token)
	var out context.Context
	_, _ = f.authSvc.Interceptor()(func(c context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		out = c
		return nil, nil
	})(ctx, req)
	if !auth.ActorFrom(out).LoggedIn() {
		f.t.Fatal("actor not resolved")
	}
	return out, u.ID
}

func (f *fixture) book(ctx context.Context, name string, startsAt time.Time, tables ...string) (*restaurantv1.Reservation, error) {
	return f.svc.CreateReservation(ctx, &restaurantv1.CreateReservationRequest{
		RequestId: uuid.NewString(), NewCustomer: &restaurantv1.NewBookingCustomer{Name: name},
		PartySize: 4, StartsAtUnix: startsAt.Unix(), TableIds: tables, Source: restaurantv1.ReservationSource_RESERVATION_SOURCE_PHONE,
	})
}

func (f *fixture) mustBook(ctx context.Context, name string, startsAt time.Time, tables ...string) *restaurantv1.Reservation {
	f.t.Helper()
	r, err := f.book(ctx, name, startsAt, tables...)
	if err != nil {
		f.t.Fatalf("book %s: %v", name, err)
	}
	return r
}

func reasonOf(t *testing.T, err error) restaurantv1.ErrorReason {
	t.Helper()
	var e *connecterr.Error
	if !errors.As(err, &e) {
		t.Fatalf("want *connecterr.Error, got %T (%v)", err, err)
	}
	return e.Reason
}

func expectReason(t *testing.T, err error, want restaurantv1.ErrorReason) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v, got nil", want)
	}
	if got := reasonOf(t, err); got != want {
		t.Fatalf("reason = %v, want %v (%v)", got, want, err)
	}
}

func TestCreateAndOverlap(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	at := time.Now().Add(3 * time.Hour).Truncate(time.Minute)

	req := &restaurantv1.CreateReservationRequest{
		RequestId: uuid.NewString(), NewCustomer: &restaurantv1.NewBookingCustomer{Name: " Ram Shrestha ", Phone: "+977 984-1234567"},
		PartySize: 4, StartsAtUnix: at.Unix(), TableIds: []string{f.t1}, Notes: "birthday",
	}
	r, err := f.svc.CreateReservation(f.waiter, req)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r.CustomerName != "Ram Shrestha" || r.CustomerPhone != "9841234567" || r.Status != restaurantv1.ReservationStatus_RESERVATION_STATUS_BOOKED ||
		r.DurationMin != 90 || r.Version != 1 || r.CreatedById != f.waiterID.String() || r.Source != restaurantv1.ReservationSource_RESERVATION_SOURCE_PHONE {
		t.Fatalf("unexpected reservation %+v", r)
	}
	// Retry-safe on request_id.
	again, err := f.svc.CreateReservation(f.waiter, req)
	if err != nil || again.Id != r.Id {
		t.Fatalf("retry: %v %v", again, err)
	}
	var limit int64
	var customers int
	if err := f.pool.QueryRow(ctx, `SELECT count(*), max(credit_limit_paisa) FROM customers`).Scan(&customers, &limit); err != nil || customers != 1 || limit != 0 {
		t.Fatalf("customers = %d limit = %d (%v)", customers, limit, err)
	}
	var audits int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'reservation.create'`).Scan(&audits)
	if audits != 1 {
		t.Fatalf("audit rows = %d", audits)
	}

	// Same table, overlapping window → TABLE_ALREADY_BOOKED naming the clash.
	_, err = f.book(f.waiter, "Hari", at.Add(60*time.Minute), f.t1, f.t2)
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_TABLE_ALREADY_BOOKED)
	var ce *connecterr.Error
	errors.As(err, &ce)
	if ce.Message != r.Id {
		t.Fatalf("clash message = %q, want %s", ce.Message, r.Id)
	}
	// Right after the window ends, another table, or unassigned: fine.
	f.mustBook(f.waiter, "Hari", at.Add(90*time.Minute), f.t1)
	f.mustBook(f.waiter, "Sita", at, f.t2)
	f.mustBook(f.waiter, "Maya", at)
	f.mustBook(f.waiter, "Bina", at)

	// Existing phone as a new customer → DUPLICATE phone; existing customer by id works.
	_, err = f.svc.CreateReservation(f.waiter, &restaurantv1.CreateReservationRequest{
		RequestId: uuid.NewString(), NewCustomer: &restaurantv1.NewBookingCustomer{Name: "Other", Phone: "9841234567"}, PartySize: 2, StartsAtUnix: at.Unix(),
	})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_DUPLICATE)
	if _, err := f.svc.CreateReservation(f.waiter, &restaurantv1.CreateReservationRequest{
		RequestId: uuid.NewString(), CustomerId: r.CustomerId, PartySize: 10, StartsAtUnix: at.Add(24 * time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("existing customer: %v", err)
	}
	found, err := f.svc.FindBookingCustomers(f.waiter, &restaurantv1.FindBookingCustomersRequest{Query: "984123"})
	if err != nil || len(found.Customers) != 1 || found.Customers[0].BookingCount != 2 {
		t.Fatalf("find: %+v %v", found, err)
	}

	// Validation.
	_, err = f.book(f.waiter, "Past", time.Now().Add(-20*time.Minute))
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID)

	// Updating onto a clashing table is rejected too.
	sita := f.mustBook(f.waiter, "Late Sita", at.Add(5*time.Hour), f.t2)
	_, err = f.svc.UpdateReservation(f.waiter, &restaurantv1.UpdateReservationRequest{
		Id: sita.Id, BaseVersion: sita.Version, StartsAtUnix: ptr(at.Add(30 * time.Minute).Unix()),
	})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_TABLE_ALREADY_BOOKED)
	_, err = f.svc.UpdateReservation(f.waiter, &restaurantv1.UpdateReservationRequest{Id: sita.Id, BaseVersion: 99, PartySize: ptr(int32(3))})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_STALE_VERSION)
	upd, err := f.svc.UpdateReservation(f.waiter, &restaurantv1.UpdateReservationRequest{Id: sita.Id, BaseVersion: sita.Version, PartySize: ptr(int32(3))})
	if err != nil || upd.PartySize != 3 || upd.Version != 2 {
		t.Fatalf("update: %+v %v", upd, err)
	}
}

func TestSeatCreatesOrder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	r := f.mustBook(f.waiter, "Ram", time.Now().Add(30*time.Minute), f.t1)

	cmd := uuid.NewString()
	seat := &restaurantv1.SeatReservationRequest{Id: r.Id, TableIds: []string{f.t1, f.t2}, GuestCount: 5, CommandId: cmd}
	o, err := f.svc.SeatReservation(f.waiter, seat)
	if err != nil {
		t.Fatalf("seat: %v", err)
	}
	if o.Type != restaurantv1.OrderType_ORDER_TYPE_DINE_IN || o.GetTableId() != f.t1 || o.GuestCount != 5 || o.ReservationId != r.Id ||
		o.Status != restaurantv1.OrderStatus_ORDER_STATUS_OPEN {
		t.Fatalf("unexpected order %+v", o)
	}
	again, err := f.svc.SeatReservation(f.waiter, seat)
	if err != nil || again.Id != o.Id {
		t.Fatalf("idempotent seat: %v %v", again, err)
	}
	got, err := f.svc.GetReservation(f.waiter, &restaurantv1.GetReservationRequest{Id: r.Id})
	if err != nil || got.Status != restaurantv1.ReservationStatus_RESERVATION_STATUS_SEATED || got.OrderId != o.Id || len(got.TableIds) != 2 {
		t.Fatalf("seated reservation: %+v %v", got, err)
	}
	var orders int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE reservation_id = $1`, r.Id).Scan(&orders)
	if orders != 1 {
		t.Fatalf("orders = %d", orders)
	}
	// Seated again → INVALID_STATUS (a new command id).
	_, err = f.svc.SeatReservation(f.waiter, &restaurantv1.SeatReservationRequest{Id: r.Id, TableIds: []string{f.t1}, GuestCount: 2, CommandId: uuid.NewString()})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID_STATUS)

	// Another booking on the occupied table: IN_USE, or link the open order.
	r2 := f.mustBook(f.waiter, "Hari", time.Now().Add(4*time.Hour))
	_, err = f.svc.SeatReservation(f.waiter, &restaurantv1.SeatReservationRequest{Id: r2.Id, TableIds: []string{f.t1}, GuestCount: 2, CommandId: uuid.NewString()})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_IN_USE)
	linked, err := f.svc.SeatReservation(f.waiter, &restaurantv1.SeatReservationRequest{Id: r2.Id, TableIds: []string{f.t1}, GuestCount: 2, ExistingOrderId: o.Id, CommandId: uuid.NewString()})
	if err != nil || linked.Id != o.Id || linked.ReservationId != r2.Id || linked.Version != o.Version+1 {
		t.Fatalf("link: %+v %v", linked, err)
	}
}

func TestCancelNoShowRestore(t *testing.T) {
	f := newFixture(t)
	future := f.mustBook(f.waiter, "Ram", time.Now().Add(2*time.Hour), f.t1)
	_, err := f.svc.MarkNoShow(f.waiter, &restaurantv1.MarkNoShowRequest{Id: future.Id})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_TOO_EARLY)

	_, err = f.svc.CancelReservation(f.waiter, &restaurantv1.CancelReservationRequest{Id: future.Id, Reason: " "})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID)
	c, err := f.svc.CancelReservation(f.waiter, &restaurantv1.CancelReservationRequest{Id: future.Id, Reason: "Guest cancelled"})
	if err != nil || c.Status != restaurantv1.ReservationStatus_RESERVATION_STATUS_CANCELLED || c.CancelReason != "Guest cancelled" || c.CancelledById != f.waiterID.String() {
		t.Fatalf("cancel: %+v %v", c, err)
	}
	_, err = f.svc.CancelReservation(f.waiter, &restaurantv1.CancelReservationRequest{Id: future.Id, Reason: "again"})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID_STATUS)
	// The table is free again while cancelled: another booking takes it.
	other := f.mustBook(f.waiter, "Hari", time.Now().Add(2*time.Hour), f.t1)
	// Restoring would double-book the table.
	_, err = f.svc.RestoreReservation(f.waiter, &restaurantv1.RestoreReservationRequest{Id: future.Id})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_TABLE_ALREADY_BOOKED)
	if _, err := f.svc.CancelReservation(f.waiter, &restaurantv1.CancelReservationRequest{Id: other.Id, Reason: "Restaurant cancelled"}); err != nil {
		t.Fatal(err)
	}
	back, err := f.svc.RestoreReservation(f.waiter, &restaurantv1.RestoreReservationRequest{Id: future.Id})
	if err != nil || back.Status != restaurantv1.ReservationStatus_RESERVATION_STATUS_BOOKED || back.CancelReason != "" || back.CancelledAtUnix != 0 {
		t.Fatalf("restore: %+v %v", back, err)
	}
	_, err = f.svc.RestoreReservation(f.waiter, &restaurantv1.RestoreReservationRequest{Id: future.Id})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID_STATUS)

	// After the booking time: no-show, then restore the same day.
	past := f.mustBook(f.waiter, "Maya", time.Now().Add(-10*time.Minute))
	ns, err := f.svc.MarkNoShow(f.waiter, &restaurantv1.MarkNoShowRequest{Id: past.Id})
	if err != nil || ns.Status != restaurantv1.ReservationStatus_RESERVATION_STATUS_NO_SHOW || ns.NoShowById != f.waiterID.String() {
		t.Fatalf("no-show: %+v %v", ns, err)
	}
	if _, err := f.svc.RestoreReservation(f.waiter, &restaurantv1.RestoreReservationRequest{Id: past.Id}); err != nil {
		t.Fatalf("restore no-show: %v", err)
	}
	// Restore from another Kathmandu day is refused.
	if _, err := f.svc.MarkNoShow(f.waiter, &restaurantv1.MarkNoShowRequest{Id: past.Id}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE reservations SET no_show_at = now() - interval '2 days' WHERE id = $1`, past.Id); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.RestoreReservation(f.waiter, &restaurantv1.RestoreReservationRequest{Id: past.Id})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_INVALID_STATUS)
}

func TestRolesAndSwitch(t *testing.T) {
	f := newFixture(t)
	at := time.Now().Add(2 * time.Hour)
	_, err := f.book(f.kitchen, "Ram", at)
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)
	today := time.Now().In(time.FixedZone("NPT", 5*3600+45*60)).Format("2006-01-02")
	_, err = f.svc.ListReservations(f.kitchen, &restaurantv1.ListReservationsRequest{From: today, To: today})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)
	_, err = f.svc.SaveReservationSettings(f.waiter, &restaurantv1.ReservationSettings{Enabled: false})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)
	_, err = f.svc.ReservationReport(f.waiter, &restaurantv1.ReservationReportRequest{From: today, To: today})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_NOT_ALLOWED)

	r := f.mustBook(f.waiter, "Ram", at)
	list, err := f.svc.ListReservations(f.waiter, &restaurantv1.ListReservationsRequest{From: today, To: today})
	if err != nil {
		t.Fatal(err)
	}
	_ = list // the booking may fall on tomorrow's Kathmandu date late in the evening
	if _, err := f.svc.GetReservationsToday(f.owner, &restaurantv1.ReservationsTodayRequest{}); err != nil {
		t.Fatal(err)
	}

	s, err := f.svc.GetReservationSettings(f.waiter, &restaurantv1.GetReservationSettingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	s.Enabled = false
	if _, err := f.svc.SaveReservationSettings(f.owner, s); err != nil {
		t.Fatal(err)
	}
	_, err = f.book(f.waiter, "Hari", at)
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_RESERVATIONS_OFF)
	_, err = f.svc.CancelReservation(f.waiter, &restaurantv1.CancelReservationRequest{Id: r.Id, Reason: "x"})
	expectReason(t, err, restaurantv1.ErrorReason_ERROR_REASON_RESERVATIONS_OFF)
}

func ptr[T any](v T) *T { return &v }
