// Package credit owns credit customers (udharo): accounts with a limit, the
// on-account check billing runs while issuing a bill, repayments (counted in the
// collector's shift, own receipt sequence), statements, and the credit report.
//
// A customer's balance is never stored: it is the on-account payments on ISSUED
// invoices minus the repayments not voided (the customer_balances view), so
// cancelling a bill reverses its debt and a negative balance is credit in the
// customer's favour. Repayments pay off the oldest bills first.
package credit

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
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
	"google.golang.org/protobuf/proto"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// Audit actions (the web maps them to CUSTOMER_CHANGED, CREDIT_LIMIT_CHANGED,
// CREDIT_GIVEN, REPAYMENT_TAKEN, REPAYMENT_VOIDED).
const (
	ActionCustomerChange = "customer.change"
	ActionLimitChange    = "customer.credit_limit"
	ActionCreditGiven    = "credit.give"
	ActionRepay          = "credit.repay"
	ActionRepayVoid      = "credit.repay_void"
)

// EventCustomerUpdated is the realtime poke for a customer's account or balance.
const EventCustomerUpdated = "customer.updated"

var nepal = time.FixedZone("NPT", 5*3600+45*60)

type Service struct {
	pool *pgxpool.Pool
	q    *sqlc.Queries
	hub  *realtime.Hub
	log  *zap.Logger
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, log: log}
}

func (s *Service) emit(ev realtime.Event) {
	if s.hub != nil {
		s.hub.Broadcast(ev)
	}
}

// --- errors ---

func withField(e *connecterr.Error, field string) *connecterr.Error {
	e.Field = field
	return e
}

// CreditLimitExceeded: the on-account amount is over the available credit and no
// manager approved it.
func CreditLimitExceeded(msg string) *connecterr.Error {
	return withField(connecterr.New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_CREDIT_LIMIT_EXCEEDED, msg), "customer_id")
}

func customerInactive() *connecterr.Error {
	return withField(connecterr.New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_CUSTOMER_INACTIVE, "this account is deactivated"), "customer_id")
}

func repaymentTooHigh(balance int64) *connecterr.Error {
	return withField(connecterr.New(connect.CodeFailedPrecondition, restaurantv1.ErrorReason_ERROR_REASON_REPAYMENT_TOO_HIGH,
		fmt.Sprintf("the customer owes %d paisa", max(balance, 0))), "amount")
}

func notFound(field, msg string) *connecterr.Error {
	return withField(connecterr.NotFound(msg), field)
}

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

// requireBiller admits the roles that bill: owner, manager, cashier.
func requireBiller(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleCashier)
}

func isManager(a *auth.Actor) bool {
	return a.User.Role == auth.RoleOwner || a.User.Role == auth.RoleManager
}

// --- customers ---

var panRe = regexp.MustCompile(`^\d{9}$`)

type cleaned struct {
	name, phone, notes, pan, address, contact string
}

// clean validates and trims the editable fields (selfID = the account being
// edited, uuid.Nil on create).
func clean(ctx context.Context, q *sqlc.Queries, in *restaurantv1.CustomerInput, selfID uuid.UUID) (cleaned, error) {
	c := cleaned{
		name: strings.TrimSpace(in.GetName()), notes: strings.TrimSpace(in.GetNotes()), pan: strings.TrimSpace(in.GetPan()),
		address: strings.TrimSpace(in.GetAddress()), contact: strings.TrimSpace(in.GetContactPerson()),
	}
	if c.name == "" || utf8.RuneCountInString(c.name) > 80 {
		return c, connecterr.Invalid("name", "a name of up to 80 characters is required")
	}
	phone, ok := auth.NormalisePhone(in.GetPhone())
	if !ok {
		return c, connecterr.Invalid("phone", "not a Nepali phone number")
	}
	c.phone = phone
	if c.pan != "" && !panRe.MatchString(c.pan) {
		return c, connecterr.Invalid("pan", "PAN is 9 digits")
	}
	if phone != "" {
		id, err := q.CustomerIDByPhone(ctx, phone)
		if err == nil && id != selfID {
			return c, connecterr.Duplicate("phone", "another customer has this phone")
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return c, connecterr.Internal(err)
		}
	}
	return c, nil
}

// uniquePhone maps a race on the phone's unique index to DUPLICATE "phone".
func uniquePhone(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return connecterr.Duplicate("phone", "another customer has this phone")
	}
	return connecterr.Internal(err)
}

