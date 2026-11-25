package guest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	restaurantv1 "github.com/oneaccord/restro/api/gen/restaurant/v1"
	"github.com/oneaccord/restro/api/internal/audit"
	"github.com/oneaccord/restro/api/internal/auth"
	"github.com/oneaccord/restro/api/internal/connecterr"
	"github.com/oneaccord/restro/api/internal/db/sqlc"
	"github.com/oneaccord/restro/api/internal/dbx"
	"github.com/oneaccord/restro/api/internal/orders"
	"github.com/oneaccord/restro/api/internal/realtime"
)

// Staff GuestDeskService.

// List: pending requests and open alerts, plus those handled in the last 15 minutes.
func (s *Service) List(ctx context.Context) (*restaurantv1.ListGuestRequestsResponse, error) {
	if _, err := requireDesk(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListGuestRequests(ctx, pgtype.Timestamptz{Time: s.now().Add(-recent), Valid: true})
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.ListGuestRequestsResponse{Requests: make([]*restaurantv1.GuestRequest, 0, len(rows))}
	for _, r := range rows {
		out.Requests = append(out.Requests, toRequest(r))
	}
	return out, nil
}

// lockPending locks a guest request for a decision: NOT_FOUND, or ALREADY_HANDLED
// (message = who handled it) when it is no longer pending.
func lockPending(ctx context.Context, q *sqlc.Queries, id string) (sqlc.GuestRequest, error) {
	rid, err := parseID("request_id", id)
	if err != nil {
		return sqlc.GuestRequest{}, err
	}
	r, err := q.LockGuestRequest(ctx, rid)
	if errors.Is(err, pgx.ErrNoRows) {
		return sqlc.GuestRequest{}, connecterr.NotFound("guest request not found")
	}
	if err != nil {
		return sqlc.GuestRequest{}, connecterr.Internal(err)
	}
	if r.Status != statusPending {
		name := ""
		if r.HandledByUserID.Valid {
			name, _ = q.GetUserName(ctx, uuid.UUID(r.HandledByUserID.Bytes))
		}
		return sqlc.GuestRequest{}, alreadyHandled(name)
	}
	return r, nil
}

// Accept adds the kept lines (staff may lower quantities or drop lines) to the
// table's open order, creating one on an empty table, and sends them to the
// kitchen — all in one transaction with the decision and its audit row.
func (s *Service) Accept(ctx context.Context, req *restaurantv1.AcceptGuestOrderRequest) (*restaurantv1.Order, error) {
	actor, err := requireDesk(ctx)
	if err != nil {
		return nil, err
	}
	var order *restaurantv1.Order
	var handled sqlc.GuestRequest
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		r, e := lockPending(ctx, q, req.GetRequestId())
		if e != nil {
			return e
		}
		if r.Kind != kindOrder {
			return connecterr.Invalid("request_id", "not a guest order")
		}
		before := decodeLines(r.Lines)
		kept, e := keptLines(before, req.GetLines())
		if e != nil {
			return e
		}

		// One table, one open order: serialize accepts per table.
		if _, e := q.LockGuestTable(ctx, r.TableID); e != nil {
			return connecterr.Internal(e)
		}
		open, e := q.OpenOrdersOfTable(ctx, dbx.UUID(r.TableID))
		if e != nil {
			return connecterr.Internal(e)
		}
		var target sqlc.Order
		switch {
		case len(open) == 0 && req.BaseVersion != nil:
			return connecterr.StaleVersion("the table's order has closed; refresh")
		case len(open) > 0 && (req.BaseVersion == nil || open[0].Version != req.GetBaseVersion()):
			return connecterr.StaleVersion("the table's order changed; refresh")
		case len(open) > 0:
			target = open[0]
		default:
			target, e = q.CreateOrder(ctx, sqlc.CreateOrderParams{
				ClientID: uuid.New(), Type: "DINE_IN", TableID: dbx.UUID(r.TableID), WaiterUserID: actor.User.ID,
			})
			if e != nil {
				return connecterr.Internal(e)
			}
		}

		items := make([]*restaurantv1.NewItem, 0, len(kept))
		lineOf := map[string]string{}
		clientIDs := make([]uuid.UUID, 0, len(kept))
		for _, l := range kept {
			cid := uuid.New()
			ni := &restaurantv1.NewItem{ClientId: cid.String(), MenuItemId: l.MenuItemID, ModifierIds: l.ModifierIDs, Quantity: l.Quantity, Note: l.Note}
			if l.VariantID != "" {
				v := l.VariantID
				ni.VariantId = &v
			}
			items = append(items, ni)
			lineOf[cid.String()] = l.LineID
			clientIDs = append(clientIDs, cid)
		}
		// Re-validated against the live menu (an item 86'd since the guest sent it is
		// refused) and sent like a waiter's send: kitchen ticket + stock deduction.
		applied, rejected, e := orders.InsertLines(ctx, q, target.ID, items, "SENT")
		if e != nil {
			return e
		}
		if len(rejected) > 0 {
			rj := rejected[0]
			if rj.Reason == restaurantv1.ErrorReason_ERROR_REASON_ITEM_86D {
				return item86d(lineOf[rj.ClientId], rj.Message)
			}
			return connecterr.Invalid(lineOf[rj.ClientId], rj.Message)
		}
		if applied != len(items) {
			return connecterr.Internal(fmt.Errorf("guest accept: %d of %d lines inserted", applied, len(items)))
		}
		if e := q.MarkOrderItemsGuest(ctx, clientIDs); e != nil {
			return connecterr.Internal(e)
		}
		if _, e := q.BumpOrderVersion(ctx, target.ID); e != nil {
			return connecterr.Internal(e)
		}
		handled, e = q.HandleGuestRequest(ctx, sqlc.HandleGuestRequestParams{
			ID: r.ID, Status: statusAccepted, HandledByUserID: dbx.UUID(actor.User.ID), OrderID: dbx.UUID(target.ID), Lines: encodeLines(kept),
		})
		if e != nil {
			return connecterr.Internal(e)
		}
		var count int32
		for _, l := range kept {
			count += l.Quantity
		}
		if e := audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: ActionAccept, Entity: "guest_request", EntityID: r.ID.String(),
			Before: audit.JSON(map[string]any{"lines": before}),
			After:  audit.JSON(map[string]any{"order_id": target.ID.String(), "table_id": r.TableID.String(), "items": count, "edits": editsNote(before, kept)}),
		}); e != nil {
			return e
		}
		order, e = orders.AssembleOrder(ctx, q, target.ID)
		return e
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "ticket.created", ID: order.Id}, realtime.Event{Type: "order.updated", ID: order.Id, Version: order.Version})
	s.emit(requestEvents(handled)...)
	return order, nil
}

