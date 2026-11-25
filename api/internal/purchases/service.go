// Package purchases owns suppliers, purchase bills (goods received + the
// supplier's invoice in one record), supplier payments, purchase returns, the buy
// list and the purchase reports (spec: purchases-suppliers-design; web fake:
// lib/api/mock/mock-purchases-api.ts, the executable spec).
//
// Stock goes in through inventory.PostPurchase (PURCHASE movements + moving
// average cost) and out through inventory.PostPurchaseReturn. A supplier's balance
// is never stored (supplier_balances view): opening + recorded bills − returns −
// live payments + refunds handed back. Payments settle the oldest debt first.
// "From till" payments are paid-outs in the payer's open shift (shift reports
// subtract them from expected cash).
package purchases

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// Audit actions (the web's vocabulary).
const (
	ActionSupplierChanged = "SUPPLIER_CHANGED"
	ActionRecorded        = "PURCHASE_RECORDED"
	ActionVoided          = "PURCHASE_VOIDED"
	ActionReturned        = "PURCHASE_RETURNED"
	ActionPaid            = "SUPPLIER_PAID"
	ActionPaymentVoided   = "SUPPLIER_PAYMENT_VOIDED"
	ActionItemChanged     = "INVENTORY_ITEM_CHANGED"
)

// Realtime events.
const (
	EventPurchaseRecorded = "purchase.recorded"
	EventSupplierUpdated  = "supplier.updated"
)

// TillPayoutLimitPaisa: a cashier pays a supplier from the till up to this much per
// payment; above it a manager PIN is needed.
const TillPayoutLimitPaisa = 500_000

// Sanity bounds that keep the paisa arithmetic far from int64 overflow.
const (
	maxQtyMilli  = 1_000_000_000  // 1,000,000 purchase units
	maxUnitCost  = 10_000_000_000 // Rs 10 crore per unit
	maxLines     = 100
	maxMoneyBill = 10_000_000_000_000 // Rs 10,000 crore
)

var nepal = time.FixedZone("NPT", 5*3600+45*60)

type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	hub  *realtime.Hub
	auth *auth.Service
	log  *zap.Logger
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, authSvc *auth.Service, log *zap.Logger) *Service {
	if log == nil {
		log = zap.NewNop()
	}
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, auth: authSvc, log: log}
}

func (s *Service) emit(evs ...realtime.Event) {
	if s.hub == nil {
		return
	}
	for _, ev := range evs {
		s.hub.Broadcast(ev)
	}
}

// --- roles / errors ---

func requireBuyer(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
}

func isManager(a *auth.Actor) bool {
	return a.User.Role == auth.RoleOwner || a.User.Role == auth.RoleManager
}

func withField(e *connecterr.Error, field string) *connecterr.Error {
	e.Field = field
	return e
}

func duplicateSupplierBill() *connecterr.Error {
	return withField(connecterr.New(connect.CodeAlreadyExists, restaurantv1.ErrorReason_ERROR_REASON_DUPLICATE_SUPPLIER_BILL,
		"this supplier bill is already recorded"), "supplier_bill_no")
}

func notAVatBill() *connecterr.Error {
	return withField(connecterr.New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_NOT_A_VAT_BILL,
		"a VAT bill needs a VAT-registered supplier with a PAN and a bill number"), "is_vat_bill")
}

func paymentTooHigh() *connecterr.Error {
	return withField(connecterr.New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_PAYMENT_TOO_HIGH,
		"the payment is more than what is owed"), "amount")
}

func returnTooHigh() *connecterr.Error {
	return withField(connecterr.New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_RETURN_TOO_HIGH,
		"more than was bought less what is already returned"), "lines")
}

func parseID(field, v string) (uuid.UUID, error) {
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

func parseDate(field, v string) (time.Time, error) {
	d, err := time.ParseInLocation(time.DateOnly, v, nepal)
	if err != nil {
		return time.Time{}, connecterr.Invalid(field, "expected a date as YYYY-MM-DD")
	}
	return d, nil
}

func pgDate(t time.Time) pgtype.Date {
	return pgtype.Date{Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
}

func dateStr(d pgtype.Date) string { return d.Time.Format(time.DateOnly) }

func ktmDate(t time.Time) string { return t.In(nepal).Format(time.DateOnly) }

func idStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return uuid.UUID(u.Bytes).String()
}

func unixOf(t pgtype.Timestamptz) int64 {
	if !t.Valid {
		return 0
	}
	return t.Time.Unix()
}

func local(en, ne string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en, Ne: ne} }

func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// lockOpenShift returns the actor's open shift, share-locked so a concurrent
// close waits for this transaction (and counts what it records).
func lockOpenShift(ctx context.Context, q *sqlc.Queries, actorID uuid.UUID, msg string) (sqlc.Shift, error) {
	sh, err := q.LockOpenShiftForUser(ctx, actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.Shift{}, connecterr.ShiftNotOpen(msg)
	}
	if err != nil {
		return sqlc.Shift{}, connecterr.Internal(err)
	}
	return sh, nil
}

// --- suppliers ---

var panRe = regexp.MustCompile(`^\d{9}$`)