func (s *Service) ListCustomers(ctx context.Context, req *restaurantv1.ListCustomersRequest) (*restaurantv1.ListCustomersResponse, error) {
	if _, err := requireBiller(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListCustomersWithBalance(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.ListCustomersResponse{Customers: []*restaurantv1.Customer{}}
	for _, r := range rows {
		// Loyalty-only members (no credit account) are not browsable here.
		if r.CreditAccount && (req.GetIncludeInactive() || r.Active) && matches(r.Name, r.Phone, req.GetQuery()) {
			out.Customers = append(out.Customers, toCustomer(sqlc.GetCustomerWithBalanceRow(r)))
		}
	}
	return out, nil
}

var countryCodeRe = regexp.MustCompile(`^(\+|00)977`)

// matches: name contains the query (any case), or the phone contains its digits
// (3 or more, without a +977 / 00977 prefix).
func matches(name, phone, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" || strings.Contains(strings.ToLower(name), q) {
		return true
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, q)
	if countryCodeRe.MatchString(q) {
		digits = strings.TrimPrefix(strings.TrimPrefix(digits, "00"), "977")
	}
	return len(digits) >= 3 && strings.Contains(phone, digits)
}

func (s *Service) GetCustomer(ctx context.Context, req *restaurantv1.GetCustomerRequest) (*restaurantv1.Customer, error) {
	if _, err := requireBiller(ctx); err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	return s.load(ctx, s.q, id)
}

func (s *Service) load(ctx context.Context, q *sqlc.Queries, id uuid.UUID) (*restaurantv1.Customer, error) {
	c, err := q.GetCustomerWithBalance(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("id", "customer not found")
	}
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	return toCustomer(c), nil
}

func (s *Service) CreateCustomer(ctx context.Context, req *restaurantv1.CreateCustomerRequest) (*restaurantv1.Customer, error) {
	actor, err := requireBiller(ctx)
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.Customer{}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		// Loyalty: a member without a credit account gets one (same record, one per phone).
		if phone, ok := auth.NormalisePhone(req.GetCustomer().GetPhone()); ok && phone != "" {
			if id, e := q.CustomerIDByPhone(ctx, phone); e == nil {
				if m, e := q.LockCustomer(ctx, id); e == nil && !m.CreditAccount {
					return s.openAccount(ctx, q, actor, m, req.GetCustomer(), out)
				}
			}
		}
		f, e := clean(ctx, q, req.GetCustomer(), uuid.Nil)
		if e != nil {
			return e
		}
		c, e := q.CreateCustomer(ctx, sqlc.CreateCustomerParams{
			Name: f.name, Phone: f.phone, Notes: f.notes, Pan: f.pan, Address: f.address, ContactPerson: f.contact,
			CreatedByUserID: dbx.UUID(actor.User.ID),
		})
		if e != nil {
			return uniquePhone(e)
		}
		if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionCustomerChange, Entity: "customer", EntityID: c.ID.String(),
			After: audit.JSON(map[string]any{"detail": "created"})}); e != nil {
			return e
		}
		got, e := s.load(ctx, q, c.ID)
		if e != nil {
			return e
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventCustomerUpdated, ID: out.Id})
	return out, nil
}

func (s *Service) UpdateCustomer(ctx context.Context, req *restaurantv1.UpdateCustomerRequest) (*restaurantv1.Customer, error) {
	actor, err := requireBiller(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	limit := req.GetCreditLimitPaisa()
	if limit < 0 {
		return nil, connecterr.Invalid("credit_limit", "the limit can't be negative")
	}
	out := &restaurantv1.Customer{}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		old, e := q.LockCustomer(ctx, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return notFound("id", "customer not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		f, e := clean(ctx, q, req.GetCustomer(), id)
		if e != nil {
			return e
		}
		limitChanged := limit != old.CreditLimitPaisa
		if limitChanged && !isManager(actor) {
			return withField(connecterr.NotAllowed("only an owner or manager changes the credit limit"), "credit_limit")
		}
		if _, e := q.UpdateCustomer(ctx, sqlc.UpdateCustomerParams{
			ID: id, Name: f.name, Phone: f.phone, Notes: f.notes, Pan: f.pan, Address: f.address, ContactPerson: f.contact, CreditLimitPaisa: limit,
		}); e != nil {
			return uniquePhone(e)
		}
		// A credit limit given to a loyalty-only member opens their credit account.
		if limitChanged && limit > 0 && !old.CreditAccount {
			if e := q.OpenCreditAccount(ctx, id); e != nil {
				return connecterr.Internal(e)
			}
		}
		var changed []string
		for _, c := range []struct {
			name     string
			old, new string
		}{
			{"name", old.Name, f.name}, {"phone", old.Phone, f.phone}, {"notes", old.Notes, f.notes}, {"pan", old.Pan, f.pan},
			{"address", old.Address, f.address}, {"contact_person", old.ContactPerson, f.contact},
		} {
			if c.old != c.new {
				changed = append(changed, c.name)
			}
		}
		if len(changed) > 0 {
			if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionCustomerChange, Entity: "customer", EntityID: id.String(),
				After: audit.JSON(map[string]any{"detail": "changed " + strings.Join(changed, ", ")})}); e != nil {
				return e
			}
		}
		if limitChanged {
			if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionLimitChange, Entity: "customer", EntityID: id.String(),
				Before: audit.JSON(map[string]any{"credit_limit": old.CreditLimitPaisa}),
				After:  audit.JSON(map[string]any{"credit_limit": limit, "detail": fmt.Sprintf("Rs %s → Rs %s", rupees(old.CreditLimitPaisa), rupees(limit))})}); e != nil {
				return e
			}
		}
		got, e := s.load(ctx, q, id)
		if e != nil {
			return e
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventCustomerUpdated, ID: out.Id})
	return out, nil
}