// keptLines applies staff edits: each kept line once, quantity 1..20 and never
// above the guest's.
func keptLines(before []storedLine, in []*restaurantv1.AcceptedGuestLine) ([]storedLine, error) {
	if len(in) == 0 {
		return nil, connecterr.Invalid("lines", "keep at least one line, or reject the order")
	}
	byID := map[string]storedLine{}
	for _, l := range before {
		byID[l.LineID] = l
	}
	seen := map[string]bool{}
	out := make([]storedLine, 0, len(in))
	for _, k := range in {
		l, ok := byID[k.GetLineId()]
		if !ok || seen[k.GetLineId()] {
			return nil, connecterr.Invalid("lines", "unknown or repeated line")
		}
		seen[k.GetLineId()] = true
		if k.GetQuantity() < 1 || k.GetQuantity() > MaxQuantity || k.GetQuantity() > l.Quantity {
			return nil, connecterr.Invalid("quantity", "quantity must be 1 to the guest's quantity")
		}
		l.Quantity = k.GetQuantity()
		out = append(out, l)
	}
	return out, nil
}

// editsNote describes staff edits for the audit log ("" when none).
func editsNote(before, after []storedLine) string {
	var notes []string
	for _, b := range before {
		found := false
		for _, a := range after {
			if a.LineID == b.LineID {
				found = true
				if a.Quantity != b.Quantity {
					notes = append(notes, fmt.Sprintf("%s %d → %d", b.NameEn, b.Quantity, a.Quantity))
				}
			}
		}
		if !found {
			notes = append(notes, b.NameEn+" removed")
		}
	}
	return strings.Join(notes, ", ")
}

// Reject a pending guest order with a reason (a preset code or free text).
func (s *Service) Reject(ctx context.Context, req *restaurantv1.RejectGuestOrderRequest) (*restaurantv1.RejectGuestOrderResponse, error) {
	actor, err := requireDesk(ctx)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(req.GetReason())
	if reason == "" || utf8.RuneCountInString(reason) > MaxReason {
		return nil, connecterr.Invalid("reason", "a reason of up to 200 characters is required")
	}
	var handled sqlc.GuestRequest
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		r, e := lockPending(ctx, q, req.GetRequestId())
		if e != nil {
			return e
		}
		if r.Kind != kindOrder {
			return connecterr.Invalid("request_id", "not a guest order")
		}
		handled, e = q.HandleGuestRequest(ctx, sqlc.HandleGuestRequestParams{
			ID: r.ID, Status: statusRejected, HandledByUserID: dbx.UUID(actor.User.ID), RejectReason: dbx.Text(reason), Lines: r.Lines,
		})
		if e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: ActionReject, Entity: "guest_request", EntityID: r.ID.String(), Reason: reason,
			After: audit.JSON(map[string]any{"table_id": r.TableID.String()}),
		})
	})
	if err != nil {
		return nil, err
	}
	s.emit(requestEvents(handled)...)
	return &restaurantv1.RejectGuestOrderResponse{}, nil
}