func cleanSupplier(in *restaurantv1.SupplierInput) (sqlc.UpdateSupplierParams, error) {
	var p sqlc.UpdateSupplierParams
	p.Name = strings.TrimSpace(in.GetName())
	if p.Name == "" || utf8.RuneCountInString(p.Name) > 80 {
		return p, connecterr.Invalid("name", "a name of up to 80 characters is required")
	}
	phone, ok := auth.NormalisePhone(in.GetPhone())
	if !ok {
		return p, connecterr.Invalid("phone", "not a phone number")
	}
	p.Phone = phone
	p.Pan = strings.TrimSpace(in.GetPan())
	if p.Pan != "" && !panRe.MatchString(p.Pan) {
		return p, connecterr.Invalid("pan", "a PAN is 9 digits")
	}
	if in.GetVatRegistered() && p.Pan == "" {
		return p, connecterr.Invalid("pan", "a VAT-registered supplier needs a PAN")
	}
	p.VatRegistered = in.GetVatRegistered()
	p.Address = strings.TrimSpace(in.GetAddress())
	p.ContactPerson = strings.TrimSpace(in.GetContactPerson())
	p.Notes = strings.TrimSpace(in.GetNotes())
	return p, nil
}

func uniqueName(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return connecterr.Duplicate("name", "another supplier has this name")
	}
	return connecterr.Internal(err)
}

func toSupplier(x sqlc.Supplier, balance int64) *restaurantv1.Supplier {
	return &restaurantv1.Supplier{
		Id: x.ID.String(), Name: x.Name, Phone: x.Phone, Address: x.Address, Pan: x.Pan, VatRegistered: x.VatRegistered,
		ContactPerson: x.ContactPerson, Notes: x.Notes, Active: x.Active, OpeningBalancePaisa: x.OpeningBalancePaisa,
		CreatedById: idStr(x.CreatedByUserID), CreatedAtUnix: x.CreatedAt.Unix(), BalancePaisa: balance,
	}
}

func supplierWithBalance(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (*restaurantv1.Supplier, error) {
	r, err := q.GetSupplierWithBalance(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, connecterr.NotFound("supplier not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return toSupplier(sqlc.Supplier{
		ID: r.ID, Name: r.Name, Phone: r.Phone, Address: r.Address, Pan: r.Pan, VatRegistered: r.VatRegistered,
		ContactPerson: r.ContactPerson, Notes: r.Notes, Active: r.Active, OpeningBalancePaisa: r.OpeningBalancePaisa,
		CreatedByUserID: r.CreatedByUserID, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}, r.BalancePaisa), nil
}

func matches(x sqlc.ListSuppliersWithBalanceRow, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" || strings.Contains(strings.ToLower(x.Name), q) {
		return true
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, q)
	return len(digits) >= 3 && (strings.Contains(x.Phone, digits) || strings.Contains(x.Pan, digits))
}

func (s *Service) ListSuppliers(ctx context.Context, req *restaurantv1.ListSuppliersRequest) (*restaurantv1.ListSuppliersResponse, error) {
	if _, err := requireBuyer(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListSuppliersWithBalance(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.ListSuppliersResponse{Suppliers: []*restaurantv1.Supplier{}}
	for _, r := range rows {
		if (!r.Active && !req.GetIncludeInactive()) || !matches(r, req.GetQuery()) {
			continue
		}
		out.Suppliers = append(out.Suppliers, toSupplier(sqlc.Supplier{
			ID: r.ID, Name: r.Name, Phone: r.Phone, Address: r.Address, Pan: r.Pan, VatRegistered: r.VatRegistered,
			ContactPerson: r.ContactPerson, Notes: r.Notes, Active: r.Active, OpeningBalancePaisa: r.OpeningBalancePaisa,
			CreatedByUserID: r.CreatedByUserID, CreatedAt: r.CreatedAt,
		}, r.BalancePaisa))
	}
	return out, nil
}

func (s *Service) GetSupplier(ctx context.Context, id string) (*restaurantv1.Supplier, error) {
	if _, err := requireBuyer(ctx); err != nil {
		return nil, err
	}
	sid, err := parseID("id", id)
	if err != nil {
		return nil, err
	}
	return supplierWithBalance(ctx, s.q, sid)
}

// CreateSupplier: owner, manager, cashier; only owner/manager set an opening balance.
func (s *Service) CreateSupplier(ctx context.Context, req *restaurantv1.CreateSupplierRequest) (*restaurantv1.Supplier, error) {
	actor, err := requireBuyer(ctx)
	if err != nil {
		return nil, err
	}
	opening := req.GetOpeningBalancePaisa()
	if opening < 0 {
		return nil, connecterr.Invalid("opening_balance", "the opening balance can't be negative")
	}
	if opening > 0 && !isManager(actor) {
		return nil, withField(connecterr.NotAllowed("only an owner or manager sets an opening balance"), "opening_balance")
	}
	p, err := cleanSupplier(req.GetInput())
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.Supplier
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		x, e := q.CreateSupplier(ctx, sqlc.CreateSupplierParams{
			Name: p.Name, Phone: p.Phone, Address: p.Address, Pan: p.Pan, VatRegistered: p.VatRegistered,
			ContactPerson: p.ContactPerson, Notes: p.Notes, OpeningBalancePaisa: opening, CreatedByUserID: dbx.UUID(actor.User.ID),
		})
		if e != nil {
			return uniqueName(e)
		}
		reason := "created"
		if opening > 0 {
			reason = "created · opening balance " + itoa(opening) + " paisa"
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionSupplierChanged, Entity: "supplier", EntityID: x.ID.String(),
			Reason: x.Name + " · " + reason, After: audit.JSON(p)}); e != nil {
			return e
		}
		out = toSupplier(x, opening)
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventSupplierUpdated, ID: out.Id})
	return out, nil
}