func (s *Service) SetCustomerActive(ctx context.Context, req *restaurantv1.SetCustomerActiveRequest) (*restaurantv1.Customer, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("id", req.GetId())
	if err != nil {
		return nil, err
	}
	out := &restaurantv1.Customer{}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		old, e := q.LockCustomer(ctx, id)
		if errors.Is(e, pgx.ErrNoRows) {
			return notFound("id", "customer not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		if old.Active != req.GetActive() {
			if _, e := q.SetCustomerActive(ctx, sqlc.SetCustomerActiveParams{ID: id, Active: req.GetActive()}); e != nil {
				return connecterr.Internal(e)
			}
			detail := "deactivated"
			if req.GetActive() {
				detail = "activated"
			}
			if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionCustomerChange, Entity: "customer", EntityID: id.String(),
				After: audit.JSON(map[string]any{"detail": detail})}); e != nil {
				return e
			}
		}
		got, e := s.load(ctx, q, id)
		if e != nil {
			return e
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventCustomerUpdated, ID: out.Id})
	return out, nil
}

// --- ledger (balance, oldest-first allocation) ---

// charge is one issued bill's on-account amount for a customer.
type charge struct {
	sqlc.ListCreditChargesRow
	settled int64
}

// allocate pays the charges (oldest first) from the total repaid.
func allocate(charges []sqlc.ListCreditChargesRow, repaid int64) []charge {
	out := make([]charge, 0, len(charges))
	for _, c := range charges {
		settled := max(min(repaid, c.AmountPaisa), 0)
		repaid -= settled
		out = append(out, charge{ListCreditChargesRow: c, settled: settled})
	}
	return out
}

func sumRepaid(rs []sqlc.Repayment) int64 {
	var n int64
	for _, r := range rs {
		n += r.AmountPaisa
	}
	return n
}

// CreditReport lists every account whose balance is not zero, oldest unpaid bill
// first (accounts with none last), then by name. The caller checks the role.
func CreditReport(ctx context.Context, q *sqlc.Queries) ([]*restaurantv1.CreditReportRow, error) {
	customers, err := q.ListCustomersWithBalance(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	charges, err := q.ListCreditCharges(ctx, pgtype.UUID{})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	repayments, err := q.ListLiveRepayments(ctx, pgtype.UUID{})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	chargesBy := map[uuid.UUID][]sqlc.ListCreditChargesRow{}
	for _, c := range charges {
		chargesBy[c.CustomerID] = append(chargesBy[c.CustomerID], c)
	}
	repaysBy := map[uuid.UUID][]sqlc.Repayment{}
	for _, r := range repayments {
		repaysBy[r.CustomerID] = append(repaysBy[r.CustomerID], r)
	}
	rows := []*restaurantv1.CreditReportRow{}
	for _, c := range customers {
		if c.BalancePaisa == 0 {
			continue
		}
		row := &restaurantv1.CreditReportRow{
			CustomerId: c.ID.String(), Name: c.Name, Phone: c.Phone, Active: c.Active,
			BalancePaisa: c.BalancePaisa, CreditLimitPaisa: c.CreditLimitPaisa,
		}
		rs := repaysBy[c.ID]
		for _, ch := range allocate(chargesBy[c.ID], sumRepaid(rs)) {
			if ch.settled < ch.AmountPaisa {
				row.OldestUnpaidAtUnix = ch.AdDatetime.Unix()
				break
			}
		}
		if len(rs) > 0 {
			row.LastRepaymentAtUnix = rs[len(rs)-1].CreatedAt.Unix()
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].OldestUnpaidAtUnix, rows[j].OldestUnpaidAtUnix
		if (a == 0) != (b == 0) {
			return b == 0
		}
		if a != b {
			return a < b
		}
		return strings.ToLower(rows[i].Name) < strings.ToLower(rows[j].Name)
	})
	return rows, nil
}