// Resolve marks a Call waiter / Ask for bill alert done; Ask for bill also clears
// bill_requested_at on the table's open orders.
func (s *Service) Resolve(ctx context.Context, req *restaurantv1.ResolveGuestAlertRequest) (*restaurantv1.ResolveGuestAlertResponse, error) {
	actor, err := requireDesk(ctx)
	if err != nil {
		return nil, err
	}
	var events []realtime.Event
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		r, e := lockPending(ctx, q, req.GetRequestId())
		if e != nil {
			return e
		}
		if r.Kind == kindOrder {
			return connecterr.Invalid("request_id", "accept or reject a guest order")
		}
		handled, e := q.HandleGuestRequest(ctx, sqlc.HandleGuestRequestParams{
			ID: r.ID, Status: statusDone, HandledByUserID: dbx.UUID(actor.User.ID), Lines: r.Lines,
		})
		if e != nil {
			return connecterr.Internal(e)
		}
		events = requestEvents(handled)
		if r.Kind == kindBill {
			ids, e := q.ClearTableBillRequested(ctx, dbx.UUID(r.TableID))
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
	return &restaurantv1.ResolveGuestAlertResponse{}, nil
}

// --- admin (owner/manager): the code is the table's QR ---

func (s *Service) ListTables(ctx context.Context) (*restaurantv1.ListGuestTablesResponse, error) {
	if _, err := auth.RequireManager(ctx); err != nil {
		return nil, err
	}
	rows, err := s.q.ListGuestTables(ctx)
	if err != nil {
		return nil, connecterr.Internal(err)
	}
	out := &restaurantv1.ListGuestTablesResponse{Tables: make([]*restaurantv1.GuestTableConfig, 0, len(rows))}
	for _, t := range rows {
		out.Tables = append(out.Tables, toConfig(t))
	}
	return out, nil
}

// SetEnabled is the restaurant-wide switch (Settings.guest_ordering).
func (s *Service) SetEnabled(ctx context.Context, req *restaurantv1.SetGuestOrderingEnabledRequest) (*restaurantv1.SetGuestOrderingEnabledResponse, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		if _, e := q.SetSettingsGuestOrdering(ctx, req.GetEnabled()); e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: ActionSettings, Entity: "settings", EntityID: "guest_ordering",
			After: audit.JSON(map[string]any{"guest_ordering": req.GetEnabled()}),
		})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: "settings.updated"})
	return &restaurantv1.SetGuestOrderingEnabledResponse{}, nil
}

func (s *Service) SetTableOrdering(ctx context.Context, req *restaurantv1.SetTableGuestOrderingRequest) (*restaurantv1.SetTableGuestOrderingResponse, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("table_id", req.GetTableId())
	if err != nil {
		return nil, err
	}
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		t, e := q.SetTableGuestOrdering(ctx, sqlc.SetTableGuestOrderingParams{ID: id, GuestOrdering: req.GetOrdering()})
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("table not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: ActionSettings, Entity: "dining_table", EntityID: t.ID.String(),
			After: audit.JSON(map[string]any{"table": t.Name, "guest_ordering": req.GetOrdering()}),
		})
	})
	if err != nil {
		return nil, err
	}
	s.emit(realtime.Event{Type: EventStatus, ID: id.String()})
	return &restaurantv1.SetTableGuestOrderingResponse{}, nil
}

// NewCode gives the table a new random code: its old QR stops working.
func (s *Service) NewCode(ctx context.Context, req *restaurantv1.NewGuestCodeRequest) (*restaurantv1.GuestTableConfig, error) {
	actor, err := auth.RequireManager(ctx)
	if err != nil {
		return nil, err
	}
	id, err := parseID("table_id", req.GetTableId())
	if err != nil {
		return nil, err
	}
	var out *restaurantv1.GuestTableConfig
	err = dbx.WithTx(ctx, s.pool, s.q, func(q *sqlc.Queries) error {
		t, e := q.SetTableGuestCode(ctx, sqlc.SetTableGuestCodeParams{ID: id, GuestCode: newCode()})
		if errors.Is(e, pgx.ErrNoRows) {
			return connecterr.NotFound("table not found")
		}
		if e != nil {
			return connecterr.Internal(e)
		}
		out = toConfig(t)
		return audit.Write(ctx, q, audit.Entry{
			ActorID: actor.User.ID, Action: ActionCode, Entity: "dining_table", EntityID: t.ID.String(),
			Reason: "old QR code no longer works", After: audit.JSON(map[string]any{"table": t.Name}),
		})
	})
	if err != nil {
		return nil, err
	}
	// Open guest pages on the old code refetch, get TABLE_CODE_INVALID, and their socket closes.
	s.emit(realtime.Event{Type: EventStatus, ID: id.String()})
	return out, nil
}

// ClearBillRequest is billing's hook, called in the bill's transaction: a bill
// issued on the order answers Ask for bill — it clears the order's
// bill_requested_at and marks its table's open Ask for bill alerts done (by =
// the issuer; uuid.Nil = none).
func ClearBillRequest(ctx context.Context, q *sqlc.Queries, orderID, by uuid.UUID) error {
	if err := q.ClearOrderBillRequested(ctx, orderID); err != nil {
		return connecterr.Internal(err)
	}
	if _, err := q.DoneOrderTableBillAlerts(ctx, sqlc.DoneOrderTableBillAlertsParams{OrderID: orderID, HandledByUserID: dbx.UUIDOpt(by)}); err != nil {
		return connecterr.Internal(err)
	}
	return nil
}