// UpdateSupplier: owner/manager. The opening balance can't change once set.
func (s *Service) UpdateSupplier(ctx context.Context, req *restaurantv1.UpdateSupplierRequest) (*restaurantv1.Supplier, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	p, err := cleanSupplier(req.GetInput())
	if err != nil {
		return nil, err
	}
	p.ID = id
	var out *restaurantv1.Supplier
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		before, e := q.LockSupplier(ctx, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("supplier not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		x, e := q.UpdateSupplier(ctx, p)
		if e != nil {
			return uniqueName(e)
		}
		var changed []string
		for _, c := range []struct {
			name string
			eq   bool
		}{
			{"name", before.Name == x.Name}, {"phone", before.Phone == x.Phone}, {"pan", before.Pan == x.Pan},
			{"vatRegistered", before.VatRegistered == x.VatRegistered}, {"address", before.Address == x.Address},
			{"contactPerson", before.ContactPerson == x.ContactPerson}, {"notes", before.Notes == x.Notes},
		} {
			if !c.eq {
				changed = append(changed, c.name)
			}
		}
		if len(changed) > 0 {
			if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionSupplierChanged, Entity: "supplier", EntityID: id.String(),
				Reason: x.Name + " · changed " + strings.Join(changed, ", "), Before: audit.JSON(before), After: audit.JSON(p)}); e != nil {
				return e
			}
		}
		out, e = supplierWithBalance(ctx, q, id)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventSupplierUpdated, ID: out.Id})
	return out, nil
}

// SetSupplierActive: owner/manager. Deactivate, never delete.
func (s *Service) SetSupplierActive(ctx context.Context, req *restaurantv1.SetSupplierActiveRequest) (*restaurantv1.Supplier, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.Supplier
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		before, e := q.LockSupplier(ctx, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("supplier not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if before.Active != req.GetActive() {
			if _, e := q.SetSupplierActive(ctx, sqlc.SetSupplierActiveParams{ID: id, Active: req.GetActive()}); e != nil {
				return connecterr.Internal(e)
			}
			what := "deactivated"
			if req.GetActive() {
				what = "activated"
			}
			if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionSupplierChanged, Entity: "supplier", EntityID: id.String(),
				Reason: before.Name + " · " + what}); e != nil {
				return e
			}
		}
		out, e = supplierWithBalance(ctx, q, id)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventSupplierUpdated, ID: out.Id})
	return out, nil
}

// --- balances and oldest-first allocation ---

type debt struct {
	id     uuid.UUID // uuid.Nil = the opening balance
	date   string
	at     time.Time
	amount int64
}

// debts: what the supplier is owed for, oldest first: the opening balance, then
// recorded bills net of returns.
func debts(ctx context.Context, q *sqlc.Queries, sup sqlc.Supplier) ([]debt, error) {
	rows, err := q.ListSupplierDebts(ctx, sup.ID)
	if err != nil {
		return nil, err
	}
	out := make([]debt, 0, len(rows)+1)
	if sup.OpeningBalancePaisa > 0 {
		out = append(out, debt{date: ktmDate(sup.CreatedAt), at: sup.CreatedAt, amount: sup.OpeningBalancePaisa})
	}
	bills := make([]debt, 0, len(rows))
	for _, r := range rows {
		bills = append(bills, debt{id: r.ID, date: dateStr(r.BillDate), at: r.At, amount: r.AmountPaisa})
	}
	slices.SortStableFunc(bills, func(a, b debt) int {
		if c := strings.Compare(a.date, b.date); c != 0 {
			return c
		}
		return a.at.Compare(b.at)
	})
	return append(out, bills...), nil
}

// allocate: payments (less refunds) settle the oldest debt first; debt id → settled.
func allocate(ctx context.Context, q *sqlc.Queries, sup sqlc.Supplier) ([]debt, map[uuid.UUID]int64, error) {
	ds, err := debts(ctx, q, sup)
	if err != nil {
		return nil, nil, err
	}
	pool, err := q.SupplierPaidNet(ctx, sup.ID)
	if err != nil {
		return nil, nil, err
	}
	settled := make(map[uuid.UUID]int64, len(ds))
	for _, d := range ds {
		part := max(0, min(pool, d.amount))
		pool -= part
		settled[d.id] = part
	}
	return ds, settled, nil
}

func itoa(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	var b [24]byte
	i := len(b)
	for {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
		if n == 0 {
			break
		}
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
