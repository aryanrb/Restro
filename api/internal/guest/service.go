// Package guest owns guest table ordering (QR menu): the PUBLIC GuestService,
// identified only by a table's guest code and never reaching a staff function, and
// the staff GuestDeskService (accept / reject / resolve, per-table setup and codes).
// A guest order is priced by the server at submit (orders.PriceLine) and kept as
// a snapshot until a staff member accepts it into the table's open order, which
// sends it to the kitchen exactly like a waiter's send (orders.InsertLines, stock
// included). Mirrors the web fake (web/src/lib/api/mock/mock-guest-api.ts).
package guest

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// Audit actions (the web maps them to GUEST_ORDER_ACCEPTED, GUEST_ORDER_REJECTED,
// GUEST_CODE_CHANGED and SETTINGS_CHANGED).
const (
	ActionAccept   = "guest.accept"
	ActionReject   = "guest.reject"
	ActionCode     = "guest.code"
	ActionSettings = "guest.settings"
)

// Realtime events: EventRequest (id = guest request id) for staff screens,
// EventStatus (id = table id) for the guest page.
const (
	EventRequest = "guest.request"
	EventStatus  = "guest.status"
)

// Limits (spec "Server rules"; starting values).
const (
	MaxLines        = 30
	MaxQuantity     = 20
	MaxPending      = 3
	MaxNote         = 140
	MaxReason       = 200
	OrdersPerMinute = 5  // per table
	CallsPerMinute  = 10 // per table
	// Per client address: a little more than one table's worth.
	ClientOrdersPerMinute = 10
	ClientCallsPerMinute  = 20
	recent                = 15 * time.Minute
)

const (
	kindOrder      = "ORDER"
	kindCallWaiter = "CALL_WAITER"
	kindBill       = "BILL"

	statusPending  = "PENDING"
	statusAccepted = "ACCEPTED"
	statusRejected = "REJECTED"
	statusDone     = "DONE"
)

type Service struct {
	pool    *pgxpool.Pool
	q       *sqlc.Queries
	hub     *realtime.Hub
	log     *zap.Logger
	now     func() time.Time
	limiter *limiter
}

func NewService(pool *pgxpool.Pool, hub *realtime.Hub, log *zap.Logger) *Service {
	return &Service{pool: pool, q: sqlc.New(pool), hub: hub, log: log, now: time.Now, limiter: newLimiter()}
}

func (s *Service) emit(evs ...realtime.Event) {
	if s.hub == nil {
		return
	}
	for _, ev := range evs {
		s.hub.Broadcast(ev)
	}
}

func requestEvents(r sqlc.GuestRequest) []realtime.Event {
	return []realtime.Event{{Type: EventRequest, ID: r.ID.String()}, {Type: EventStatus, ID: r.TableID.String()}}
}

// --- errors ---

func failed(reason restaurantv1.ErrorReason, field, msg string) *connecterr.Error {
	e := connecterr.New(connect.CodeFailedPrecondition, reason, msg)
	e.Field = field
	return e
}

func tableCodeInvalid() *connecterr.Error {
	return connecterr.New(connect.CodeNotFound, restaurantv1.ErrorReason_ERROR_REASON_TABLE_CODE_INVALID, "this table code is not valid")
}

func orderingOff() *connecterr.Error {
	return failed(restaurantv1.ErrorReason_ERROR_REASON_GUEST_ORDERING_OFF, "", "ordering from the table is switched off")
}

func rateLimited() *connecterr.Error {
	return connecterr.New(connect.CodeResourceExhausted, restaurantv1.ErrorReason_ERROR_REASON_RATE_LIMITED, "too many requests; try again in a minute")
}

func alreadyHandled(by string) *connecterr.Error {
	return failed(restaurantv1.ErrorReason_ERROR_REASON_ALREADY_HANDLED, "", by)
}

func item86d(field, msg string) *connecterr.Error {
	return failed(restaurantv1.ErrorReason_ERROR_REASON_ITEM_86D, field, msg)
}

func parseID(field, s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, connecterr.Invalid(field, "invalid id")
	}
	return id, nil
}

