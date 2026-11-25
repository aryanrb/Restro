package guest

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/catalog"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/money"
	"github.com/oneaccord/restro/api/internal/orders"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// Public GuestService: nothing here reads the session. Every call is identified
// only by the table code, and touches only that table's guest requests (plus the
// bill_requested_at flag of its open orders).

// Menu: restaurant and table name, whether ordering is on, and the public menu.
// Readable while ordering is off (view-only menu).
func (s *Service) Menu(ctx context.Context, code string) (*restaurantv1.GuestMenu, error) {
	t, err := tableByCode(ctx, s.q, code)
	if err != nil {
		return nil, err
	}
	on, st, err := orderingOn(ctx, s.q, t)
	if err != nil {
		return nil, err
	}
	menu, err := catalog.PublicMenu(ctx, s.q)
	if err != nil {
		return nil, err
	}
	return &restaurantv1.GuestMenu{RestaurantName: local(st.NameEn, st.NameNe), TableName: t.Name, Ordering: on, Menu: menu}, nil
}

// SubmitOrder records a PENDING guest order priced by the server. Idempotent on
// (table, request_id). client is the caller's address for the per-client limit
// ("" = unknown).
func (s *Service) SubmitOrder(ctx context.Context, req *restaurantv1.SubmitGuestOrderRequest, client string) (*restaurantv1.GuestRequestView, error) {
	t, err := tableByCode(ctx, s.q, req.GetTableCode())
	if err != nil {
		return nil, err
	}
	on, _, err := orderingOn(ctx, s.q, t)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, orderingOff()
	}
	requestID := strings.TrimSpace(req.GetRequestId())
	if requestID == "" || len(requestID) > 64 {
		return nil, connecterr.Invalid("request_id", "request_id is required")
	}
	if done, e := s.q.GetGuestOrderByRequestID(ctx, sqlc.GetGuestOrderByRequestIDParams{TableID: t.ID, RequestID: requestID}); e == nil {
		return toView(done), nil
	} else if !errors.Is(e, pgx.ErrNoRows) {
		return nil, connecterr.Internal(e)
	}
	if n := len(req.GetLines()); n == 0 || n > MaxLines {
		return nil, connecterr.Invalid("lines", "1 to 30 lines")
	}
	lines := make([]storedLine, 0, len(req.GetLines()))
	for _, in := range req.GetLines() {
		l, e := s.priceGuestLine(ctx, in)
		if e != nil {
			return nil, e
		}
		lines = append(lines, l)
	}

	var out *restaurantv1.GuestRequestView
	var created *sqlc.GuestRequest
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if _, e := q.LockGuestTable(ctx, t.ID); e != nil {
			return connecterr.Internal(e)
		}
		pending, e := q.CountPendingGuestOrders(ctx, t.ID)
		if e != nil {
			return connecterr.Internal(e)
		}
		if pending >= MaxPending {
			return failed(restaurantv1.ErrorReason_ERROR_REASON_TOO_MANY_PENDING, "", "this table already has orders waiting for staff")
		}
		if e := s.limit("order", t.ID, client, OrdersPerMinute, ClientOrdersPerMinute); e != nil {
			return e
		}
		r, e := q.CreateGuestRequest(ctx, sqlc.CreateGuestRequestParams{TableID: t.ID, Kind: kindOrder, RequestID: requestID, Lines: encodeLines(lines)})
		if errors.Is(e, pgx.ErrNoRows) { // the same request_id raced in
			r, e = q.GetGuestOrderByRequestID(ctx, sqlc.GetGuestOrderByRequestIDParams{TableID: t.ID, RequestID: requestID})
			if e != nil {
				return connecterr.Internal(e)
			}
			out = toView(r)
			return nil
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		created = &r
		out = toView(r)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if created != nil {
		s.emit(requestEvents(*created)...)
	}
	return out, nil
}

// priceGuestLine validates one cart line and prices it with the orders rules.
func (s *Service) priceGuestLine(ctx context.Context, in *restaurantv1.GuestLineInput) (storedLine, error) {
	if in.GetQuantity() < 1 || in.GetQuantity() > MaxQuantity {
		return storedLine{}, connecterr.Invalid("quantity", "quantity must be 1 to 20")
	}
	note := strings.TrimSpace(in.GetNote())
	if utf8.RuneCountInString(note) > MaxNote {
		return storedLine{}, connecterr.Invalid("note", "note is too long")
	}
	ni := &restaurantv1.NewItem{
		ClientId: uuid.NewString(), MenuItemId: in.GetMenuItemId(), VariantId: in.VariantId,
		ModifierIds: in.GetModifierIds(), Quantity: in.GetQuantity(), Note: note,
	}
	pl, rej, err := orders.PriceLine(ctx, s.q, ni)
	if err != nil {
		return storedLine{}, err
	}
	if rej != nil {
		if rej.Reason == restaurantv1.ErrorReason_ERROR_REASON_ITEM_86D {
			return storedLine{}, item86d(in.GetMenuItemId(), rej.Message)
		}
		return storedLine{}, connecterr.Invalid("lines", rej.Message)
	}
	l := storedLine{
		LineID: uuid.NewString(), MenuItemID: pl.Item.ID.String(), ModifierIDs: []string{}, Quantity: in.GetQuantity(), Note: note,
		NameEn: pl.Item.NameEn, NameNe: pl.Item.NameNe, Modifiers: []lineMod{}, UnitPrice: pl.UnitPrice,
	}
	if pl.Variant != nil {
		l.VariantID, l.HasVariant = pl.Variant.ID.String(), true
		l.VariantNameEn, l.VariantNameNe = pl.Variant.NameEn, pl.Variant.NameNe
	}
	for _, m := range pl.Modifiers {
		l.ModifierIDs = append(l.ModifierIDs, m.ID.String())
		l.Modifiers = append(l.Modifiers, lineMod{NameEn: m.NameEn, NameNe: m.NameNe, PriceDelta: m.PriceDeltaPaisa})
	}
	return l, nil
}