// --- converters ---

func uuidStr(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return uuid.UUID(id.Bytes).String()
}

func toCustomer(c sqlc.GetCustomerWithBalanceRow) *restaurantv1.Customer {
	out := &restaurantv1.Customer{
		Id: c.ID.String(), Name: c.Name, Phone: c.Phone, Notes: c.Notes, CreditLimitPaisa: c.CreditLimitPaisa, Active: c.Active,
		Pan: c.Pan, Address: c.Address, ContactPerson: c.ContactPerson, CreatedByUserId: uuidStr(c.CreatedByUserID),
		CreatedAtUnix: c.CreatedAt.Unix(), BalancePaisa: c.BalancePaisa,
		CreditAccount: c.CreditAccount, LoyaltyMember: c.LoyaltyMember, LoyaltyJoinedByUserId: uuidStr(c.LoyaltyJoinedByUserID),
		CardToken: c.CardToken.String,
	}
	if c.LoyaltyJoinedAt.Valid {
		out.LoyaltyJoinedAtUnix = c.LoyaltyJoinedAt.Time.Unix()
	}
	if c.LoyaltyConsentAt.Valid {
		out.LoyaltyConsentAtUnix = c.LoyaltyConsentAt.Time.Unix()
	}
	if c.BirthMonthBs.Valid {
		out.BirthMonthBs = int32(c.BirthMonthBs.Int16)
	}
	return out
}

// openAccount turns a loyalty-only member into a credit account (CreateCustomer
// with their phone): the input's fields are saved on the same record.
func (s *Service) openAccount(ctx context.Context, q *sqlc.Queries, actor *auth.Actor, m sqlc.Customer, in *restaurantv1.CustomerInput, out *restaurantv1.Customer) error {
	f, e := clean(ctx, q, in, m.ID)
	if e != nil {
		return e
	}
	if _, e := q.UpdateCustomer(ctx, sqlc.UpdateCustomerParams{
		ID: m.ID, Name: f.name, Phone: f.phone, Notes: f.notes, Pan: f.pan, Address: f.address, ContactPerson: f.contact, CreditLimitPaisa: m.CreditLimitPaisa,
	}); e != nil {
		return uniquePhone(e)
	}
	if e := q.OpenCreditAccount(ctx, m.ID); e != nil {
		return connecterr.Internal(e)
	}
	if e := audit.Write(ctx, q, audit.Entry{ActorID: actor.User.ID, Action: ActionCustomerChange, Entity: "customer", EntityID: m.ID.String(),
		After: audit.JSON(map[string]any{"detail": "credit account opened"})}); e != nil {
		return e
	}
	got, e := s.load(ctx, q, m.ID)
	if e != nil {
		return e
	}
	proto.Merge(out, got)
	return nil
}

func rupees(paisa int64) string {
	sign := ""
	if paisa < 0 {
		sign, paisa = "-", -paisa
	}
	return fmt.Sprintf("%s%d.%02d", sign, paisa/100, paisa%100)
}

// RepaymentNumber is a repayment receipt's printed number: "R-12".
func RepaymentNumber(n int32) string { return fmt.Sprintf("R-%d", n) }

// MethodEnum / MethodStr convert between the stored payment method and the proto enum.
func MethodEnum(s string) restaurantv1.PaymentMethod {
	switch s {
	case "CASH":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH
	case "CARD":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD
	case "QR_FONEPAY":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY
	case "QR_OTHER":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_OTHER
	case "ON_ACCOUNT":
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_ON_ACCOUNT
	default:
		return restaurantv1.PaymentMethod_PAYMENT_METHOD_UNSPECIFIED
	}
}

func MethodStr(m restaurantv1.PaymentMethod) string {
	switch m {
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_CASH:
		return "CASH"
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_CARD:
		return "CARD"
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_FONEPAY:
		return "QR_FONEPAY"
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_QR_OTHER:
		return "QR_OTHER"
	case restaurantv1.PaymentMethod_PAYMENT_METHOD_ON_ACCOUNT:
		return "ON_ACCOUNT"
	default:
		return ""
	}
}