// --- roles ---

// requireDesk: the floor and counter roles (everyone but kitchen).
func requireDesk(ctx context.Context) (*auth.Actor, error) {
	return auth.RequireRole(ctx, auth.RoleOwner, auth.RoleManager, auth.RoleWaiter, auth.RoleCashier)
}

// --- the guest's table ---

// tableByCode resolves a guest code to its active table, else TABLE_CODE_INVALID.
func tableByCode(ctx context.Context, q *sqlc.Queries, code string) (sqlc.DiningTable, error) {
	if code == "" || len(code) > 64 {
		return sqlc.DiningTable{}, tableCodeInvalid()
	}
	t, err := q.GuestTableByCode(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.DiningTable{}, tableCodeInvalid()
	}
	if err != nil {
		return sqlc.DiningTable{}, connecterr.Internal(err)
	}
	return t, nil
}

// orderingOn: the restaurant-wide switch and the table's own.
func orderingOn(ctx context.Context, q *sqlc.Queries, t sqlc.DiningTable) (bool, sqlc.Setting, error) {
	st, err := q.GetSettings(ctx)
	if err != nil {
		return false, sqlc.Setting{}, connecterr.Internal(err)
	}
	return st.GuestOrdering && t.GuestOrdering, st, nil
}

// newCode is a random, unguessable table code (16 base32 chars, 80 bits).
func newCode() string { return rand.Text()[:16] }

// --- stored lines (guest_requests.lines) ---

type lineMod struct {
	NameEn     string `json:"name_en"`
	NameNe     string `json:"name_ne"`
	PriceDelta int64  `json:"price_delta"`
}

type storedLine struct {
	LineID        string    `json:"line_id"`
	MenuItemID    string    `json:"menu_item_id"`
	VariantID     string    `json:"variant_id,omitempty"`
	ModifierIDs   []string  `json:"modifier_ids"`
	Quantity      int32     `json:"quantity"`
	Note          string    `json:"note"`
	NameEn        string    `json:"name_en"`
	NameNe        string    `json:"name_ne"`
	HasVariant    bool      `json:"has_variant"`
	VariantNameEn string    `json:"variant_name_en,omitempty"`
	VariantNameNe string    `json:"variant_name_ne,omitempty"`
	Modifiers     []lineMod `json:"modifiers"`
	UnitPrice     int64     `json:"unit_price"`
}

func decodeLines(b []byte) []storedLine {
	var out []storedLine
	_ = json.Unmarshal(b, &out)
	return out
}

func encodeLines(lines []storedLine) []byte {
	if lines == nil {
		lines = []storedLine{}
	}
	b, _ := json.Marshal(lines)
	return b
}

func local(en, ne string) *restaurantv1.LocalText { return &restaurantv1.LocalText{En: en, Ne: ne} }

func toGuestLine(l storedLine) *restaurantv1.GuestLine {
	out := &restaurantv1.GuestLine{
		LineId: l.LineID, MenuItemId: l.MenuItemID, ModifierIds: append([]string{}, l.ModifierIDs...), Quantity: l.Quantity,
		Note: l.Note, Name: local(l.NameEn, l.NameNe), UnitPricePaisa: l.UnitPrice, Modifiers: []*restaurantv1.GuestLineModifier{},
	}
	if l.VariantID != "" {
		v := l.VariantID
		out.VariantId = &v
	}
	if l.HasVariant {
		out.VariantName = local(l.VariantNameEn, l.VariantNameNe)
	}
	for _, m := range l.Modifiers {
		out.Modifiers = append(out.Modifiers, &restaurantv1.GuestLineModifier{Name: local(m.NameEn, m.NameNe), PriceDeltaPaisa: m.PriceDelta})
	}
	return out
}

// --- request -> proto ---

func kindEnum(k string) restaurantv1.GuestRequestKind {
	switch k {
	case kindOrder:
		return restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_ORDER
	case kindCallWaiter:
		return restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_CALL_WAITER
	case kindBill:
		return restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_BILL
	}
	return restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_UNSPECIFIED
}