// Call opens a Call waiter / Ask for bill alert, or returns the one already open.
// Ask for bill also flags the table's open orders (bill_requested_at).
func (s *Service) Call(ctx context.Context, req *restaurantv1.CallStaffRequest, client string) (*restaurantv1.GuestRequestView, error) {
	t, err := tableByCode(ctx, s.q, req.GetTableCode())
	if err != nil {
		return nil, err
	}
	on, _, err := orderingOn(ctx, s.q, t)
	if err != nil {
		return nil, err
	}
	if !on {
		return nil, orderingOff()
	}
	var kind string
	switch req.GetKind() {
	case restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_CALL_WAITER:
		kind = kindCallWaiter
	case restaurantv1.GuestRequestKind_GUEST_REQUEST_KIND_BILL:
		kind = kindBill
	default:
		return nil, connecterr.Invalid("kind", "CALL_WAITER or BILL")
	}
	if err := s.limit("call", t.ID, client, CallsPerMinute, ClientCallsPerMinute); err != nil {
		return nil, err
	}
	var out *restaurantv1.GuestRequestView
	var events []realtime.Event
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		r, e := q.CreateGuestRequest(ctx, sqlc.CreateGuestRequestParams{TableID: t.ID, Kind: kind, RequestID: "", Lines: encodeLines(nil)})
		if errors.Is(e, pgx.ErrNoRows) { // one already open: tapping again does nothing new
			r, e = q.GetOpenGuestAlert(ctx, sqlc.GetOpenGuestAlertParams{TableID: t.ID, Kind: kind})
			if e != nil {
				return connecterr.Internal(e)
			}
			out = toView(r)
			return nil
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		out = toView(r)
		events = requestEvents(r)
		if kind == kindBill {
			ids, e := q.MarkBillRequested(ctx, dbx.UUID(t.ID))
			if e != nil {
				return connecterr.Internal(e)
			}
			for _, id := range ids {
				events = append(events, realtime.Event{Type: "order.updated", ID: id.String()})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.emit(events...)
	return out, nil
}

// Status: the table's open orders (sent lines, voids left out) with the running
// total as the bill will show it, and this table's guest requests.
func (s *Service) Status(ctx context.Context, code string) (*restaurantv1.GuestStatus, error) {
	t, err := tableByCode(ctx, s.q, code)
	if err != nil {
		return nil, err
	}
	on, st, err := orderingOn(ctx, s.q, t)
	if err != nil {
		return nil, err
	}
	open, err := s.q.OpenOrdersOfTable(ctx, dbx.UUID(t.ID))
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.GuestStatus{TableName: t.Name, Ordering: on, Items: []*restaurantv1.GuestStatusItem{}, Requests: []*restaurantv1.GuestRequestView{}}
	var subtotal int64
	for _, o := range open {
		ord, e := orders.AssembleOrder(ctx, s.q, o.ID)
		if e != nil {
			return nil, e
		}
		for _, it := range ord.Items {
			if it.Status == restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_VOIDED || it.Status == restaurantv1.OrderItemStatus_ORDER_ITEM_STATUS_DRAFT {
				continue
			}
			gi := &restaurantv1.GuestStatusItem{Name: it.Name, VariantName: it.VariantName, Quantity: it.Quantity, UnitPricePaisa: it.UnitPricePaisa, Status: it.Status, Modifiers: []*restaurantv1.GuestStatusModifier{}}
			for _, m := range it.Modifiers {
				gi.Modifiers = append(gi.Modifiers, &restaurantv1.GuestStatusModifier{Name: m.Name})
			}
			out.Items = append(out.Items, gi)
			subtotal += it.UnitPricePaisa * int64(it.Quantity)
		}
	}
	if subtotal > 0 {
		out.TotalPaisa = money.Compute(money.Input{
			Subtotal: subtotal, ServiceChargeEnabled: st.ServiceChargeEnabled, ServiceChargeRateBp: st.ServiceChargeRateBp,
			VatRegistered: st.VatRegistered, PricesIncludeVat: st.PricesIncludeVat, VatRateBp: st.VatRateBp, RoundTotalToRupee: st.RoundTotalToRupee,
		}).Total
	}
	// This seating: since the oldest open order, else the last 15 minutes.
	since := s.now().Add(-recent)
	if len(open) > 0 {
		since = open[0].CreatedAt
	}
	reqs, err := s.q.ListTableGuestRequests(ctx, sqlc.ListTableGuestRequestsParams{TableID: t.ID, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	for _, r := range reqs {
		out.Requests = append(out.Requests, toView(r))
	}
	return out, nil
}