func statusEnum(s string) restaurantv1.GuestRequestStatus {
	switch s {
	case statusPending:
		return restaurantv1.GuestRequestStatus_GUEST_REQUEST_STATUS_PENDING
	case statusAccepted:
		return restaurantv1.GuestRequestStatus_GUEST_REQUEST_STATUS_ACCEPTED
	case statusRejected:
		return restaurantv1.GuestRequestStatus_GUEST_REQUEST_STATUS_REJECTED
	case statusDone:
		return restaurantv1.GuestRequestStatus_GUEST_REQUEST_STATUS_DONE
	}
	return restaurantv1.GuestRequestStatus_GUEST_REQUEST_STATUS_UNSPECIFIED
}

func unixOpt(t pgtype.Timestamptz) int64 {
	if !t.Valid {
		return 0
	}
	return t.Time.Unix()
}

// toView: what a guest may see of a request (no staff names or ids).
func toView(r sqlc.GuestRequest) *restaurantv1.GuestRequestView {
	v := &restaurantv1.GuestRequestView{
		Id: r.ID.String(), Kind: kindEnum(r.Kind), Status: statusEnum(r.Status), CreatedAtUnix: r.CreatedAt.Unix(),
		HandledAtUnix: unixOpt(r.HandledAt), RejectReason: r.RejectReason.String, RequestId: r.RequestID,
		Lines: []*restaurantv1.GuestLine{},
	}
	for _, l := range decodeLines(r.Lines) {
		v.Lines = append(v.Lines, toGuestLine(l))
	}
	return v
}

func toRequest(r sqlc.ListGuestRequestsRow) *restaurantv1.GuestRequest {
	out := &restaurantv1.GuestRequest{
		Id: r.ID.String(), TableId: r.TableID.String(), TableName: r.TableName, Kind: kindEnum(r.Kind), Status: statusEnum(r.Status),
		CreatedAtUnix: r.CreatedAt.Unix(), HandledByName: r.HandledByName, HandledAtUnix: unixOpt(r.HandledAt),
		RejectReason: r.RejectReason.String, RequestId: r.RequestID, Lines: []*restaurantv1.GuestLine{},
	}
	if r.HandledByUserID.Valid {
		out.HandledById = uuid.UUID(r.HandledByUserID.Bytes).String()
	}
	for _, l := range decodeLines(r.Lines) {
		out.Lines = append(out.Lines, toGuestLine(l))
	}
	return out
}

func toConfig(t sqlc.DiningTable) *restaurantv1.GuestTableConfig {
	return &restaurantv1.GuestTableConfig{
		TableId: t.ID.String(), AreaId: t.AreaID.String(), Name: t.Name, Active: t.Active, Code: t.GuestCode, Ordering: t.GuestOrdering,
	}
}

// --- rate limiting (in process: one deployment = one process) ---

type limiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{hits: map[string][]time.Time{}} }

// allow records a hit for every key if each is under its max in the last minute;
// otherwise it records nothing and returns false.
func (l *limiter) allow(now time.Time, keys map[string]int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	since := now.Add(-time.Minute)
	if len(l.hits) > 10000 { // forget idle keys now and then
		for k, hs := range l.hits {
			if len(hs) == 0 || !hs[len(hs)-1].After(since) {
				delete(l.hits, k)
			}
		}
	}
	for k, max := range keys {
		hs := l.hits[k]
		i := 0
		for i < len(hs) && !hs[i].After(since) {
			i++
		}
		hs = hs[i:]
		l.hits[k] = hs
		if len(hs) >= max {
			return false
		}
	}
	for k := range keys {
		l.hits[k] = append(l.hits[k], now)
	}
	return true
}

// limit checks the per-table and (when known) per-client-address limits.
func (s *Service) limit(what string, tableID uuid.UUID, client string, perTable, perClient int) error {
	keys := map[string]int{what + ":t:" + tableID.String(): perTable}
	if client != "" {
		keys[what+":c:"+client] = perClient
	}
	if !s.limiter.allow(s.now(), keys) {
		return rateLimited()
	}
	return nil
}
